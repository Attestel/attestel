package main

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	portfolioChangeVersion       = "portfolio-changes-v1"
	materialPositionWeightChange = 0.02
	materialHoldingWeight        = 0.05
	materialCashWeightChange     = 0.02
)

type PortfolioChange struct {
	Type         string         `json:"type"`
	Subject      string         `json:"subject"`
	Summary      string         `json:"summary"`
	Material     bool           `json:"material"`
	ImpactWeight *float64       `json:"impactWeight,omitempty"`
	Before       map[string]any `json:"before,omitempty"`
	After        map[string]any `json:"after,omitempty"`
}

type PortfolioSnapshot struct {
	ID                  string                `json:"id"`
	PortfolioID         string                `json:"portfolioId"`
	ContextVersion      string                `json:"contextVersion"`
	ChangePolicyVersion string                `json:"changePolicyVersion"`
	Intelligence        PortfolioIntelligence `json:"intelligence"`
	Changes             []PortfolioChange     `json:"changes"`
	MaterialChangeCount int                   `json:"materialChangeCount"`
	CreatedAt           int64                 `json:"createdAt"`
}

func weightDelta(a, b *float64) float64 {
	if a == nil || b == nil {
		return 0
	}
	delta := *b - *a
	if delta < 0 {
		return -delta
	}
	return delta
}

func positionMap(intelligence PortfolioIntelligence) map[string]PortfolioPositionIntelligence {
	out := map[string]PortfolioPositionIntelligence{}
	for _, position := range intelligence.Positions {
		out[position.Ticker] = position
	}
	return out
}

func targetMap(intelligence PortfolioIntelligence) map[string]PortfolioTargetStatus {
	out := map[string]PortfolioTargetStatus{}
	for _, target := range intelligence.Targets {
		out[target.Kind+":"+strings.ToLower(target.Key)] = target
	}
	return out
}

func findingMap(intelligence PortfolioIntelligence) map[string]PortfolioFinding {
	out := map[string]PortfolioFinding{}
	for _, finding := range intelligence.Findings {
		out[finding.Code+":"+strings.ToLower(finding.Subject)] = finding
	}
	return out
}

func intPointersEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func int64PointersEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func diffPortfolioIntelligence(before, after PortfolioIntelligence) []PortfolioChange {
	changes := []PortfolioChange{}
	oldPositions, newPositions := positionMap(before), positionMap(after)
	tickers := map[string]bool{}
	for ticker := range oldPositions {
		tickers[ticker] = true
	}
	for ticker := range newPositions {
		tickers[ticker] = true
	}
	orderedTickers := make([]string, 0, len(tickers))
	for ticker := range tickers {
		orderedTickers = append(orderedTickers, ticker)
	}
	sort.Strings(orderedTickers)
	for _, ticker := range orderedTickers {
		oldPosition, hadOld := oldPositions[ticker]
		newPosition, hasNew := newPositions[ticker]
		switch {
		case !hadOld && hasNew:
			changes = append(changes, PortfolioChange{
				Type: "position_added", Subject: ticker, Summary: "Position was added to the portfolio.",
				Material:     newPosition.Weight != nil && *newPosition.Weight >= materialHoldingWeight,
				ImpactWeight: newPosition.Weight, After: map[string]any{"weight": newPosition.Weight},
			})
			continue
		case hadOld && !hasNew:
			changes = append(changes, PortfolioChange{
				Type: "position_removed", Subject: ticker, Summary: "Position was removed from the portfolio.",
				Material:     oldPosition.Weight != nil && *oldPosition.Weight >= materialHoldingWeight,
				ImpactWeight: oldPosition.Weight, Before: map[string]any{"weight": oldPosition.Weight},
			})
			continue
		}
		if oldPosition.Weight != nil && newPosition.Weight != nil && weightDelta(oldPosition.Weight, newPosition.Weight) >= materialPositionWeightChange {
			changes = append(changes, PortfolioChange{
				Type: "position_weight", Subject: ticker, Summary: "Position weight changed materially.",
				Material: true, ImpactWeight: newPosition.Weight,
				Before: map[string]any{"weight": oldPosition.Weight}, After: map[string]any{"weight": newPosition.Weight},
			})
		}
		oldThesis, newThesis := oldPosition.Thesis, newPosition.Thesis
		if oldThesis == nil && newThesis != nil {
			changes = append(changes, PortfolioChange{
				Type: "thesis_attached", Subject: ticker, Summary: "An active ticker thesis is now attached.",
				Material: false, ImpactWeight: newPosition.Weight,
				After: map[string]any{"thesisId": newThesis.ID, "updatedAt": newThesis.UpdatedAt},
			})
		} else if oldThesis != nil && newThesis == nil {
			changes = append(changes, PortfolioChange{
				Type: "thesis_detached", Subject: ticker, Summary: "The previously attached active thesis is no longer active.",
				Material:     newPosition.Weight != nil && *newPosition.Weight >= materialHoldingWeight,
				ImpactWeight: newPosition.Weight, Before: map[string]any{"thesisId": oldThesis.ID},
			})
		} else if oldThesis != nil && newThesis != nil {
			if oldThesis.ID != newThesis.ID || oldThesis.UpdatedAt != newThesis.UpdatedAt {
				changes = append(changes, PortfolioChange{
					Type: "thesis_updated", Subject: ticker, Summary: "The attached ticker thesis changed.",
					Material:     newPosition.Weight != nil && *newPosition.Weight >= materialHoldingWeight,
					ImpactWeight: newPosition.Weight,
					Before:       map[string]any{"thesisId": oldThesis.ID, "updatedAt": oldThesis.UpdatedAt},
					After:        map[string]any{"thesisId": newThesis.ID, "updatedAt": newThesis.UpdatedAt},
				})
			}
			if oldThesis.LatestCheckVerdict != newThesis.LatestCheckVerdict ||
				!intPointersEqual(oldThesis.LatestCheckConfidence, newThesis.LatestCheckConfidence) ||
				!int64PointersEqual(oldThesis.LatestCheckAt, newThesis.LatestCheckAt) {
				changes = append(changes, PortfolioChange{
					Type: "thesis_check", Subject: ticker, Summary: "The latest thesis check changed.",
					Material:     newPosition.Weight != nil && *newPosition.Weight >= materialHoldingWeight,
					ImpactWeight: newPosition.Weight,
					Before:       map[string]any{"verdict": oldThesis.LatestCheckVerdict, "confidence": oldThesis.LatestCheckConfidence},
					After:        map[string]any{"verdict": newThesis.LatestCheckVerdict, "confidence": newThesis.LatestCheckConfidence},
				})
			}
		}
	}

	if before.CashWeight != nil && after.CashWeight != nil && weightDelta(before.CashWeight, after.CashWeight) >= materialCashWeightChange {
		changes = append(changes, PortfolioChange{
			Type: "cash_weight", Subject: "CASH", Summary: "Cash weight changed materially.", Material: true,
			ImpactWeight: after.CashWeight,
			Before:       map[string]any{"weight": before.CashWeight}, After: map[string]any{"weight": after.CashWeight},
		})
	}
	if before.Concentration.LargestTicker != after.Concentration.LargestTicker ||
		weightDelta(before.Concentration.LargestWeight, after.Concentration.LargestWeight) >= materialPositionWeightChange {
		changes = append(changes, PortfolioChange{
			Type: "concentration", Subject: after.Concentration.LargestTicker,
			Summary: "Largest-position concentration changed materially.", Material: true,
			ImpactWeight: after.Concentration.LargestWeight,
			Before:       map[string]any{"ticker": before.Concentration.LargestTicker, "weight": before.Concentration.LargestWeight},
			After:        map[string]any{"ticker": after.Concentration.LargestTicker, "weight": after.Concentration.LargestWeight},
		})
	}

	oldTargets, newTargets := targetMap(before), targetMap(after)
	for key, current := range newTargets {
		prior, ok := oldTargets[key]
		if !ok || prior.State == current.State {
			continue
		}
		outside := current.State == "above_range" || current.State == "below_range"
		changes = append(changes, PortfolioChange{
			Type: "target_range", Subject: current.Key, Summary: "Target-range status changed to " + strings.ReplaceAll(current.State, "_", " ") + ".",
			Material: outside, ImpactWeight: current.CurrentWeight,
			Before: map[string]any{"state": prior.State, "weight": prior.CurrentWeight},
			After:  map[string]any{"state": current.State, "weight": current.CurrentWeight},
		})
	}

	oldFindings, newFindings := findingMap(before), findingMap(after)
	for key, finding := range newFindings {
		if _, ok := oldFindings[key]; !ok {
			changes = append(changes, PortfolioChange{
				Type: "finding_opened", Subject: finding.Subject, Summary: finding.Summary,
				Material: finding.Severity == "attention", After: finding.Evidence,
			})
		}
	}
	for key, finding := range oldFindings {
		if _, ok := newFindings[key]; !ok {
			changes = append(changes, PortfolioChange{
				Type: "finding_resolved", Subject: finding.Subject, Summary: "A prior portfolio finding is no longer present.",
				Material: false, Before: finding.Evidence,
			})
		}
	}

	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Material != changes[j].Material {
			return changes[i].Material
		}
		if changes[i].Type != changes[j].Type {
			return changes[i].Type < changes[j].Type
		}
		return changes[i].Subject < changes[j].Subject
	})
	return changes
}

func (s *Server) handleCreatePortfolioSnapshot(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	portfolio, ok, err := s.portfolios.Get(uid, r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio store is unreadable"})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "portfolio not found"})
		return
	}
	intel, err := s.buildPortfolioIntelligence(r.Context(), uid, portfolio)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio intelligence is unavailable"})
		return
	}
	previous, hasPrevious, err := s.portfolioSnapshots.Latest(uid, portfolio.ID)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio snapshot store is unreadable"})
		return
	}
	if hasPrevious && previous.ContextVersion == intel.ContextVersion {
		writeJSON(w, http.StatusOK, map[string]any{"snapshot": previous, "reused": true})
		return
	}
	changes := []PortfolioChange{}
	if hasPrevious {
		changes = diffPortfolioIntelligence(previous.Intelligence, intel)
	}
	material := 0
	for _, change := range changes {
		if change.Material {
			material++
		}
	}
	now := time.Now().Unix()
	snapshot := PortfolioSnapshot{
		ID: "ps_" + newID(), PortfolioID: portfolio.ID, ContextVersion: intel.ContextVersion,
		ChangePolicyVersion: portfolioChangeVersion, Intelligence: intel, Changes: changes,
		MaterialChangeCount: material, CreatedAt: now,
	}
	snapshot, err = s.portfolioSnapshots.Add(uid, snapshot)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio snapshot could not be persisted"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"snapshot": snapshot, "reused": false})
}

func (s *Server) handleListPortfolioSnapshots(w http.ResponseWriter, r *http.Request) {
	uid := userID(r)
	portfolioID := r.PathValue("id")
	if _, ok, err := s.portfolios.Get(uid, portfolioID); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio store is unreadable"})
		return
	} else if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "portfolio not found"})
		return
	}
	limit := 20
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 1 && n <= 100 {
			limit = n
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "limit must be between 1 and 100"})
			return
		}
	}
	items, err := s.portfolioSnapshots.List(uid, portfolioID, limit)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "portfolio snapshot store is unreadable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"snapshots": items})
}
