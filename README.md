# Blast Radius

> If I change this, what breaks? — answered without thinking.

**Blast Radius** is a companion to [Git Archaeologist](../git-archaeologist). Archaeologist *understands* a repo; Blast Radius tells you the *consequences* of changing any part of it. It reads the SQLite index that Archaeologist produces and adds its own analyses on top:

- **Impact analysis** — given any symbol, walk the call graph backwards to find everything that could break.
- **Diff analysis** — paste a `git diff` and get the pre-PR review a senior would give.
- **Risk scoring** — 0–100 score per symbol combining fan-in, exportedness, churn, file size, interface fanout.
- **Test recommendation** — given a change, surface the tests that exercise the impacted paths.

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

Risk score is a weighted sum of five normalised factors (`transitive_in`, `is_exported`, `is_interface`, `churn`, `loc_in_file`). Tuned so a private helper scores ~10 and a core utility scores ~85.

---

## Install

```bash
# Prerequisites: Git Archaeologist installed and a repo already indexed.
#   archaeo index --repo /path/to/repo

git clone https://github.com/yourname/blast-radius
cd blast-radius
go mod tidy
make install   # installs `blast` and `blast-mcp`
```

---

## Usage

### 1. Prep a repo

After every `archaeo index`, run these two once to populate Blast's tables:

```bash
cd /path/to/repo
blast metrics      # compute per-symbol risk scores (~10s on 50k symbols)
blast tests        # build the test→prod symbol map
```

You only need to re-run these when Archaeologist re-indexes — Blast detects stale data via `meta.last_index` and auto-invalidates its caches.

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
```

### 3. Plug into Claude Desktop (or any MCP client)

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

Each tool returns a structured **Verdict**: a severity bucket (`low|medium|high|critical`), a one-line headline, a list of reasons, top 8 impacted symbols ranked by risk, and recommended tests. The LLM consumes this and presents it; the heuristics are already baked in so the LLM doesn't have to invent them.

---

## Design choices worth knowing

- **Read-only on Archaeologist's tables.** We never modify upstream. If you want to fix something, re-run `archaeo index`.
- **All `blast_*` tables.** Namespace-scoped so multiple companion agents (Bug Hunter, Test Sentinel…) can share the same DB without collisions.
- **Cache invalidation by upstream timestamp.** Blast stores `seen_index` and compares to Archaeologist's `meta.last_index` on open. If they differ, all `blast_metrics`, `blast_test_map`, `blast_impact_cache` rows are wiped.
- **Brute-force BFS, capped at depth 6.** Beyond 6 hops, the impact set tends toward "everything reaches main()" — meaningless. The cap is configurable per call.
- **Interface fan-out is opt-in (default on).** Changing an interface ripples to every implementer; changing a struct method does not. We model this explicitly with the `implements` edges.
- **Severity heuristic is opinionated.** Tweak `report.classify()` to match your team's risk appetite.

---

## Roadmap

- [ ] Watch mode — invalidate caches on `.archaeo/index.db` mtime change
- [ ] Diff impact: handle renames (currently treated as delete+create)
- [ ] PageRank-based centrality factor in risk score
- [ ] Cyclomatic complexity factor in risk score (parse AST locally for touched funcs)
- [ ] TypeScript support (depends on Archaeologist's TS call graph)

---

## Project layout

```
cmd/
  blast/            CLI: blast info|metrics|tests|impact|file|diff
  blast-mcp/        MCP server (stdio transport)
internal/
  store/            DB layer (read archaeo, write blast_*)
  analyze/          Reverse BFS impact algorithm
  diff/             Unified diff parser + diff → impact
  risk/             Per-symbol risk score computation
  tests/            test → prod symbol map builder
  report/           Severity classification + human-friendly verdict
  mcpserver/        The 5 MCP tools
```
