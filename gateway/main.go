// Command gateway is the Go API gateway for the NVDA platform.
//
// It is the single entrypoint for the React frontend. It fans out concurrently to the Python
// analysis and llm services, merges their output with seed fundamentals/roadmap data, and
// returns one Dashboard object. Standard library only — no external modules.
package main

import (
	"log"
	"net/http"
	"os"
	"time"
)

func main() {
	cfg := loadConfig()
	srv := &Server{
		cfg:   cfg,
		cache: newCache(),
		http:  &http.Client{Timeout: 130 * time.Second},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", srv.handleHealth) // liveness — 200 while the gateway can serve
	mux.HandleFunc("GET /ready", srv.handleReady)   // readiness — 503 when a required upstream is down
	mux.HandleFunc("GET /api/tickers", srv.handleTickers)
	mux.HandleFunc("GET /api/tickers/search", srv.handleTickerSearch)
	mux.HandleFunc("GET /api/quote/{ticker}", srv.handleQuote)
	mux.HandleFunc("GET /api/confluence/{ticker}", srv.handleConfluence)
	mux.HandleFunc("GET /api/predict/{ticker}", srv.handlePredict)
	mux.HandleFunc("POST /api/train/{ticker}", srv.handleTrain)
	srv.registerModelRoutes(mux)
	mux.HandleFunc("GET /api/dashboard/{ticker}", srv.handleDashboard)
	mux.HandleFunc("GET /api/fundamentals/{ticker}", srv.handleFundamentals)
	mux.HandleFunc("GET /api/catalysts/{ticker}", srv.handleCatalysts)
	mux.HandleFunc("GET /api/news/{ticker}", srv.handleNews)
	mux.HandleFunc("GET /api/earnings/{ticker}", srv.handleEarnings)
	mux.HandleFunc("GET /api/sentiment/{ticker}", srv.handleSentiment)
	mux.HandleFunc("GET /api/calendar", srv.handleCalendar)
	mux.HandleFunc("GET /api/next-earnings/{ticker}", srv.handleNextEarnings)
	// AI Market Brief. The literal /market pattern is more specific than /{ticker}, so it wins.
	mux.HandleFunc("GET /api/brief/market", srv.handleMarketBrief)
	mux.HandleFunc("GET /api/brief/{ticker}", srv.handleBrief)
	// AI assistant: situational note (cached) + grounded chat (never cached).
	mux.HandleFunc("POST /api/assistant/{ticker}", srv.handleAssistant)
	mux.HandleFunc("POST /api/chat/{ticker}", srv.handleChat)
	// Chat personas ("instructors"): user-editable custom instructions that shape the chat's style.
	mux.HandleFunc("GET /api/personas", srv.handlePersonas)
	mux.HandleFunc("POST /api/personas", srv.handlePersonas)
	mux.HandleFunc("PATCH /api/personas/{id}", srv.handlePersonaItem)
	mux.HandleFunc("DELETE /api/personas/{id}", srv.handlePersonaItem)
	// Research features (qwen reads text): transcripts, thesis checks, news digest, scenarios.
	mux.HandleFunc("POST /api/transcript/{ticker}", srv.handleTranscript)
	mux.HandleFunc("GET /api/transcript/{ticker}", srv.handleTranscriptHistory)
	mux.HandleFunc("POST /api/theses/{id}/check", srv.handleThesisCheck)
	// Thesis reads/writes live in the journal; these forward the session and pass the journal's own
	// status + error code through unchanged (see proxyJournal). CRUD stays on the journal directly.
	mux.HandleFunc("GET /api/theses/{id}", srv.handleThesisGet)
	mux.HandleFunc("GET /api/theses/{id}/versions", srv.handleThesisVersions)
	mux.HandleFunc("POST /api/theses/{id}/review-checkpoint", srv.handleThesisReviewCheckpoint)
	mux.HandleFunc("GET /api/digest/{ticker}", srv.handleDigest)
	mux.HandleFunc("POST /api/scenario/{ticker}", srv.handleScenario)
	// One bounded research job. Streams truthful progress events, then one structured report.
	mux.HandleFunc("POST /api/investigate/{ticker}", srv.handleInvestigation)
	// Analyst committee (agent pipeline). On-demand only — never polled, never in the dashboard.
	mux.HandleFunc("GET /api/committee/{ticker}", srv.handleCommittee)
	mux.HandleFunc("GET /api/committee/{ticker}/history", srv.handleCommitteeHistory)
	mux.HandleFunc("GET /api/reads/{ticker}", srv.handleReads)
	mux.HandleFunc("GET /api/reads/{ticker}/diff", srv.handleReadsDiff)
	// Evidence + thesis links. Registered from evidence.go so the routes, the journal pass-through,
	// and the server-side provenance composer all live in one file.
	srv.registerEvidenceRoutes(mux)
	// Contextual research lenses + saved reviews. Registered from lens.go for the same reason: this
	// lane's routes, ID resolution, and review persistence stay in one file.
	srv.registerLensRoutes(mux)
	// Thesis-aware monitoring: the deterministic "what changed since your last review" set and its
	// optional on-demand summary. Registered from changes.go for the same reason as the two above.
	srv.registerChangeRoutes(mux)
	// Decision records + outcome reviews. Registered from decisions.go: the journal owns the store,
	// and the only things the gateway adds — validated-signal context and a cited recap — live there.
	// Monitoring is registered first because a decision references its change items by id (§4.4).
	srv.registerDecisionRoutes(mux)
	// Wave 0 seams. Empty today; Wave 1 Lane A and Wave 2 Lane C attach registrars from their own
	// files so neither edits main.go. See routeseams.go.
	srv.registerEventRoutes(mux)
	srv.registerSubscriptionRoutes(mux)
	// Wave 3 Lane 3B: POST /api/analyst/{ticker}, the agentic analyst pipeline. On-demand only —
	// never polled, never in the dashboard (invariant #4). Unlike 3A's context envelope, which
	// self-registers onto the event seam from its own init(), this one needs an explicit call:
	// analyst.go declares registerAnalystRoutes but attaches no registrar, so without this line the
	// route is defined and unreachable.
	srv.registerAnalystRoutes(mux)
	// Price + PEAD evaluation runners and forward-estimate collector. These jobs were CLI-only, so
	// they could not run in the shell-less single-container deploy. All `run` routes are gated on the
	// EVAL_ADMIN_UIDS allow-list (empty = nobody); see
	// evaluate.go. PEAD and estimate collection remain manual. The price evaluator can also be
	// started by prediction's bounded, opt-in candidate controller; this gateway surface remains an
	// explicit operator trigger and never schedules work itself.
	srv.registerEvaluateRoutes(mux)

	handler := srv.withCORS(withLogging(mux))

	log.Printf("gateway listening on :%s (analysis=%s llm=%s)", cfg.Port, cfg.AnalysisURL, cfg.LLMURL)
	if err := http.ListenAndServe(":"+cfg.Port, handler); err != nil {
		log.Fatalf("server error: %v", err)
		os.Exit(1)
	}
}
