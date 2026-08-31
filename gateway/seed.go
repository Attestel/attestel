package main

import (
	_ "embed"
	"encoding/json"
)

// seedJSON is the verified NVIDIA reference data, compiled into the binary so the platform
// renders real fundamentals/roadmap/news with no API keys. Source: docs/NVIDIA_RESEARCH.md.
//
//go:embed seed/nvda.json
var seedJSON []byte

var seedData map[string]any

func init() {
	if err := json.Unmarshal(seedJSON, &seedData); err != nil {
		panic("invalid embedded seed/nvda.json: " + err.Error())
	}
}

// seedNews synthesizes a small news feed from the seeded catalysts so the news panel is
// populated in v1. Phase 2 replaces this with a live Marketaux / SEC 8-K feed.
func seedNews() []map[string]any {
	catalysts, _ := seedData["catalysts"].([]any)
	news := make([]map[string]any, 0, len(catalysts))
	for _, c := range catalysts {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		sentiment := "neutral"
		if imp, _ := m["impact"].(string); imp == "high" {
			sentiment = "positive"
		}
		news = append(news, map[string]any{
			"headline":    m["title"],
			"source":      "NVIDIA / filings (seed)",
			"url":         "https://nvidianews.nvidia.com",
			"publishedAt": m["date"],
			"category":    m["type"],
			"sentiment":   sentiment,
			"note":        m["note"],
		})
	}
	return news
}
