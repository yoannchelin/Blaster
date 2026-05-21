// Command blast is the Blast Radius CLI.
//
// Subcommands:
//
//	blast info                  Print state of the blast tables for a repo
//	blast metrics               Precompute per-symbol risk scores
//	blast tests                 Build the test→prod symbol map
//	blast impact   <qualified>  Impact analysis for one symbol
//	blast file     <path>       Impact analysis for one file
//	blast diff     [path]       Impact analysis for a diff (file or stdin)
//
// All subcommands accept --repo (default: current directory).
// The repo must have been indexed first with `archaeo index`.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/yourname/blast-radius/internal/analyze"
	"github.com/yourname/blast-radius/internal/diff"
	"github.com/yourname/blast-radius/internal/report"
	"github.com/yourname/blast-radius/internal/risk"
	"github.com/yourname/blast-radius/internal/store"
	"github.com/yourname/blast-radius/internal/tests"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]
	switch cmd {
	case "info":
		runInfo(args)
	case "metrics":
		runMetrics(args)
	case "tests":
		runTests(args)
	case "impact":
		runImpact(args)
	case "file":
		runFile(args)
	case "diff":
		runDiff(args)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `blast — Blast Radius CLI

Subcommands:
  info       Show blast state for a repo
  metrics    Precompute per-symbol risk scores
  tests      Build test → prod symbol map
  impact     Impact of changing a named symbol
  file       Impact of changing any symbol in a file
  diff       Impact of a unified diff (path or stdin)

Run "blast <subcommand> -h" for flags.

The repo must be indexed first with: archaeo index --repo <path>
`)
}

func runInfo(args []string) {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	repo := fs.String("repo", ".", "path to the repo root")
	_ = fs.Parse(args)

	s := mustOpen(*repo)
	defer s.Close()

	var nMetrics, nMappings, nCache int
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM blast_metrics`).Scan(&nMetrics)
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM blast_test_map`).Scan(&nMappings)
	_ = s.DB().QueryRow(`SELECT COUNT(*) FROM blast_impact_cache`).Scan(&nCache)

	last, _ := s.LastIndexedAt()
	seen, _, _ := s.GetBlastMeta("seen_index")

	fmt.Printf("DB: %s\n", s.Path())
	fmt.Printf("Archaeologist last_index: %s\n", or(last, "never"))
	fmt.Printf("Blast seen_index:         %s\n", or(seen, "never"))
	fmt.Printf("Metrics rows: %d\n", nMetrics)
	fmt.Printf("Test mappings: %d\n", nMappings)
	fmt.Printf("Impact cache entries: %d\n", nCache)
}

func runMetrics(args []string) {
	fs := flag.NewFlagSet("metrics", flag.ExitOnError)
	repo := fs.String("repo", ".", "path to the repo root")
	_ = fs.Parse(args)
	s := mustOpen(*repo)
	defer s.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	err := risk.Compute(ctx, s, risk.DefaultWeights(), func(done, total int) {
		fmt.Fprintf(os.Stderr, "\r[metrics] %d/%d ", done, total)
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		die("metrics: %v", err)
	}
	if up, _ := s.LastIndexedAt(); up != "" {
		_ = s.SetBlastMeta("seen_index", up)
	}
	fmt.Println("metrics computed.")
}

func runTests(args []string) {
	fs := flag.NewFlagSet("tests", flag.ExitOnError)
	repo := fs.String("repo", ".", "path to the repo root")
	depth := fs.Int("depth", 6, "BFS depth limit")
	_ = fs.Parse(args)
	s := mustOpen(*repo)
	defer s.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	n, err := tests.BuildMap(ctx, s, tests.Options{MaxDepth: *depth}, func(done, total int) {
		fmt.Fprintf(os.Stderr, "\r[tests] %d/%d ", done, total)
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		die("tests: %v", err)
	}
	if up, _ := s.LastIndexedAt(); up != "" {
		_ = s.SetBlastMeta("seen_index", up)
	}
	fmt.Printf("%d test→prod mappings written.\n", n)
}

func runImpact(args []string) {
	fs := flag.NewFlagSet("impact", flag.ExitOnError)
	repo := fs.String("repo", ".", "path to the repo root")
	depth := fs.Int("depth", 6, "BFS depth")
	withTests := fs.Bool("include-tests", false, "include test functions in the impacted set")
	rec := fs.Bool("recommend-tests", false, "also recommend tests to run")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		die("usage: blast impact [flags] <qualified-name>")
	}
	q := fs.Arg(0)
	s := mustOpen(*repo)
	defer s.Close()

	sym, err := s.LookupSymbol(q)
	if err != nil || sym == nil {
		die("symbol not found: %q", q)
	}
	opt := analyze.DefaultOptions()
	opt.MaxDepth = *depth
	opt.IncludeTests = *withTests

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	rep, err := analyze.Impact(ctx, s, sym.ID, opt)
	if err != nil {
		die("impact: %v", err)
	}
	var recs []tests.TestRecommendation
	if *rec {
		recs, _ = tests.TestsForSymbols(s, []int64{sym.ID})
	}
	v, err := report.Synthesize(s, []int64{sym.ID}, rep.Impacted, recs)
	if err != nil {
		die("synthesize: %v", err)
	}
	fmt.Println(report.FormatVerdict(v))
}

func runFile(args []string) {
	fs := flag.NewFlagSet("file", flag.ExitOnError)
	repo := fs.String("repo", ".", "path to the repo root")
	depth := fs.Int("depth", 6, "BFS depth")
	rec := fs.Bool("recommend-tests", false, "also recommend tests to run")
	_ = fs.Parse(args)
	if fs.NArg() < 1 {
		die("usage: blast file [flags] <repo-relative-path>")
	}
	path := fs.Arg(0)
	s := mustOpen(*repo)
	defer s.Close()

	opt := analyze.DefaultOptions()
	opt.MaxDepth = *depth
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	frep, err := analyze.ImpactOfFile(ctx, s, path, opt)
	if err != nil {
		die("impact_of_file: %v", err)
	}
	rootIDs := make([]int64, 0, len(frep.Symbols))
	for _, q := range frep.Symbols {
		if sym, _ := s.LookupSymbol(q); sym != nil {
			rootIDs = append(rootIDs, sym.ID)
		}
	}
	var recs []tests.TestRecommendation
	if *rec {
		recs, _ = tests.TestsForSymbols(s, rootIDs)
	}
	v, err := report.Synthesize(s, rootIDs, frep.Impacted, recs)
	if err != nil {
		die("synthesize: %v", err)
	}
	fmt.Println(report.FormatVerdict(v))
}

func runDiff(args []string) {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	repo := fs.String("repo", ".", "path to the repo root")
	depth := fs.Int("depth", 6, "BFS depth")
	rec := fs.Bool("recommend-tests", false, "also recommend tests to run")
	_ = fs.Parse(args)

	var rd io.Reader
	if fs.NArg() >= 1 {
		f, err := os.Open(fs.Arg(0))
		if err != nil {
			die("open diff: %v", err)
		}
		defer f.Close()
		rd = f
	} else {
		// Read from stdin when no path is given (pipes `git diff | blast diff`).
		rd = os.Stdin
	}
	s := mustOpen(*repo)
	defer s.Close()

	touched, err := diff.Parse(rd)
	if err != nil {
		die("parse diff: %v", err)
	}
	if len(touched) == 0 {
		die("diff is empty or unparseable (did you pipe a unified diff?)")
	}
	opt := analyze.DefaultOptions()
	opt.MaxDepth = *depth
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	dr, err := diff.AnalyzeFiles(ctx, s, touched, opt)
	if err != nil {
		die("analyze: %v", err)
	}
	rootIDs := make([]int64, 0)
	for _, f := range dr.Files {
		for _, t := range f.TouchedSymbols {
			if sym, _ := s.LookupSymbol(t.Qualified); sym != nil {
				rootIDs = append(rootIDs, sym.ID)
			}
		}
	}
	var recs []tests.TestRecommendation
	if *rec {
		recs, _ = tests.TestsForSymbols(s, rootIDs)
	}
	v, err := report.Synthesize(s, rootIDs, dr.UniqueImpacted, recs)
	if err != nil {
		die("synthesize: %v", err)
	}

	fmt.Printf("Diff touches %d file(s):\n", dr.TotalFiles)
	for _, f := range dr.Files {
		marker := ""
		if f.IsNew {
			marker = " (new)"
		}
		fmt.Printf("  %s%s — %d symbol(s) touched\n", f.Path, marker, len(f.TouchedSymbols))
		for _, t := range f.TouchedSymbols {
			fmt.Printf("    • %s  [%s, hunk %d-%d]\n", t.Qualified, t.Kind, t.HunkStart, t.HunkEnd)
		}
	}
	fmt.Println()
	fmt.Println(report.FormatVerdict(v))
}

// --- helpers -----------------------------------------------------------------

func mustOpen(repo string) *store.Store {
	root, err := filepath.Abs(repo)
	if err != nil {
		die("resolve repo path: %v", err)
	}
	s, err := store.Open(root)
	if err != nil {
		die("open store: %v", err)
	}
	return s
}

func or(a, b string) string {
	if strings.TrimSpace(a) == "" {
		return b
	}
	return a
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "blast: "+format+"\n", args...)
	os.Exit(1)
}
