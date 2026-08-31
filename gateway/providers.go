package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// providers.go — Phase 2 live data sources.
//
// Both are free-tier, tightly rate-limited (Alpha Vantage ~25 req/day, Marketaux ~100 req/day),
// so results are cached hard by the handlers. Every function fails soft: on any error, missing
// key, or a rate-limit response, it returns (nil, err) and the caller falls back to seed data.

// ---------- Alpha Vantage EARNINGS (EPS beat/miss history) ----------

type EarningsPoint struct {
	FiscalDateEnding  string   `json:"fiscalDateEnding"`
	ReportedDate      string   `json:"reportedDate"`
	ReportedEPS       *float64 `json:"reportedEPS"`
	EstimatedEPS      *float64 `json:"estimatedEPS"`
	Surprise          *float64 `json:"surprise"`
	SurprisePercent   *float64 `json:"surprisePercentage"`
	Beat              *bool    `json:"beat"`
}

// avRaw mirrors the subset of Alpha Vantage's EARNINGS payload we consume. Strings because AV
// returns "None"/"" for missing numerics, which don't parse as JSON numbers.
type avRaw struct {
	Symbol            string `json:"symbol"`
	QuarterlyEarnings []struct {
		FiscalDateEnding string `json:"fiscalDateEnding"`
		ReportedDate     string `json:"reportedDate"`
		ReportedEPS      string `json:"reportedEPS"`
		EstimatedEPS     string `json:"estimatedEPS"`
		Surprise         string `json:"surprise"`
		SurprisePercent  string `json:"surprisePercentage"`
	} `json:"quarterlyEarnings"`
	// rate-limit / error signals (AV returns these with HTTP 200)
	Note        string `json:"Note"`
	Information string `json:"Information"`
	ErrorMsg    string `json:"Error Message"`
}

func parseFloatPtr(s string) *float64 {
	if s == "" || s == "None" {
		return nil
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return &v
	}
	return nil
}

func (s *Server) fetchEarnings(ctx context.Context, ticker string, limit int) ([]EarningsPoint, error) {
	if s.cfg.AlphaVantageKey == "" {
		return nil, fmt.Errorf("no ALPHAVANTAGE_API_KEY")
	}
	u := fmt.Sprintf(
		"https://www.alphavantage.co/query?function=EARNINGS&symbol=%s&apikey=%s",
		url.QueryEscape(ticker), url.QueryEscape(s.cfg.AlphaVantageKey),
	)
	body, err := s.rawGet(ctx, u)
	if err != nil {
		return nil, err
	}
	var raw avRaw
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("av decode: %w", err)
	}
	if raw.Note != "" || raw.Information != "" || raw.ErrorMsg != "" {
		return nil, fmt.Errorf("av rate-limited/error: %s%s%s", raw.Note, raw.Information, raw.ErrorMsg)
	}
	if len(raw.QuarterlyEarnings) == 0 {
		return nil, fmt.Errorf("av: no quarterlyEarnings")
	}
	out := make([]EarningsPoint, 0, limit)
	for i, q := range raw.QuarterlyEarnings {
		if i >= limit {
			break
		}
		p := EarningsPoint{
			FiscalDateEnding: q.FiscalDateEnding,
			ReportedDate:     q.ReportedDate,
			ReportedEPS:      parseFloatPtr(q.ReportedEPS),
			EstimatedEPS:     parseFloatPtr(q.EstimatedEPS),
			Surprise:         parseFloatPtr(q.Surprise),
			SurprisePercent:  parseFloatPtr(q.SurprisePercent),
		}
		if p.Surprise != nil {
			b := *p.Surprise >= 0
			p.Beat = &b
		}
		out = append(out, p)
	}
	return out, nil
}

// ---------- Marketaux news (entity-tagged sentiment) ----------

type NewsItem struct {
	Headline    string  `json:"headline"`
	Source      string  `json:"source"`
	URL         string  `json:"url"`
	PublishedAt string  `json:"publishedAt"`
	Sentiment   string  `json:"sentiment"`
	Score       float64 `json:"score"`
	Note        string  `json:"note"`
}

type mxRaw struct {
	Data []struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		URL         string `json:"url"`
		Source      string `json:"source"`
		PublishedAt string `json:"published_at"`
		Entities    []struct {
			Symbol         string  `json:"symbol"`
			SentimentScore float64 `json:"sentiment_score"`
		} `json:"entities"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func sentimentLabel(score float64) string {
	switch {
	case score > 0.15:
		return "positive"
	case score < -0.15:
		return "negative"
	default:
		return "neutral"
	}
}

func (s *Server) fetchNews(ctx context.Context, ticker string, limit int) ([]NewsItem, error) {
	if s.cfg.MarketauxKey == "" {
		return nil, fmt.Errorf("no MARKETAUX_API_KEY")
	}
	u := fmt.Sprintf(
		"https://api.marketaux.com/v1/news/all?symbols=%s&filter_entities=true&language=en&limit=%d&api_token=%s",
		url.QueryEscape(ticker), limit, url.QueryEscape(s.cfg.MarketauxKey),
	)
	body, err := s.rawGet(ctx, u)
	if err != nil {
		return nil, err
	}
	var raw mxRaw
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("marketaux decode: %w", err)
	}
	if raw.Error != nil {
		return nil, fmt.Errorf("marketaux error: %s", raw.Error.Message)
	}
	out := make([]NewsItem, 0, len(raw.Data))
	for _, a := range raw.Data {
		// average sentiment across entities that match this ticker
		var sum float64
		var n int
		for _, e := range a.Entities {
			if e.Symbol == ticker {
				sum += e.SentimentScore
				n++
			}
		}
		score := 0.0
		if n > 0 {
			score = sum / float64(n)
		}
		out = append(out, NewsItem{
			Headline:    a.Title,
			Source:      a.Source,
			URL:         a.URL,
			PublishedAt: a.PublishedAt,
			Sentiment:   sentimentLabel(score),
			Score:       score,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("marketaux: no articles")
	}
	return out, nil
}

// ---------- SEC EDGAR 8-K filings (free, no key, material events land here first) ----------

// Ticker→CIK is resolved dynamically from SEC company_tickers.json (see tickers.go: cikFor), so
// 8-K filings work for any configured ticker, not just NVDA.

// sec8KItems maps common 8-K item codes to human labels, so a filing reads as more than "8-K".
var sec8KItems = map[string]string{
	"1.01": "Material Agreement", "1.05": "Cybersecurity Incident",
	"2.01": "Acquisition/Disposition", "2.02": "Results of Operations",
	"2.03": "New Financial Obligation", "3.02": "Unregistered Equity Sales",
	"5.02": "Exec/Director Change", "5.03": "Bylaw Amendment",
	"5.07": "Shareholder Vote", "7.01": "Reg FD Disclosure",
	"8.01": "Other Events", "9.01": "Financial Statements & Exhibits",
}

type secSubmissions struct {
	CIK     string `json:"cik"`
	Filings struct {
		Recent struct {
			AccessionNumber []string `json:"accessionNumber"`
			FilingDate      []string `json:"filingDate"`
			AcceptanceTime  []string `json:"acceptanceDateTime"`
			Form            []string `json:"form"`
			Items           []string `json:"items"`
			PrimaryDocument []string `json:"primaryDocument"`
		} `json:"recent"`
	} `json:"filings"`
}

func itemLabels(items string) string {
	if items == "" {
		return ""
	}
	var out []string
	for _, code := range strings.Split(items, ",") {
		code = strings.TrimSpace(code)
		if label, ok := sec8KItems[code]; ok {
			out = append(out, label)
		}
	}
	return strings.Join(out, ", ")
}

func (s *Server) fetchSECFilings(ctx context.Context, ticker string, limit int) ([]NewsItem, error) {
	cik, ok := s.cikFor(ctx, ticker)
	if !ok {
		return nil, fmt.Errorf("no CIK for %s", ticker)
	}
	u := "https://data.sec.gov/submissions/CIK" + cik + ".json"
	body, err := s.rawGetUA(ctx, u, s.cfg.SECUserAgent)
	if err != nil {
		return nil, err
	}
	var sub secSubmissions
	if err := json.Unmarshal(body, &sub); err != nil {
		return nil, fmt.Errorf("sec decode: %w", err)
	}
	r := sub.Filings.Recent
	cikInt := strings.TrimLeft(cik, "0") // URL path uses the un-padded CIK
	out := make([]NewsItem, 0, limit)
	for i := range r.Form {
		if r.Form[i] != "8-K" {
			continue
		}
		acc := ""
		if i < len(r.AccessionNumber) {
			acc = strings.ReplaceAll(r.AccessionNumber[i], "-", "")
		}
		doc := ""
		if i < len(r.PrimaryDocument) {
			doc = r.PrimaryDocument[i]
		}
		items := ""
		if i < len(r.Items) {
			items = r.Items[i]
		}
		date := ""
		if i < len(r.FilingDate) {
			date = r.FilingDate[i]
		}
		labels := itemLabels(items)
		headline := "8-K filed " + date
		if labels != "" {
			headline += " — " + labels
		}
		out = append(out, NewsItem{
			Headline:    headline,
			Source:      "SEC EDGAR",
			URL:         fmt.Sprintf("https://www.sec.gov/Archives/edgar/data/%s/%s/%s", cikInt, acc, doc),
			PublishedAt: date,
			Sentiment:   "neutral",
			Note:        "filing",
		})
		if len(out) >= limit {
			break
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("sec: no 8-K filings")
	}
	return out, nil
}

// ---------- StockTwits crowd sentiment (free, keyless, UNOFFICIAL public endpoint) ----------
//
// DESCRIPTIVE crowd color only — never a buy/sell signal (extreme bullishness can mark tops). The
// endpoint is unofficial and rate-limited: on any error (esp. 403/404/429) we return an error and
// the caller degrades to available:false. Parsed defensively — fields may be null/missing.

const sentimentNote = "crowd sentiment, not a signal"

// A browser-ish UA — StockTwits rejects blank/obvious-bot user-agents.
const stockTwitsUA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

type stRaw struct {
	Messages []struct {
		ID        int64  `json:"id"`
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		User      struct {
			Username  string `json:"username"`
			Followers int    `json:"followers"`
		} `json:"user"`
		Entities struct {
			Sentiment *struct {
				Basic string `json:"basic"`
			} `json:"sentiment"`
		} `json:"entities"`
		Likes struct {
			Total int `json:"total"`
		} `json:"likes"`
	} `json:"messages"`
}

// buzzHistory tracks recent message-count samples per ticker so we can flag a buzz spike. In-memory,
// mutex-guarded, resets on restart. One sample lands per cache-miss (~SentimentTTL apart).
var buzzHistory = &buzzTracker{hist: map[string][]int{}}

type buzzTracker struct {
	mu   sync.Mutex
	hist map[string][]int
}

// push records `buzz` for ticker and returns whether it's a spike vs the PRIOR ring (> 1.5× median).
func (b *buzzTracker) push(ticker string, buzz int) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := b.hist[ticker]
	spike := false
	if len(h) >= 3 {
		if m := medianInt(h); m > 0 && float64(buzz) > 1.5*m {
			spike = true
		}
	}
	h = append(h, buzz)
	if len(h) > 12 {
		h = h[len(h)-12:]
	}
	b.hist[ticker] = h
	return spike
}

func medianInt(v []int) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]int(nil), v...)
	sort.Ints(c)
	n := len(c)
	if n%2 == 1 {
		return float64(c[n/2])
	}
	return float64(c[n/2-1]+c[n/2]) / 2
}

func (s *Server) fetchStockTwits(ctx context.Context, ticker string) (map[string]any, error) {
	if !s.cfg.StockTwitsEnabled {
		return nil, fmt.Errorf("stocktwits disabled")
	}
	ticker = strings.ToUpper(ticker)
	u := fmt.Sprintf("%s/streams/symbol/%s.json", strings.TrimRight(s.cfg.StockTwitsBase, "/"), url.PathEscape(ticker))
	body, err := s.rawGetUA(ctx, u, stockTwitsUA) // rawGetUA errors on HTTP >= 400 (403/404/429 -> fallback)
	if err != nil {
		return nil, err
	}
	var raw stRaw
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("stocktwits decode: %w", err)
	}
	if len(raw.Messages) == 0 {
		return nil, fmt.Errorf("stocktwits: no messages")
	}

	buzz := len(raw.Messages)
	bullish, bearish := 0, 0
	recent := make([]map[string]any, 0, 6)
	for i, m := range raw.Messages {
		var sent any // nil unless the crowd tagged it
		if m.Entities.Sentiment != nil {
			switch strings.ToLower(strings.TrimSpace(m.Entities.Sentiment.Basic)) {
			case "bullish":
				bullish++
				sent = "bullish"
			case "bearish":
				bearish++
				sent = "bearish"
			}
		}
		if i < 6 {
			text := strings.TrimSpace(m.Body)
			if r := []rune(text); len(r) > 180 { // rune-safe trim
				text = strings.TrimSpace(string(r[:180])) + "…"
			}
			msg := map[string]any{
				"text":      text,
				"username":  m.User.Username,
				"sentiment": sent,
				"at":        m.CreatedAt,
			}
			// StockTwits permalink (never links out to X).
			if m.ID != 0 && m.User.Username != "" {
				msg["url"] = fmt.Sprintf("https://stocktwits.com/%s/message/%d", m.User.Username, m.ID)
			}
			recent = append(recent, msg)
		}
	}

	tagged := bullish + bearish
	var bullRatio any // nil when nothing is tagged
	if tagged > 0 {
		bullRatio = float64(bullish) / float64(tagged)
	}
	label := "mixed"
	if bullish > bearish {
		label = "bullish"
	} else if bearish > bullish {
		label = "bearish"
	}

	return map[string]any{
		"source":         "stocktwits",
		"available":      true,
		"buzz":           buzz,
		"tagged":         tagged,
		"bullish":        bullish,
		"bearish":        bearish,
		"bullRatio":      bullRatio,
		"sentimentLabel": label,
		"spike":          buzzHistory.push(ticker, buzz),
		"note":           sentimentNote,
		"asOf":           time.Now().UTC().Format(time.RFC3339),
		"messages":       recent,
	}, nil
}

// ---------- shared raw GET ----------

func (s *Server) rawGet(ctx context.Context, u string) ([]byte, error) {
	return s.rawGetUA(ctx, u, "")
}

func (s *Server) rawGetUA(ctx context.Context, u, userAgent string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if userAgent != "" {
		req.Header.Set("User-Agent", userAgent) // SEC rejects requests without a contact UA
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, truncate(body, 160))
	}
	return body, nil
}
