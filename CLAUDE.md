# CLAUDE.md — Blast Radius

> Briefing pour reprendre ce projet dans une nouvelle session Claude.
> Lis ce fichier *en entier* avant d'écrire la moindre ligne. Il contient les décisions déjà prises et les pièges à ne pas refaire.

---

## 1. Le projet en une phrase

**Blast Radius** est un **serveur MCP** compagnon de [Git Archaeologist](../git-archaeologist) qui répond à la question senior la plus utile : *"si je change ça, qu'est-ce qui pète ?"*. Il lit la DB SQLite produite par Archaeologist (`.archaeo/index.db`), n'y écrit jamais, et ajoute ses propres tables `blast_*` pour les analyses qu'il fait lui-même. Tout tourne **en local**, **sans LLM**, **sans appel API**. C'est du pur graph traversal + SQL.

Use cases prioritaires :
- *"Si je rename `ChargeCustomer`, quoi d'autre breaks ?"* → impact analysis.
- *"Voilà mon `git diff` avant PR, dis-moi si je devrais m'inquiéter."* → diff impact (le tueur).
- *"Quels tests dois-je rerun ?"* → test recommendation via le call graph.

Blast Radius **ne remplace pas** Git Archaeologist — il le **complète**. Archaeologist comprend, Blast prévient des conséquences.

---

## 2. Décisions architecturales — ne pas remettre en cause sans bonne raison

| Décision | Raison |
|---|---|
| **Lit la DB de Git Archaeologist, ne la modifie jamais** | Séparation claire des responsabilités. Si tu veux modifier les `symbols` ou `edges`, re-run `archaeo index`. Sinon tu casses la cohérence entre les deux outils. |
| **Tables préfixées `blast_*`** | Plusieurs agents peuvent partager la même SQLite sans collision de namespace (futur Bug Hunter, Test Sentinel, etc.). |
| **Reverse BFS sur le call graph, cap depth=6** | Au-delà de 6 hops, *tout* atteint `main()`. Le concept de "blast" perd son sens. Le cap est par défaut, configurable par appel. |
| **Pas de LLM** | Blast est déterministe par essence. La synthèse "voici les 3 trucs qui doivent t'inquiéter" est faite par `report.Synthesize()` avec des heuristiques simples. Le LLM côté client MCP fait juste la présentation. |
| **Cache invalidation par `meta.last_index`** | Quand Archaeologist re-indexe, il met à jour `meta.last_index`. Blast compare ce timestamp à son `blast_meta.seen_index` et wipe `blast_metrics`, `blast_test_map`, `blast_impact_cache` si différent. |
| **Risk score = somme pondérée de 5 facteurs normalisés** | Heuristique opinionée, pas science. Voir `risk.DefaultWeights()`. Tunable sans recompile (la fonction prend `Weights` en paramètre). |
| **Interface fan-out activé par défaut** | Changer une interface ripple sur tous les implementers. C'est *le* différenciateur vs un simple reverse-grep. |
| **Diff parser maison, pas `go-git diff`** | On veut accepter un `git diff` collé dans le chat, un fichier `.patch`, ou stdin. Un parser unified-diff custom de 160 lignes fait le job sans dépendance. |
| **Severity bucket en 4 niveaux (low/medium/high/critical)** | Tunable dans `report.classify()`. Critical = interface fan-out + >50 impactés. |
| **CLI + serveur MCP, deux binaires séparés** | Le CLI sert au debug et au dogfooding, le MCP au consumption depuis Claude Desktop / Cursor / Zed. |

---

## 3. Stack technique exacte

- **Go 1.25+** (requis par MCP SDK v1.4.x).
- **`github.com/modelcontextprotocol/go-sdk` v1.4.1** — même version que Git Archaeologist.
- **`github.com/mattn/go-sqlite3`** — driver SQLite avec FTS5 compilé. Pas de cgo problématique.
- **`golang.org/x/tools`** — listé dans `go.mod` mais peu utilisé directement (pour cohérence avec l'éventuel partage de helpers avec Archaeologist).
- **Aucune dépendance LLM** — pas d'Ollama, pas d'embeddings, pas d'API.

---

## 4. Layout — où se trouve quoi

```
cmd/
  blast/            CLI : `blast info|metrics|tests|impact|file|diff`
  blast-mcp/        Binaire serveur MCP (stdio transport)
internal/
  store/            DB layer : ouvre une DB archaeo, ajoute les tables blast_*
                    - schema.go     : DDL des blast_* tables uniquement
                    - store.go      : Open() vérifie que `symbols` existe upstream
                    - store_test.go : smoke test avec DB fixture in-memory
  analyze/          Reverse BFS impact algorithm
                    - analyze.go      : Impact() et ImpactOfFile()
                    - analyze_test.go : test sur un mini call graph 4-nœuds
  diff/             Unified diff parser + diff → impact
                    - diff.go      : Parse() (Reader → []TouchedFile)
                    - impact.go    : AnalyzeFiles() (TouchedFiles → impact)
                    - diff_test.go : test sur un sample diff réaliste
  risk/             Per-symbol risk score computation
                    - risk.go : Compute() écrit blast_metrics rows
  tests/            test → prod symbol map builder
                    - tests.go : BuildMap() + TestsForSymbols()
  report/           Severity classification + verdict assembly
                    - report.go : Synthesize() + FormatVerdict() (CLI text)
  mcpserver/        Les 5 MCP tools
                    - server.go : Register() + 5 handlers
```

**Schéma SQLite (blast_* uniquement)** : `blast_meta`, `blast_impact_cache`, `blast_test_map`, `blast_metrics`. Détail complet dans `internal/store/schema.go`. Les tables archaeo (`symbols`, `edges`, `files`, etc.) sont lues mais jamais écrites.

---

## 5. Les 5 outils MCP exposés

Définis dans `internal/mcpserver/server.go`. Si tu en ajoutes un, **résiste à la prolifération** — la valeur vient du focus de ces 5.

| Tool | Quand l'utiliser |
|---|---|
| `impact_of` | Impact d'un changement sur un symbole nommé (qualified name). |
| `impact_of_file` | Impact si on touche n'importe quel symbole d'un fichier. |
| `impact_of_diff` | **Le tueur** : impact d'un `git diff` collé. Pre-PR review. |
| `risk_score` | Lecture de `blast_metrics` pour un symbole (rapide, requiert `blast metrics` au préalable). |
| `tests_to_run` | Pour une liste de symboles, retourne les tests qui les exercent (requiert `blast tests` au préalable). |

Tous les tools retournent un **Verdict** structuré avec : severity (`low|medium|high|critical`), headline, reasons, top 8 impactés rankés par risk, tests recommandés. Le LLM client n'a qu'à présenter, les heuristiques sont déjà appliquées côté serveur.

---

## 6. État actuel — ce qui marche, ce qui manque

### ✅ Implémenté et validé en production

- Schéma `blast_*` complet avec invalidation par timestamp
- Store layer en lecture sur archaeo + écriture sur blast_*
- Algorithme d'impact (reverse BFS + interface fan-out + depth cap)
- Parser unified diff (avec hunk headers, single-count, fichiers nouveaux/supprimés)
- Mapping diff → symbols touchés par intersection de plages de lignes
- Risk scoring 0–100 sur 5 facteurs normalisés
- Test mapping (forward BFS depuis chaque test)
- Severity classifier + verdict synthesizer
- CLI `blast` (info / metrics / tests / impact / file / diff avec stdin support)
- Serveur MCP stdio avec les 5 tools
- 3 fichiers de tests unitaires (store, analyze, diff)
- README + Makefile

**Dogfooding réalisé sur Hugo** (892 fichiers Go, 12 916 symboles, 10 683 call edges, 3 088 impl edges) :
- `blast metrics` : **0.66s** sur 9 206 symboles — scaling linéaire confirmé
- `blast tests` : **3.4s** pour 1 995 test functions → 130 430 mappings
- `blast impact` sur `NewHugoSites` (674 callers transitifs) : verdict MEDIUM, 41 symboles, depth 4 jusqu'au test infra
- `blast diff` sur un diff 2-fichiers : détection correcte des symboles touchés, recommandations de tests triées par distance (les tests unitaires proches en premier, les benchmarks/intégration après)

**Bugs corrigés lors de la mise en route** (documentés en section 7) :
- Fichiers tous à la racine au lieu de `internal/` — réorganisés
- 4 fonctions de tri manquantes — ajoutées
- API MCP incorrecte — corrigée
- Backtick imbriqué dans un struct tag — corrigé

### ❌ À faire (ordre = ROI décroissant)

1. ~~**Faire compiler + tester**~~ ✅ Fait.

2. ~~**Dogfooding sur Git Archaeologist + Hugo**~~ ✅ Fait.

3. **Tests d'intégration sur Terraform / Kubernetes** — Hugo est validé. Kubernetes (~10k tests) permettrait de mesurer `blast tests` à l'échelle annoncée dans les pièges (potentiellement ~1 min).

4. ~~**Gestion des renames dans le diff parser**~~ ✅ Fait. `rename from`/`rename to` détectés ; `AnalyzeFiles` run l'impact sur les symboles de l'ancien path, `FileImpact` expose `OldPath`+`IsRename`.

5. ~~**PageRank centrality dans le risk score**~~ ✅ Fait. Colonne `pagerank` lue depuis `symbols` (Archaeologist la calcule déjà). 6ème facteur, poids 0.10 pris sur `TransitiveIn` (0.35→0.25). Sur Hugo : `hugolib.Test` (pagerank=1.0) remonte correctement en tête.

6. ~~**Cyclomatic complexity locale**~~ ✅ Fait. `internal/complexity.OfFunction()` parse le source Go via `go/ast` et compte if/for/range/case/select/&&/||. `TouchedSymbol.Complexity` populé dans `AnalyzeFiles`. CLI affiche un ⚠ pour complexity ≥ 10. `Store.Root()` ajouté pour dériver la racine du repo depuis le path de la DB.

7. ~~**Watch mode**~~ ✅ Fait. `Store.Watch()` poll `meta.last_index` sur un ticker, invalide les caches blast au re-index. CLI : `blast watch [--interval] [--with-tests]`. MCP : goroutine en background dans `blast-mcp` (caches toujours frais sans intervention).

8. **TypeScript** — dépend du call graph TS d'Archaeologist (déjà implémenté). Pas de blocage technique, juste vérifier que les requêtes SQL marchent sur des qualified names format TS (`relPath.Function`).

9. ~~**Mode "explain"**~~ ✅ Fait. `ImpactedHighlight.Explain` = rôle (test entry point, HTTP handler, constructor…) + profondeur + métriques (transitive_in, interface, exported). Affiché en `→` dans le CLI, `omitempty` en JSON.

10. ~~**Cache des impact reports**~~ ✅ Fait. `CachedImpact()` wraps `Impact()` avec `blast_impact_cache` (clé sha256 rootID+options). Hugo : 466ms → 6ms sur hit. Invalidation via `Store.Open` au re-index.

---

## 7. Pièges connus / leçons apprises

- **Ne pas modifier les tables archaeo** — `symbols`, `edges`, `files`, `embeddings`, `commits` sont READ-ONLY côté blast. Si tu as besoin de plus, demande à archaeo de l'ajouter à son schéma upstream.
- **`mcp-blast` log uniquement sur stderr** — stdout est le wire MCP, idem que pour `archaeo-mcp`.
- **`mcp.AddTool` Out type** — peut être struct, pointer-to-struct, ou `any`. Si tu changes vers `any`, le schema généré disparaît côté client.
- **API MCP SDK v1.4.1** — `mcp.NewServer` prend `(*mcp.Implementation, *mcp.ServerOptions)`, pas `(string, string, nil)`. Le transport stdio est `&mcp.StdioTransport{}`, pas `mcp.NewStdioTransport()`. Vérifie avant toute mise à jour du SDK.
- **Backtick interdit dans les struct tags Go** — un backtick à l'intérieur d'un raw string literal (délimité par backticks) ferme le literal. Dans les `jsonschema:"..."` tags, ne jamais utiliser de backtick dans la valeur.
- **Le parser de diff ignore les `old_count==0`** — c'est volontaire : un hunk d'insertion pure (`@@ -0,0 +1,12 @@`) est traité comme une plage de lignes au moment de l'insertion (start=1 dans l'exemple). Si tu touches `parseHunkHeader`, vérifie que `TestParseSingleLineHunk` passe toujours.
- **`tests.BuildMap` produit 0 mappings si archaeo n'a pas indexé les `_test.go`** — il faut impérativement passer `--with-tests` à `archaeo index`. Sur Hugo avec `--with-tests` : 1 995 test functions → 130 430 mappings en 3.4s. Sans ce flag, 0 résultats.
- **`blast_test_map.depth` est le MIN** — un test atteint la même fonction prod par plusieurs chemins, on garde le plus court. `ON CONFLICT DO UPDATE SET depth = MIN(depth, excluded.depth)` dans `PutTestMapping`.
- **`risk.transitiveCallers` est cappé à depth=6** — au-delà tout converge vers main(). Si tu changes ça, vérifie que les scores ne s'écrasent pas vers 100.
- **Les scores de `transitive_in` sont 0 pour les librairies/outils CLI** — normal : leurs fonctions sont appelées depuis l'extérieur du repo, pas en interne. Le score est alors porté par export/churn/LOC. Sur un monorepo ou une appli (Hugo, Kubernetes), les `transitive_in` sont significatifs et les verdicts différenciés.
- **L'invalidation de cache est *all-or-nothing*** — au moindre changement d'`last_index`, tout `blast_*` data est wipé. C'est volontairement conservateur. Si tu veux invalider à la granularité fichier, il faut un schéma plus fin (genre tracker quels fichiers ont changé via `git diff <last_index>..HEAD`).
- **`diff.Parse` ne supporte pas les diffs binaires** — il les ignore silencieusement (`Binary files differ` n'a pas de hunk header). C'est OK pour notre use case Go.
- **`risk.fetchFileChurn` ne déclenche pas une transaction** — la lecture est simple, mais ça veut dire que si Archaeologist re-indexe en parallèle, on lit un snapshot incohérent. En pratique on ne fait jamais les deux en même temps. Si ça devient un problème : ajouter `BEGIN`/`COMMIT` autour du `fetch`.
- **Compteurs FanIn/FanOut peuvent mentir si edges sont dédupliqués** — la table `edges` a une PK (src, dst, relation), donc deux appels de A→B comptent comme 1. C'est volontaire mais l'utilisateur peut être surpris.
- **Les fichiers à la racine doivent tous être dans le même package** — Go interdit plusieurs packages dans le même répertoire. La structure `internal/` est obligatoire, pas optionnelle.
- **`pagerank` est déjà dans `symbols`, pas besoin de le calculer** — Archaeologist écrit la colonne à chaque `archaeo index`. Blast la lit directement. Si elle vaut 0 partout, c'est que la version d'Archaeologist est ancienne (avant le commit pagerank).
- **Fixtures de test : toujours ajouter `pagerank REAL NOT NULL DEFAULT 0`** dans le DDL des `symbols` des tests. Sinon `scanSymbol` plante avec "no such column: pagerank". Voir `internal/store/store_test.go` et `internal/analyze/analyze_test.go`.
- **`CachedImpact` invalide sur les options, pas seulement le rootID** — la clé sha256 inclut `maxDepth + includeTests + expandInterfaces`. Changer une option = cache miss garanti, pas de résultat périmé.
- **Rename git : deux formats possibles** — le format `rename from/to` (similarity ≥ 50%) est géré. Le format "delete + create" (similarity < 50%) produit deux entrées séparées dans le diff et est traité comme une suppression + nouveau fichier — comportement correct mais on perd la traçabilité sémantique du rename.

---

## 8. Comportements attendus de toi (Claude) sur ce projet

### Style de code
- **Code Go idiomatique**. Pas de OOP gratuit. Petites interfaces (1-3 méthodes max).
- **Commentaires en haut de chaque fichier expliquant le *pourquoi***, pas juste le *quoi*. Les commentaires existants suivent ce pattern.
- **Aucun TODO/FIXME laissé en place** sauf accord explicite. Soit on fait, soit on liste dans ce fichier.
- **Erreurs wrappées avec `fmt.Errorf("...: %w", err)`**, jamais retournées nues sans contexte.

### Style de communication
- **Réponses concises**. L'utilisateur préfère le punch à la verbosité. Pas de listes à puces dans les réponses conversationnelles sauf nécessité.
- **Tu peux pousser back**. Si l'utilisateur propose un truc qui contredit la section 2, dis-le clairement.
- **Tu travailles directement, sans préambule.** Pas de "Excellente question !", pas de récap inutile.

### Quand modifier le code
- **Toujours `view` le fichier juste avant `str_replace`** — le code peut avoir bougé.
- **Au moindre doute sur une API tierce**, fais une `web_search` plutôt que de deviner. Le SDK MCP Go évolue vite.
- **Lance `make test` après chaque modif structurelle.**

### Quand ajouter une feature
- **Vérifie d'abord la section 6** pour le ROI.
- **Une feature = un commit logique.**
- **Met à jour le README** si tu changes la surface publique (CLI flags, MCP tools).
- **Met à jour CE fichier** si tu prends une nouvelle décision archi ou apprends un piège.

---

## 9. Commandes utiles

```bash
# Premier démarrage (module name déjà yourname/blast-radius, pas besoin de renommer)
go mod tidy
make test                       # smoke tests sur DB fixtures
make build                      # bin/blast, bin/blast-mcp

# Indexer un repo avec archaeo (OBLIGATOIRE : --with-tests pour avoir des recommandations de tests)
/path/to/archaeo index --repo /path/to/repo --no-embed --with-tests

# Prep blast sur ce repo
cd /path/to/repo
/path/to/bin/blast metrics      # Hugo 9k syms : 0.66s
/path/to/bin/blast tests        # Hugo 2k tests : 3.4s → 130k mappings

# Dogfooding
/path/to/bin/blast info
/path/to/bin/blast impact "github.com/your/repo/payment.ChargeCustomer" --recommend-tests
/path/to/bin/blast file "internal/payment/charge.go"

# Pre-PR check
git diff main | /path/to/bin/blast diff --recommend-tests

# Brancher sur Claude Desktop
# ~/Library/Application Support/Claude/claude_desktop_config.json :
# {
#   "mcpServers": {
#     "blast": {
#       "command": "/abs/path/bin/blast-mcp",
#       "args": ["--repo", "/abs/path/to/repo"]
#     }
#   }
# }
```

---

## 10. Comment reprendre dans une nouvelle session

1. Lis ce fichier **en entier**.
2. Vérifie que `git-archaeologist` est installé et fonctionnel à côté — Blast en dépend.
3. Demande à l'utilisateur : *"On reprend où ? Section 6 indique qu'il reste : watch mode, TypeScript, cyclomatic complexity, tester sur Kubernetes. Tu veux attaquer lequel ?"*
4. Si l'utilisateur dit *"continue"* sans préciser, propose le premier item non fait de la section 6.
5. Avant d'écrire du code, `view` les fichiers concernés.
6. Travaille. Mets à jour ce CLAUDE.md à la fin si tu as appris quelque chose.

---

## 11. Relation avec Git Archaeologist

Blast Radius **n'existe pas sans** Git Archaeologist. Le contrat entre les deux :

| Côté Archaeologist (responsabilité) | Côté Blast (responsabilité) |
|---|---|
| Indexer le code (parser, embeddings, git) | Ne pas dupliquer cette indexation |
| Maintenir `symbols`, `edges`, `files`, `embeddings`, `commits`, `meta` | Lire ces tables, ne jamais y écrire |
| Garantir que `meta.last_index` est mis à jour à chaque réindex | Comparer à `blast_meta.seen_index` pour invalider |
| Définir les relations `calls`, `implements`, `imports`, `embeds`, `spawns`, `schedules` | Choisir lesquelles utiliser pour l'impact (calls + implements pour l'instant) |
| Multi-langage (Go + TypeScript) | Marche pour Go aujourd'hui ; TS supportable trivialement (même schéma) |

Si Archaeologist ajoute une nouvelle relation utile pour l'impact (genre `mutates` = écrit sur un global), c'est à Blast de l'incorporer. Si Blast a besoin d'une donnée pas dans le schéma, c'est à Archaeologist de l'ajouter — pas à Blast de la calculer.

Le but final : un **stack d'agents** qui partagent la même DB. Bug Hunter, Test Sentinel, Doc Whisperer pourront tous brancher leurs propres tables `bughunter_*`, `sentinel_*`, etc. sur le même `.archaeo/index.db`.
