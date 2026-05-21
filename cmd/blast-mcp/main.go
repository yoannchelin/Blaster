// Command blast-mcp is the Blast Radius MCP server (stdio transport).
//
// Configure in Claude Desktop / Cursor / Zed:
//
//	"blast": {
//	  "command": "/abs/path/bin/blast-mcp",
//	  "args": ["--repo", "/abs/path/to/repo"]
//	}
//
// All output goes to stderr; stdout is the MCP wire protocol.
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

func main() {
	repo := flag.String("repo", ".", "path to the repo root (must contain .archaeo/index.db)")
	flag.Parse()

	root, err := filepath.Abs(*repo)
	if err != nil {
		fmt.Fprintf(os.Stderr, "blast-mcp: resolve repo: %v\n", err)
		os.Exit(1)
	}

	s, err := store.Open(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "blast-mcp: open store: %v\n", err)
		os.Exit(1)
	}
	defer s.Close()

	srv := mcp.NewServer(&mcp.Implementation{Name: "blast", Version: "0.1.0"}, nil)
	bs := &mcpserver.Server{Store: s, RepoRoot: root}
	bs.Register(srv)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.SetOutput(os.Stderr)
	log.Printf("blast-mcp: serving repo %s", root)

	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "blast-mcp: %v\n", err)
		os.Exit(1)
	}
}
