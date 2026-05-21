// Package report turns raw analysis results into a human-friendly summary.
//
// The MCP tools and the CLI both call into this package so they return the
// same shape of data. The LLM that consumes the MCP response doesn't have
// to do the "okay what does this mean?" work — the report already classifies
// findings as low/medium/high risk with explanations.
//
// Design intent: a senior reviewing a PR doesn't want a 200-symbol dump.
// They want "here are the 3 things that should worry you, here's why,
// here's what to test". This package does that synthesis.
package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/yourname/blast-radius/internal/analyze"
	"github.com/yourname/blast-radius/internal/store"
	"github.com/yourname/blast-radius/internal/tests"
)

// Severity is the qualitative risk bucket of a change.
type Severity string

const (
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

// Verdict is the top-level summary of a blast analysis.
type Verdict struct {
	Severity        Severity                   `json:"severity"`
	Headline        string                     `json:"headline"`
	Reasons         []string                   `json:"reasons"`
	TopRisks        []ImpactedHighlight        `json:"top_risks"`
	RecommendTests  []tests.TestRecommendation `json:"recommend_tests"`
	TotalImpacted   int                        `json:"total_impacted"`
	DepthHistogram  map[int]int                `json:"depth_histogram"`
	InterfaceFanout bool                       `json:"interface_fanout"` // true if at least one root was an interface
}

// ImpactedHighlight is a curated, ranked entry — what the LLM actually shows.
type ImpactedHighlight struct {
	Qualified string  `json:"qualified"`
	Kind      string  `json:"kind"`
	Path      string  `json:"path"`
	Line      int     `json:"line"`
	Depth     int     `json:"depth"`
	RiskScore float64 `json:"risk_score"`
	Reason    string  `json:"reason"`
	Explain   string  `json:"explain,omitempty"` // human sentence explaining why this is risky
}

// Synthesize builds a Verdict from an impact set + the store (for risk lookups).
//
// We:
//
//  1. Cross-reference each impacted symbol with `blast_metrics` to attach
//     its risk score (computed offline by `blast metrics`).
//  2. Sort by risk score, take top 8 — that's the actionable list.
//  3. Compute a severity bucket from total count + max risk + interface fanout.
//  4. Generate a human-readable headline + reasons array.
//
// `recommend` is the list of tests already computed by the tests package
// (we don't re-derive it here; pass nil if you didn't compute tests).
func Synthesize(
	s *store.Store,
	rootIDs []int64,
	impacted []analyze.ImpactedSymbol,
	recommend []tests.TestRecommendation,
) (*Verdict, error) {
	v := &Verdict{
		TotalImpacted:  len(impacted),
		DepthHistogram: map[int]int{},
		RecommendTests: recommend,
	}

	// Did any root symbol fan out via interface implementation?
	for _, rid := range rootIDs {
		sym, err := s.GetSymbolByID(rid)
		if err == nil && sym != nil && sym.Kind == "interface" {
			v.InterfaceFanout = true
			break
		}
	}

	// Attach risk scores. If a symbol has no metric row (e.g. metrics not
	// computed yet), treat its score as 0 — the depth still ranks it.
	highlights := make([]ImpactedHighlight, 0, len(impacted))
	// Keep metrics keyed by qualified name for the explain pass after sorting.
	metricsByQual := map[string]*store.Metrics{}
	var maxRisk float64
	for _, imp := range impacted {
		v.DepthHistogram[imp.Depth]++
		score := 0.0
		m, _ := s.GetMetric(imp.SymbolID)
		if m != nil {
			score = m.RiskScore
			metricsByQual[imp.Qualified] = m
		}
		if score > maxRisk {
			maxRisk = score
		}
		highlights = append(highlights, ImpactedHighlight{
			Qualified: imp.Qualified,
			Kind:      imp.Kind,
			Path:      imp.Path,
			Line:      imp.Line,
			Depth:     imp.Depth,
			RiskScore: score,
			Reason:    imp.Reason,
		})
	}

	// Rank: highest risk first, ties broken by smallest depth (closer = riskier).
	sortHighlights(highlights)
	if len(highlights) > 8 {
		v.TopRisks = highlights[:8]
	} else {
		v.TopRisks = highlights
	}
	// Populate Explain only for the top risks — no extra DB queries needed.
	for i := range v.TopRisks {
		v.TopRisks[i].Explain = explainHighlight(v.TopRisks[i], metricsByQual[v.TopRisks[i].Qualified])
	}

	// Severity bucket.
	v.Severity = classify(len(impacted), maxRisk, v.InterfaceFanout)
	v.Headline = headline(v.Severity, len(impacted), maxRisk, v.InterfaceFanout)
	v.Reasons = reasons(len(impacted), maxRisk, v.InterfaceFanout, v.DepthHistogram)
	return v, nil
}

// explainHighlight generates a human sentence describing why a symbol appears
// in the top risks. It combines depth, metrics, and a role inferred from the
// symbol name so the LLM (and the human reading the CLI) don't have to decode
// the raw numbers themselves.
func explainHighlight(h ImpactedHighlight, m *store.Metrics) string {
	var parts []string

	// Role from name pattern (highest signal, goes first).
	if role := inferRole(h.Qualified, h.Kind); role != "" {
		parts = append(parts, role)
	}

	// Proximity to the change.
	if h.Depth == 1 {
		parts = append(parts, "direct caller")
	} else {
		parts = append(parts, fmt.Sprintf("reached in %d hops", h.Depth))
	}

	// Metrics-derived context (only when meaningful).
	if m != nil {
		if m.IsInterface {
			parts = append(parts, "interface — all implementers must be updated")
		}
		if m.TransitiveIn > 20 {
			parts = append(parts, fmt.Sprintf("%d symbols depend on it transitively", m.TransitiveIn))
		} else if m.FanIn > 3 {
			parts = append(parts, fmt.Sprintf("%d callers", m.FanIn))
		}
		if m.IsExported && !m.IsInterface {
			parts = append(parts, "exported — public API surface")
		}
	}

	if len(parts) == 0 {
		return ""
	}
	// Capitalise first letter.
	s := strings.Join(parts, "; ")
	return strings.ToUpper(s[:1]) + s[1:]
}

// inferRole classifies a symbol into a human-readable role based on its name
// and kind. Returns "" when no strong signal is found.
func inferRole(qualified, kind string) string {
	name := qualified
	if i := strings.LastIndexByte(qualified, '.'); i >= 0 {
		name = qualified[i+1:]
	}
	switch {
	case strings.HasPrefix(name, "Test") || strings.HasPrefix(name, "Benchmark") || strings.HasPrefix(name, "Fuzz"):
		return "test entry point"
	case strings.HasPrefix(name, "Handle") || strings.HasSuffix(name, "Handler"):
		return "HTTP handler"
	case name == "main":
		return "program entry point"
	case (strings.HasPrefix(name, "New") || strings.HasPrefix(name, "Make")) && kind == "func":
		return "constructor"
	case kind == "interface":
		return "interface"
	case strings.HasSuffix(name, "Middleware"):
		return "middleware"
	case strings.HasSuffix(name, "Watcher") || strings.HasSuffix(name, "Worker") || strings.HasSuffix(name, "Runner"):
		return "background worker"
	}
	return ""
}

func sortHighlights(s []ImpactedHighlight) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].RiskScore != s[j].RiskScore {
			return s[i].RiskScore > s[j].RiskScore
		}
		return s[i].Depth < s[j].Depth
	})
}

// classify turns three scalar signals into one severity bucket.
//
// Heuristics, not science. Tuned so that:
//   - changing a private helper used in one place → low
//   - changing a moderately-used exported func → medium
//   - changing a widely-used exported func or a popular interface → high
//   - changing a core interface with hundreds of impacted callers → critical
//
// Adjust freely as you observe real usage.
func classify(impactedCount int, maxRisk float64, ifaceFanout bool) Severity {
	if ifaceFanout && impactedCount > 50 {
		return SeverityCritical
	}
	if impactedCount > 200 || maxRisk > 70 {
		return SeverityHigh
	}
	if impactedCount > 30 || maxRisk > 40 {
		return SeverityMedium
	}
	return SeverityLow
}

func headline(sev Severity, count int, maxRisk float64, iface bool) string {
	switch sev {
	case SeverityCritical:
		return fmt.Sprintf(
			"Critical: %d symbols impacted, including interface implementers. Treat as a breaking change.",
			count)
	case SeverityHigh:
		return fmt.Sprintf(
			"High blast radius: %d symbols affected (max risk %.0f/100). Review carefully and run the full test suite.",
			count, maxRisk)
	case SeverityMedium:
		s := fmt.Sprintf("Moderate impact: %d symbols affected.", count)
		if iface {
			s += " Interface implementers are involved."
		}
		return s
	default:
		return fmt.Sprintf("Low impact: %d symbols affected. Targeted tests should suffice.", count)
	}
}

func reasons(count int, maxRisk float64, iface bool, depthHist map[int]int) []string {
	var r []string
	if iface {
		r = append(r, "Change touches an interface — every implementer is potentially affected.")
	}
	if d1 := depthHist[1]; d1 > 0 {
		r = append(r, fmt.Sprintf("%d direct callers (depth=1) will see this change immediately.", d1))
	}
	if maxRisk >= 70 {
		r = append(r, fmt.Sprintf("Top impacted symbol has a risk score of %.0f/100 (heavily called, exported, or in a churn-heavy file).", maxRisk))
	}
	if count > 100 {
		r = append(r, "Impact set exceeds 100 symbols — consider splitting the change into smaller PRs.")
	}
	if len(r) == 0 {
		r = append(r, "No specific risk factors detected.")
	}
	return r
}

// FormatVerdict returns a compact plain-text rendering of a Verdict.
// Used by the CLI; the MCP server emits JSON instead.
func FormatVerdict(v *Verdict) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Severity: %s\n", strings.ToUpper(string(v.Severity)))
	fmt.Fprintf(&b, "%s\n\n", v.Headline)
	for _, reason := range v.Reasons {
		fmt.Fprintf(&b, "  • %s\n", reason)
	}
	if len(v.TopRisks) > 0 {
		fmt.Fprintf(&b, "\nTop impacted symbols:\n")
		for i, h := range v.TopRisks {
			fmt.Fprintf(&b, "  %d. [risk %5.1f, depth %d] %s\n", i+1, h.RiskScore, h.Depth, h.Qualified)
			if h.Path != "" {
				fmt.Fprintf(&b, "     %s:%d\n", h.Path, h.Line)
			}
			if h.Explain != "" {
				fmt.Fprintf(&b, "     → %s\n", h.Explain)
			}
		}
	}
	if len(v.RecommendTests) > 0 {
		fmt.Fprintf(&b, "\nRecommended tests to run (closest first):\n")
		cap := 10
		if len(v.RecommendTests) < cap {
			cap = len(v.RecommendTests)
		}
		for i := 0; i < cap; i++ {
			t := v.RecommendTests[i]
			fmt.Fprintf(&b, "  %d. %s  (depth %d)\n", i+1, t.TestQualified, t.MinDepth)
		}
		if len(v.RecommendTests) > cap {
			fmt.Fprintf(&b, "  …and %d more.\n", len(v.RecommendTests)-cap)
		}
	}
	return b.String()
}
