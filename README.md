# Blast Radius

> If I change this, what breaks? — answered without thinking.

**Blast Radius** is a companion to [Git Archaeologist](../git-archaeologist). Archaeologist *understands* a repo; Blast Radius tells you the *consequences* of changing any part of it. It reads the SQLite index that Archaeologist produces and adds its own analyses on top:

- **Impact analysis** — walk the call graph backwards to find everything that could break.
- **Diff analysis** — paste a `git diff` and get the pre-PR review a senior would give.
- **Risk scoring** — 0–100 score per symbol combining fan-in, exportedness, churn, PageRank, interface fan-out, and file size.
- **Test recommendation** — given a change, surface the tests that exercise the impacted paths, closest first.
- **Cyclomatic complexity** — flags high-complexity functions touched by a diff.
- **Watch mode** — recomputes metrics automatically whenever Archaeologist re-indexes.

Like Archaeologist, it runs **100% locally**. No LLM call, no API, no cloud. Pure graph traversal and SQL.

---

## Why this exists

A senior developer doesn't open a PR without asking *"what else does this touch?"*. Junior developers can't, because answering that question requires holding the whole call graph in your head. Blast Radius automates the senior reflex.

It pairs naturally with Archaeologist:

| Question | Tool |
|---|---|
| Where is auth handled? | Archaeologist (`query`) |
| What's the architecture? | Archaeologist (`architecture_overview`) |
| **If I rename this method, what breaks?** | **Blast Radius (`impact_of`)** |
| **My PR touches 4 files — what's the blast radius?** | **Blast Radius (`impact_of_diff`)** |
| **What tests should I rerun?** | **Blast Radius (`tests_to_run`)** |

---

## How it works

Blast Radius opens the database at `<repo>/.archaeo/index.db`. It never writes to Archaeologist's tables — it only adds its own `blast_*` tables:

```
                  ┌────────────────────────────┐
                  │  Archaeologist tables       │
                  │  (read-only for us)         │
                  │  symbols / edges / files    │
                  │  embeddings / commits …     │
                  └────────────────────────────┘
                            ▲
                            │ read
                            │
                  ┌────────────────────────────┐
                  │  Blast Radius layer         │
                  │  blast_metrics              │
                  │  blast_test_map             │
                  │  blast_impact_cache         │
                  │  blast_meta                 │
                  └────────────────────────────┘
                            ▲
                            │
        ┌───────────────────┼────────────────────────┐
        ▼                   ▼                        ▼
   blast CLI           blast-mcp                Other tools
   (impact, diff…)     (MCP server)             (reuse the DB)
```

The core algorithm is **reverse BFS** on the `calls` graph, with interface fan-out:

```
impact(S, depth):
    visited = {S}
    frontier = {S}
    for d in 1..depth:
        next = ∅
        for sym in frontier:
            for caller in IncomingCallers(sym):
                visited.add(caller); next.add(caller)
            if sym is an interface:
                for impl in Implementations(sym):
                    visited.add(impl); next.add(impl)
        frontier = next
    return visited
```

Results are cached in `blast_impact_cache` (keyed by `sha256(rootID:maxDepth:options)`). Cache hits drop from ~400ms to ~6ms on Hugo-scale repos. Caches are invalidated whenever Archaeologist re-indexes.

Risk score is a weighted sum of six normalised factors: `transitive_in` (0.25), `churn` (0.20), `is_exported` (0.15), `is_interface` (0.15), `loc_in_file` (0.15), `pagerank` (0.10). Tuned so a private helper scores ~10 and a core interface scores ~85.

---

## Performance

Validated on Hugo (12k symbols) and etcd (18k symbols, 21k call edges):

| Operation | Hugo | etcd |
|---|---|---|
| `blast metrics` | 0.66s / 9k syms | 1.0s / 8.7k syms |
| `blast tests` | 3.4s → 130k mappings | 1.3s → 44k mappings |
| `blast impact` (cold) | ~466ms | ~15ms |
| `blast impact` (cached) | ~6ms | ~6ms |

Scaling is linear. `blast impact` on a cached result resolves in single-digit milliseconds regardless of repo size.

---

## Install

```bash
# Prerequisites: Git Archaeologist installed and a repo already indexed.
#   archaeo index --repo /path/to/repo --with-tests

git clone https://github.com/yoannchelin/Blaster
cd Blaster
go mod tidy
make install   # installs `blast` and `blast-mcp` into ./bin/
```

---

## Usage

### 1. Prep a repo

Index with Archaeologist first, then run these two once to populate Blast's tables:

```bash
archaeo index --repo /path/to/repo --no-embed --with-tests

blast metrics --repo /path/to/repo   # ~1s on 10k symbols
blast tests   --repo /path/to/repo   # build test→prod map
```

You only need to re-run these when Archaeologist re-indexes — or use `blast watch` to do it automatically.

### 2. Ad-hoc queries

```bash
# Impact of renaming/changing one symbol
blast impact "github.com/x/y/payment.ChargeCustomer" --recommend-tests

# Impact of touching a whole file
blast file "internal/payment/charge.go" --recommend-tests

# Pre-PR check — pipe `git diff` in
git diff main | blast diff --recommend-tests

# Or pass a patch file
blast diff /tmp/proposed.patch

# Show current state of blast tables
blast info
```

### 3. Watch mode

Keep metrics fresh automatically as Archaeologist re-indexes:

```bash
blast watch --repo /path/to/repo --with-tests
```

Polls `meta.last_index` every 5 seconds (configurable with `--interval`). The MCP server (`blast-mcp`) runs this automatically in the background.

### 4. Plug into Claude Desktop (or any MCP client)

```json
{
  "mcpServers": {
    "blast-radius": {
      "command": "/abs/path/bin/blast-mcp",
      "args": ["--repo", "/abs/path/to/your/repo"]
    }
  }
}
```

Then ask in natural language: *"use blast-radius to tell me what changes if I refactor ChargeCustomer"*.

---

## Tools exposed by the MCP server

| Tool | Purpose |
|---|---|
| `impact_of` | Impact of changing a named symbol |
| `impact_of_file` | Impact of touching any symbol in a file |
| `impact_of_diff` | Impact of a unified diff (pre-PR review) |
| `risk_score` | Pre-computed risk row for a symbol |
| `tests_to_run` | Which tests should I rerun for these changes |

Each tool returns a structured **Verdict**: a severity bucket (`low|medium|high|critical`), a one-line headline, a list of reasons, top 8 impacted symbols ranked by risk with an explanation of why each is risky, and recommended tests. The LLM presents it; the heuristics are already baked in.

---

## Reading the output

```
Severity: MEDIUM
Moderate impact: 15 symbols affected.

  • 2 direct callers (depth=1) will see this change immediately.
  • 1 interface touched — all implementers must be updated.

Top impacted symbols:
  1. [risk  40.8, depth 1] go.etcd.io/etcd/server/v3/storage/backend/testing.NewTmpBackendFromCfg
     server/storage/backend/testing/betesting.go:29
     → Constructor; direct caller; 187 symbols depend on it transitively; exported — public API surface
  ...

Recommended tests to run (closest first):
  1. TestBackendSnapshot  (depth 1)
  2. TestLessorRenewExtendPileup  (depth 1)
  ...and 185 more.
```

When running `blast diff`, high-complexity functions touched by the diff are flagged:

```
  • payment.ChargeCustomer  [func, hunk 42-89, complexity=14 ⚠]
```

---

## Design choices worth knowing

- **Read-only on Archaeologist's tables.** We never modify upstream. If you want to fix something, re-run `archaeo index`.
- **All `blast_*` tables.** Namespace-scoped so multiple companion agents (Bug Hunter, Test Sentinel…) can share the same DB without collisions.
- **Cache invalidation by upstream timestamp.** Blast stores `seen_index` and compares to Archaeologist's `meta.last_index` on open. If they differ, all `blast_metrics`, `blast_test_map`, `blast_impact_cache` rows are wiped.
- **Reverse BFS, capped at depth 6.** Beyond 6 hops, the impact set tends toward "everything reaches main()" — meaningless. The cap is configurable per call.
- **Interface fan-out is on by default.** Changing an interface ripples to every implementer; we model this explicitly with the `implements` edges.
- **PageRank from Archaeologist.** We read the `symbols.pagerank` column written by Archaeologist — no recomputation. Symbols with high PageRank (heavily referenced across the graph) score higher.
- **Cyclomatic complexity via go/ast.** Parsed on demand for Go files touched by a diff. Non-Go files gracefully fall back to 1.
- **TypeScript support.** All SQL queries and the BFS are language-agnostic. Test discovery uses Archaeologist's `is_test` file flag rather than Go-specific naming patterns.
- **Severity heuristic is opinionated.** Tweak `report.classify()` to match your team's risk appetite.

---

## Project layout

```
cmd/
  blast/            CLI: blast info|metrics|tests|impact|file|diff|watch
  blast-mcp/        MCP server (stdio transport, auto watch in background)
internal/
  store/            DB layer (read archaeo, write blast_*)
  analyze/          Reverse BFS impact algorithm + impact cache
  diff/             Unified diff parser + diff → impact (handles renames)
  risk/             Per-symbol risk score computation (6 factors)
  tests/            test → prod symbol map builder
  report/           Severity classification + human-friendly verdict
  complexity/       Cyclomatic complexity via go/ast
  mcpserver/        The 5 MCP tools
```
