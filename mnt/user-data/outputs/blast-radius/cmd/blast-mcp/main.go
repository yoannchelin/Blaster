// Command blast-mcp starts the Blast Radius MCP server.
//
// The server speaks MCP over stdio, so it can be plugged into Claude Desktop,
// Zed, Cursor, or anything else that loads MCP servers as a subprocess.
//
// Configuration:
//
//	blast-mcp --repo /abs/path/to/repo
//
// The repo must already have been indexed with `archaeo index`. For full
// functionality, also run `blast metrics` and `blast tests` once after each
// indexation.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/yourname/blast-radius/internal/mcpserver"
	"github.com/yourname/blast-radius/internal/store"
)

const version = "0.1.0"

func main() {
	repo := flag.String("repo", ".", "path to the indexed repo root")
	flag.Parse()

	// All logs go to stderr — stdout is the MCP wire.
	log.SetOutput(os.Stderr)
	log.SetPrefix("blast-mcp: ")

	root, err := filepath.Abs(*repo)
	if err != nil {
		log.Fatalf("resolve repo path: %v", err)
	}
	st, err := store.Open(root)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	var nSyms int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM symbols`).Scan(&nSyms)
	log.Printf("connected to index: %d symbols at %s", nSyms, st.Path())

	var nMetrics, nMappings int
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM blast_metrics`).Scan(&nMetrics)
	_ = st.DB().QueryRow(`SELECT COUNT(*) FROM blast_test_map`).Scan(&nMappings)
	if nMetrics == 0 {
		log.Printf("warning: no metrics computed — run `blast metrics --repo %s`", root)
	}
	if nMappings == 0 {
		log.Printf("warning: no test mappings — run `blast tests --repo %s`", root)
	}

	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "blast-radius",
		Version: version,
	}, nil)

	app := &mcpserver.Server{Store: st, RepoRoot: root}
	app.Register(srv)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "mcp server: %v\n", err)
		os.Exit(1)
	}
}
