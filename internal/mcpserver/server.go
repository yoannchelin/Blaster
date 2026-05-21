// Package mcpserver exposes Blast Radius capabilities as MCP tools.
//
// The 5 tools we expose:
//
//	impact_of        — impact of changing a named symbol
//	impact_of_file   — impact of changing every top-level symbol in a file
//	impact_of_diff   — impact of an applied/pending git diff (the killer feature)
//	risk_score       — read the pre-computed risk row for a symbol
//	tests_to_run     — which tests should I rerun for a set of changes
//
// We deliberately keep tool inputs small and stringly-typed. The LLM is
// good at producing qualified names and diff text; it's bad at producing
// id integers.
package mcpserver

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yourname/blast-radius/internal/analyze"
	"github.com/yourname/blast-radius/internal/diff"
	"github.com/yourname/blast-radius/internal/report"
	"github.com/yourname/blast-radius/internal/store"
	"github.com/yourname/blast-radius/internal/tests"
)

// Server holds the shared state for the tool handlers.
type Server struct {
	Store    *store.Store
	RepoRoot string
}

// Register wires every tool onto the provided mcp.Server.
func (s *Server) Register(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "impact_of",
		Description: "Compute the blast radius of changing a single symbol, " +
			"identified by its qualified name (e.g. 'github.com/x/y/payment.ChargeCustomer'). " +
			"Returns a severity verdict, the top impacted symbols ranked by risk, " +
			"and recommended tests to run.",
	}, s.handleImpactOf)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "impact_of_file",
		Description: "Compute the blast radius of changing any top-level symbol in a file. " +
			"Path is repo-relative (e.g. 'internal/payment/charge.go').",
	}, s.handleImpactOfFile)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "impact_of_diff",
		Description: "Given a unified diff (output of `git diff` or a patch file), " +
			"identify which symbols are touched and compute their combined blast radius. " +
			"This is the pre-PR review tool: paste in `git diff main` and get the senior reviewer's analysis.",
	}, s.handleImpactOfDiff)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "risk_score",
		Description: "Return the pre-computed risk metrics for one symbol: " +
			"fan-in, fan-out, transitive callers, and a 0-100 risk score. " +
			"Requires `blast metrics` to have been run.",
	}, s.handleRiskScore)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "tests_to_run",
		Description: "Given a list of qualified symbols that will change, " +
			"return the tests that exercise them, closest tests first. " +
			"Requires `blast tests` to have built the test map.",
	}, s.handleTestsToRun)
}

// --- tool: impact_of ---------------------------------------------------------

type ImpactOfInput struct {
	Qualified        string `json:"qualified" jsonschema:"the qualified name of the symbol to analyse"`
	MaxDepth         int    `json:"max_depth,omitempty" jsonschema:"BFS hop limit (default 6)"`
	IncludeTests     bool   `json:"include_tests,omitempty" jsonschema:"include test functions in the impacted set"`
	RecommendTests   bool   `json:"recommend_tests,omitempty" jsonschema:"also return tests to run (requires blast tests)"`
}

func (s *Server) handleImpactOf(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in ImpactOfInput,
) (*mcp.CallToolResult, *report.Verdict, error) {
	sym, err := s.Store.LookupSymbol(in.Qualified)
	if err != nil {
		return nil, nil, err
	}
	if sym == nil {
		return nil, nil, fmt.Errorf("symbol not found: %q", in.Qualified)
	}
	opt := analyze.DefaultOptions()
	if in.MaxDepth > 0 {
		opt.MaxDepth = in.MaxDepth
	}
	opt.IncludeTests = in.IncludeTests

	rep, err := analyze.CachedImpact(ctx, s.Store, sym.ID, opt)
	if err != nil {
		return nil, nil, err
	}
	var recs []tests.TestRecommendation
	if in.RecommendTests {
		recs, err = tests.TestsForSymbols(s.Store, []int64{sym.ID})
		if err != nil {
			return nil, nil, err
		}
	}
	v, err := report.Synthesize(s.Store, []int64{sym.ID}, rep.Impacted, recs)
	return nil, v, err
}

// --- tool: impact_of_file ----------------------------------------------------

type ImpactOfFileInput struct {
	Path           string `json:"path" jsonschema:"repo-relative path to the file"`
	MaxDepth       int    `json:"max_depth,omitempty"`
	IncludeTests   bool   `json:"include_tests,omitempty"`
	RecommendTests bool   `json:"recommend_tests,omitempty"`
}

func (s *Server) handleImpactOfFile(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in ImpactOfFileInput,
) (*mcp.CallToolResult, *report.Verdict, error) {
	opt := analyze.DefaultOptions()
	if in.MaxDepth > 0 {
		opt.MaxDepth = in.MaxDepth
	}
	opt.IncludeTests = in.IncludeTests

	frep, err := analyze.ImpactOfFile(ctx, s.Store, in.Path, opt)
	if err != nil {
		return nil, nil, err
	}
	// Collect root ids (the symbols in the file).
	rootIDs := make([]int64, 0, len(frep.Symbols))
	for _, q := range frep.Symbols {
		sym, _ := s.Store.LookupSymbol(q)
		if sym != nil {
			rootIDs = append(rootIDs, sym.ID)
		}
	}
	var recs []tests.TestRecommendation
	if in.RecommendTests {
		recs, _ = tests.TestsForSymbols(s.Store, rootIDs)
	}
	v, err := report.Synthesize(s.Store, rootIDs, frep.Impacted, recs)
	return nil, v, err
}

// --- tool: impact_of_diff ----------------------------------------------------

type ImpactOfDiffInput struct {
	Diff           string `json:"diff,omitempty" jsonschema:"the raw unified diff text"`
	DiffPath       string `json:"diff_path,omitempty" jsonschema:"alternatively, a path to a .patch / .diff file"`
	MaxDepth       int    `json:"max_depth,omitempty"`
	RecommendTests bool   `json:"recommend_tests,omitempty"`
}

type DiffVerdict struct {
	Verdict       *report.Verdict        `json:"verdict"`
	FileBreakdown []diff.FileImpact      `json:"file_breakdown"`
	TouchedFiles  int                    `json:"touched_files"`
}

func (s *Server) handleImpactOfDiff(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in ImpactOfDiffInput,
) (*mcp.CallToolResult, *DiffVerdict, error) {
	var diffText string
	switch {
	case in.Diff != "":
		diffText = in.Diff
	case in.DiffPath != "":
		b, err := os.ReadFile(in.DiffPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read diff: %w", err)
		}
		diffText = string(b)
	default:
		return nil, nil, fmt.Errorf("either 'diff' (inline text) or 'diff_path' (file) is required")
	}

	touched, err := diff.Parse(strings.NewReader(diffText))
	if err != nil {
		return nil, nil, err
	}
	opt := analyze.DefaultOptions()
	if in.MaxDepth > 0 {
		opt.MaxDepth = in.MaxDepth
	}
	dr, err := diff.AnalyzeFiles(ctx, s.Store, touched, opt)
	if err != nil {
		return nil, nil, err
	}

	// Resolve root ids = every touched symbol across files.
	rootIDs := make([]int64, 0)
	for _, f := range dr.Files {
		for _, t := range f.TouchedSymbols {
			sym, _ := s.Store.LookupSymbol(t.Qualified)
			if sym != nil {
				rootIDs = append(rootIDs, sym.ID)
			}
		}
	}
	var recs []tests.TestRecommendation
	if in.RecommendTests {
		recs, _ = tests.TestsForSymbols(s.Store, rootIDs)
	}
	v, err := report.Synthesize(s.Store, rootIDs, dr.UniqueImpacted, recs)
	if err != nil {
		return nil, nil, err
	}
	return nil, &DiffVerdict{
		Verdict:       v,
		FileBreakdown: dr.Files,
		TouchedFiles:  dr.TotalFiles,
	}, nil
}

// --- tool: risk_score --------------------------------------------------------

type RiskScoreInput struct {
	Qualified string `json:"qualified"`
}

type RiskScoreOutput struct {
	Qualified    string  `json:"qualified"`
	FanIn        int     `json:"fan_in"`
	FanOut       int     `json:"fan_out"`
	TransitiveIn int     `json:"transitive_in"`
	IsExported   bool    `json:"is_exported"`
	IsInterface  bool    `json:"is_interface"`
	RiskScore    float64 `json:"risk_score"`
	Note         string  `json:"note,omitempty"`
}

func (s *Server) handleRiskScore(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in RiskScoreInput,
) (*mcp.CallToolResult, *RiskScoreOutput, error) {
	sym, err := s.Store.LookupSymbol(in.Qualified)
	if err != nil {
		return nil, nil, err
	}
	if sym == nil {
		return nil, nil, fmt.Errorf("symbol not found: %q", in.Qualified)
	}
	m, err := s.Store.GetMetric(sym.ID)
	if err != nil {
		return nil, nil, err
	}
	if m == nil {
		return nil, &RiskScoreOutput{
			Qualified: in.Qualified,
			Note:      "no metrics computed yet — run `blast metrics`",
		}, nil
	}
	return nil, &RiskScoreOutput{
		Qualified:    in.Qualified,
		FanIn:        m.FanIn,
		FanOut:       m.FanOut,
		TransitiveIn: m.TransitiveIn,
		IsExported:   m.IsExported,
		IsInterface:  m.IsInterface,
		RiskScore:    m.RiskScore,
	}, nil
}

// --- tool: tests_to_run ------------------------------------------------------

type TestsToRunInput struct {
	Qualified []string `json:"qualified" jsonschema:"list of qualified symbols that will change"`
}

type TestsToRunOutput struct {
	Tests []tests.TestRecommendation `json:"tests"`
	Note  string                     `json:"note,omitempty"`
}

func (s *Server) handleTestsToRun(
	_ context.Context,
	_ *mcp.CallToolRequest,
	in TestsToRunInput,
) (*mcp.CallToolResult, *TestsToRunOutput, error) {
	ids := make([]int64, 0, len(in.Qualified))
	for _, q := range in.Qualified {
		sym, _ := s.Store.LookupSymbol(q)
		if sym != nil {
			ids = append(ids, sym.ID)
		}
	}
	if len(ids) == 0 {
		return nil, &TestsToRunOutput{
			Note: "no matching symbols found in index",
		}, nil
	}
	recs, err := tests.TestsForSymbols(s.Store, ids)
	if err != nil {
		return nil, nil, err
	}
	out := &TestsToRunOutput{Tests: recs}
	if len(recs) == 0 {
		out.Note = "no tests exercise these symbols — either run `blast tests` first, or the symbols genuinely have no test coverage"
	}
	return nil, out, nil
}

// --- shared helper -----------------------------------------------------------

// (no shared helpers currently; placeholder kept for future hooks)
