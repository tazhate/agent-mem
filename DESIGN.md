# agent-mem: agent memory in Go

The binary is `amem`. This document covers the concept and design of a replacement for claude-mem 15.x. v1 is implemented; installation and commands are in [README.md](README.md), and departures from the original design are listed in the section "Decisions after implementation".

## Diagnosis of claude-mem

Numbers were taken on 5 Nov 2026 from a live installation with about 108k memories (`~/.claude-mem/`, worker 15.4.0, plugin 15.6.0).

| What | Fact |
|---|---|
| Code size | 151k lines of TypeScript, 25 tables, 54 migrations |
| chroma-mcp | 111% CPU, 2.9 GB RSS, 2.8 GB on disk; 44 process crashes in 24 hours, and the index is rebuilt after each one |
| Vectorization | one observation = 10-26 documents in Chroma (narrative plus every fact separately), each one run through ONNX MiniLM on the CPU |
| Embeddings | `all-MiniLM-L6-v2` is an English-only model, while half of the notes are in Russian |
| Sessions | 15 757 of 107 906 observations point to a `memory_session_id` that does not exist in `sdk_sessions` |
| Projects | 315 projects; worktrees become separate projects (for example, `api/wt-feature` apart from `api`) |
| Types | the mode defines 6 types, the database holds 100+ (`oddity`, `test-preparation`, `state-check`...) |
| Feedback | `relevance_count = 0` on all 107 906 records - the "this memory was useful" loop doesn't work |
| Processes | every Claude Code session gets its own `node -e <loader>` for MCP (~48 MB each); compression goes through the Claude Agent SDK, which means a full CLI subprocess |
| Versions | the worker runs 15.4.0 while 15.6.0 is installed, and the `CAPTURE_BROKEN` marker has been set since 1 Nov even though capture works |

Almost every problem traces back to three decisions.

1. **Two databases instead of one.** SQLite and Chroma are kept in sync through a watermark, an outbox and handling of corrupt segments. That took 4.5k lines (`ChromaSync.ts` + `ChromaMcpManager.ts`), and the link itself is stdio MCP to a Python process. At 108k records a vector index as a separate service isn't needed at all.
2. **Memory identity is tied to the LLM provider's session.** `memorySessionId` is issued by the Agent SDK and changes after a worker restart. Hence the orphans, the FK hacks and a separate 251-line document, `SESSION_ID_ARCHITECTURE.md`.
3. **Compression on every tool call through a long-lived agent.** The model sees a stream of scattered tool calls without the user's intent and writes XML that is later parsed with regexes.

On top of that, recall is passive. At session start the 50 most recent records are injected, ordered by time rather than relevance, and after that the agent has to think of calling search on its own. It rarely does.

## Principles

- **One static binary.** No Node, Bun, Python, uvx, no `npx ... repair`. An update means replacing the file and restarting the daemon.
- **One database.** SQLite: data, FTS5 and vectors in one file. No separate vector service.
- **A hook never waits for an LLM.** Capture is a write to a queue, and recall is a local search with no network. Slow work (compression, embeddings) runs in the background inside the daemon.
- **We own identity.** Session = `runtime:native_session_id`, project = git root. Nothing depends on which provider does the compression.
- **Recall is active and frugal.** Memory is offered at the right moment (prompt, file, error), only above a relevance threshold and without repeats within a session.
- **Runtime and model are adapters, not the core.** The core doesn't know the word "Claude".
- **Silence when idle.** Zero CPU when no agents are working.

## Architecture

```text
 runtime (Claude Code / Codex / Gemini CLI / Cursor / OpenCode / ...)
   |  hooks: JSON on stdin            |  MCP (stdio or HTTP)
   v                                  v
 amem hook <runtime> [event]     amem mcp  (thin proxy)
   |  unix socket, 0.4/1.5 s deadline |
   |  no daemon -> spool.jsonl + spawn |
   v                                  v
 +--------------------------------------------------+
 | amemd (one per user)                             |
 |  ingest   -> normalization, secret redaction     |
 |  recall   -> FTS5 + vectors + scoring, < 20 ms   |
 |  observer -> batch per turn, LLM, JSON schema    |
 |  embedder -> background queue, CPU limit         |
 |  janitor  -> raw event TTL, consolidation        |
 +--------------------------------------------------+
   |
   v
 ~/.local/share/amem/amem.db   (SQLite WAL: data + FTS5 + vectors)
```

Subcommands of the single binary:

- `amem hook <runtime> [event]` - the hook client. A Go binary starts in 2-5 ms; claude-mem's node->bun chain was to be measured for comparison in stage 1.
- `amemd` (`amem daemon`) - holds the database, the queue and the vectors in memory. Starts lazily on the first hook, exits after N minutes of idle time, or runs under a systemd user unit.
- `amem mcp` - a stdio proxy to the daemon, about 5 MB RSS. Runtimes that speak MCP over HTTP go to the daemon directly, with no process per session.
- `amem search`, `amem show <id>`, `amem import`, `amem doctor` - CLI for humans and scripts.

If the daemon is unavailable, the hook appends the event to `spool.jsonl` (`O_APPEND`, one line) and exits with code 0. On start the daemon drains the spool. The hook never blocks the session on any error except a broken config of its own.

The hook<->daemon protocol version is checked on every connection. On a mismatch the daemon restarts itself from the new binary. A situation like "worker on 15.4.0, plugin on 15.6.0" is ruled out by design.

## Storage and search

### Schema (draft)

```sql
projects  (id, key UNIQUE, root, remote, name)          -- key = hash(git remote) or hash(git common dir)
sessions  (id TEXT PK, runtime, native_id, project_id, cwd, model, started_at, ended_at)
events    (id, session_id, turn, kind, tool, input, output, exit_code, ts)   -- raw data, TTL 30 days
memories  (id, project_id NULL, session_id, turn, type, title, body, facts JSON,
           files JSON, hash UNIQUE, created_at, last_used_at, use_count, inject_count,
           superseded_by, origin, origin_id)
memory_files (memory_id, path)                           -- index for file recall
memories_fts (title, body, facts, stems)                 -- FTS5, external content
vectors   (memory_id PK, model, dim, scale REAL, q BLOB) -- int8
summaries (session_id PK, request, done, learned, next_steps)
injections(session_id, memory_id, trigger, ts, used)     -- for feedback and dedup
work_items(id, project_id, list, fields JSON, status, updated_at)
```

`project_id NULL` is global memory: user preferences, working rules. It is visible in every project.

The project is resolved with `git rev-parse --git-common-dir`. All worktrees of one repository land in one project, and the branch goes into the event metadata. For a one-worktree-per-task workflow this is critical: in claude-mem every worktree starts its memory from scratch.

### Why no Chroma

108k records × 1 vector × 384 dimensions in int8 is 41 MB. A brute-force cosine scan on one core fits in 10-20 ms; HNSW doesn't pay for itself at this size. Vectors are stored as a BLOB in the same SQLite file and read into a flat array when the daemon starts. There's no index to rebuild after a crash, because there is no index.

One vector per record (title + body), not 10-26. That alone cuts the load 15-20 times.

The embedder is an interface with three implementations:

- `none` - FTS5 only, zero CPU. On short technical notes BM25 with stemming works well.
- `static` - a model2vec static model evaluated in process, pure Go. The default is `minishlab/potion-multilingual-128M` (101 languages, 256 dims, bge-m3 tokenizer). There is no neural network at inference: Unigram tokenization, the mean of the token vectors, normalization. The 512 MB weight matrix is memory-mapped, so only rows of tokens that occur are paged in. About 660 memories per second on one core, so 108k memories take 3-4 minutes; a query takes about 1 ms. Tokens and vectors match the Python `model2vec` reference exactly.
- `openai` - any OpenAI-compatible `/v1/embeddings`: Ollama, LM Studio, vLLM, OpenAI, Voyage through a proxy.

Why static by default: a transformer through Ollama (`bge-m3`, `paraphrase-multilingual`) separates relevant from random text better, but on a laptop CPU it needed 30-100 ms per query and around 14 hours for 108k memories with three cores busy. The static model trades some quality for no service and almost no CPU. Measured on real memories with a Russian query: relevant memories 0.25-0.64 cosine, random ones median 0.13 and p95 0.24, hence `recall.vector_floor = 0.25` for it (0.40 suits transformer models).

Embedding runs in the background: one goroutine, small batches, paused while hooks are running. Each vector records its model. Switching models drops the old model's vectors and re-embeds newest first.

### Russian in FTS5

The `unicode61` tokenizer plus a separate `stems` column that holds word stems from snowball (`github.com/kljensen/snowball`, Russian and English). The query is stemmed the same way. "Запечатан" ("sealed") finds "запечатанный", and "Vault" finds "vault".

### Scoring

```text
score = RRF(bm25_rank, vec_rank)
      × type_weight          -- decision/gotcha/preference rank above discovery
      × recency(type)        -- discovery: 30-day half-life; decision/preference: no decay
      × (1 + ln(1 + use_count))
      × penalty(inject_count - use_count)   -- shown often and never useful
```

Records with `superseded_by` set never appear in results.

## Observer: compression

**One batch per turn, not per tool call.** Events accumulate per session and are flushed to the LLM on one of several triggers: end of turn (Stop), 40 events, 30k tokens of raw input, or 60 seconds of silence. The model gets the user's prompt, the filtered tool calls and the assistant's final answer. It sees the intent, not a stream of `Read` calls. The number of LLM calls drops 5-10 times.

**Prefilter without an LLM:**

- drop noise: `ls`, `git status`, `TodoWrite`, repeated `Read` of an unchanged file (by hash);
- trim output: head + tail, with a per-event limit;
- redact secrets with regexes (tokens, keys, `Authorization:`) and `<private>` tags;
- facts that are visible anyway (which files were touched, which commands failed and with what code) go straight into `events`, with no model involved.

**The response is JSON against a schema** (structured output or tool calling, whichever the provider has). A closed enum of types:

`decision`, `bugfix`, `gotcha`, `change`, `discovery`, `preference`, `todo`.

An unknown type becomes `discovery`. Title up to 80 characters, body up to 600, facts as a list. The output language is a single setting, not 28 mode files.

**Update instead of accumulate.** Each batch comes with the 5 closest existing records by project and files. The model can return `supersedes: [ids]` or `update: {id, body}`. Outdated records are marked and drop out of results. A year from now the database holds current knowledge, not 108k layers of history.

**Stateless.** Every batch is an independent call. No long SDK session, no `memorySessionId`, no generator restart loops.

**The session summary** is written on SessionEnd. If a session was killed without SessionEnd, the summary is generated lazily on the next start in the same project.

### Providers

Two protocols cover almost the whole market:

- **OpenAI-compatible chat completions** - OpenAI, OpenRouter, Ollama, vLLM, LM Studio, DeepSeek, Gemini (OpenAI endpoint), Kimi, z.ai;
- **Anthropic Messages**.

The third option is **`exec`**: any CLI that takes a prompt on stdin and prints JSON on stdout (`claude -p --output-format json`, `codex exec`, `gemini -p`). This is how a subscription works without an API key, the same way claude-mem uses Claude Code's OAuth today. The process starts for one batch and exits.

The config is a list of providers with priorities and fallback on 429/5xx. Key rotation uses the same list.

## Recall: when memory fires

This is the main difference from claude-mem. Search is local, so it can run on every event instead of once per session.

| Trigger | What we search | Budget |
|---|---|---|
| SessionStart (startup) | open `work_items`, the project's previous session summary, top durable records (decision, gotcha, preference) | 600 tokens |
| SessionStart (compact/resume) | working state of the current session: what was done, what was decided, what is left | 800 tokens |
| UserPromptSubmit | hybrid search on the prompt text within the project + global memory | 3-5 records, only above the threshold |
| PreToolUse `Read`/`Edit`/`Write` | records with this path in `memory_files`, gotcha and bugfix first | 1-3 records, once per file per session |
| PostToolUseFailure, Bash with exit code != 0 | the error text against bugfix/gotcha | 1-2 records: "seen before, #4821, fixed like this" |
| SubagentStart (where available) | search on the subagent's task | 3 records |

Rules for every trigger:

- **Threshold.** Nothing relevant means nothing gets injected. An empty injection beats noise.
- **Per-session dedup.** A record shown once is not shown again (the `injections` table).
- **Total budget per session.** After, say, 6k tokens of injections the threshold goes up.
- **Index format as in claude-mem**, because that part works well: `#4821 gotcha infra: Vault sealed - scripts silently fall back to the file (~60t)`. The agent fetches details through MCP `memory_get`.

**Feedback.** A record counts as useful if, after the injection, the agent called `memory_get` on its id, referenced `#id` in a reply, or edited files from its `files`. That increments `use_count`. Many impressions with zero use means a scoring penalty. In claude-mem this field is zero everywhere.

Target latency for the recall hook is p99 < 80 ms from process start to exit, of which search is < 20 ms.

### MCP tools

Few, and with no overlap:

- `memory_search(query, project?, type?, limit?)` - the index;
- `memory_get(ids)` - full records;
- `memory_timeline(id, window?)` - neighboring records in time;
- `memory_save(text, type?, global?)` - a manual note;
- `work_state(list, fields)` - task lists and state, compatible in meaning with `work_state_write`.

## Runtime adapters

The core accepts one canonical event:

```json
{"runtime":"claude-code","session":"…","cwd":"…","event":"tool_post",
 "turn":7,"tool":{"name":"Bash","input":{},"output":"…","exit":1},"text":null}
```

Events: `session_start`, `prompt`, `tool_pre`, `tool_post`, `tool_fail`, `turn_end`, `compact`, `subagent_start`, `session_end`.

An adapter does two things: it parses a specific runtime's stdin into this event, and it renders the response (the injected context) in that runtime's format. Each adapter is one Go file plus a set of test fixtures.

| Runtime | Capture | Injection channel |
|---|---|---|
| Claude Code | hooks (full set) | `hookSpecificOutput.additionalContext` |
| Codex CLI | hooks `session_start`, `pre_tool_use`, `post_tool_use` (claude-mem already registers them in `~/.codex/config.toml`) | hook output; fields to be verified |
| Gemini CLI, Cursor, OpenCode | their hooks/plugin API | per each API |
| Anything else | `amem watch`: tailing JSONL transcripts (`~/.claude/projects`, `~/.codex/sessions` and so on) | MCP and an `AGENTS.md` block only |

`amem install <runtime>` writes the hooks and MCP into that runtime's config and supports `--uninstall`. No nvm tricks in `PATH`: the binary lives in `~/.local/bin`.

## Migrating from claude-mem

```bash
amem import claude-mem --db ~/.claude-mem/claude-mem.db --dry-run
amem import claude-mem --db ~/.claude-mem/claude-mem.db
```

**Reading.** The source database is never opened for writing and never locked. The import takes a snapshot into a temporary file through the SQLite backup API and reads that. The claude-mem worker can keep running.

**Mapping:**

| claude-mem | amem | Notes |
|---|---|---|
| `sdk_sessions` | `sessions` | id = `platform_source:content_session_id` |
| `observations` | `memories` | linked through `memory_session_id -> content_session_id`; the 15 757 orphans get a synthetic session per project and day |
| `observations.type` (100+) | enum of 7 | normalization table; the original type goes to `meta.orig_type` |
| `facts`, `concepts`, `files_*` | `facts`, `memory_files` | JSON arrays, paths normalized to the project root |
| `observation_files` | `memory_files` | |
| `session_summaries` | `summaries` | |
| `user_prompts` | `events(kind=prompt)` | |
| `tool_uses` (164k) | `events` | only with `--raw 30d` (a window) or `--raw all`; skipped by default |
| `work_state_entries` | `work_items` | last value per key wins, as in the original |
| `project` (basename) | `projects` | recomputed from the sessions' `cwd` via the git root; worktrees collapse into the parent repository |
| Chroma | - | not migrated; MiniLM vectors are incompatible with the new model and do poorly on Russian |

**Idempotency.** Unique key `(origin='claude-mem', origin_id)`. A repeated run adds only what is new, so the import can be rerun while both systems live side by side. Dedup by `hash` of the normalized text.

**Report** after a run: how many records were migrated by type and project, how many orphans, which projects were merged, how many duplicates were dropped.

**Switchover:**

1. `amem import` + `amem install claude-code` in shadow mode: amem captures but does not inject. For a week we compare what it would show with what claude-mem shows.
2. Turn amem injections on, turn the claude-mem plugin off. Its database stays untouched as a fallback.
3. A final `amem import` to catch up, then remove the plugin. Move the `~/.claude-mem/` data to `trash`, don't `rm` it.

Embeddings for 108k records are computed in the background after the import: the last 90 days first, then the rest.

## What we don't carry over

No viewer UI in v1; there are `amem search` and `amem show`. A simple read-only page can be added later. Cloud sync, teambrain, knowledge corpora, work-view with codex runs, the deliverables bucket, PostHog telemetry, tree-sitter smart-file-read and the 28 mode files are not carried over either.

Each of these would be a separate request with a justification, not "the original had it".

## Budgets

| Metric | Target |
|---|---|
| Capture hook, p99 | < 20 ms |
| Recall hook, p99 | < 80 ms |
| Search over 110k records | < 20 ms |
| Daemon RSS at 110k records with vectors | < 150 MB |
| CPU when idle | 0 |
| CPU while agents work (no local embedder) | < 5% of one core |
| Database size after import (without `--raw`) | < 400 MB |
| Processes per runtime session | 0 persistent |

Budgets are checked by a CI benchmark on a synthetic database of the same size.

## Stack

- Go 1.26+, one module, no cgo in the main build.
- SQLite: `modernc.org/sqlite` (cgo-free, FTS5 built in), WAL, IMMEDIATE write transactions.
- MCP: `github.com/modelcontextprotocol/go-sdk`.
- Stemming: `github.com/kljensen/snowball`.
- Vectors: a custom flat int8 scan, no dependencies.
- HTTP to LLMs: standard `net/http`, no provider SDKs.

```text
cmd/amem/            main, subcommands
internal/event/      canonical event
internal/adapter/    claudecode, codex, gemini, cursor, opencode, kimi, transcript
internal/store/      schema, migrations, queries
internal/recall/     FTS, vectors, scoring, budgets
internal/observer/   batching, prefilter, prompt, JSON schema
internal/provider/   openai, anthropic, exec
internal/embed/      none, static (model2vec), openai; vector index
internal/daemon/     socket, spool, lifecycle, API
internal/mcpserver/  tools, HTTP with bearer token
internal/importer/   claudemem
internal/config/     TOML, validation, secret references
internal/project/    git common dir -> project
internal/redact/     private tags, secret patterns
internal/stem/       snowball ru/en, ё folding
internal/install/    runtime config writers
internal/watch/      transcript followers
```

## Stages

1. **Capture core.** Daemon, socket, spool, Claude Code adapter, `events`, `sessions`, `projects`. Done when: hooks don't slow the session down, and nothing is lost on `kill -9` of the daemon.
2. **Import.** `amem import claude-mem` with a report. Done when: all 107 906 records are in place and projects are collapsed.
3. **Recall on FTS5.** All triggers, budgets, dedup, MCP tools. Shadow mode.
4. **Observer.** Batches, JSON schema, supersede, three providers.
5. **Vectors.** Embedder through Ollama, hybrid scoring, background backfill.
6. **Other runtimes.** Codex, then the rest on demand, `amem watch`.
7. **Switchover** following the steps above.

After stage 3 the system can already be turned on: claude-mem memory is migrated, and recall works without vectors and without an LLM.

## Decisions after implementation

The open questions were settled like this:

- **The user picks the embedder and the compressor** in `amem init`. Embedder: `none`, the built-in static model (default), Ollama, or any OpenAI-compatible `/v1/embeddings`. Observer: a subscription CLI (`claude`, `codex`, `gemini`) or an API key (Anthropic, OpenAI-compatible). Keys are stored as `env:`, `cmd:`, `file:` references.
- **Global memory**: only `preference` with `global=true` from the observer, and explicit `memory_save --global`.
- **No sync between machines in v1.**

What changed compared to the design above:

- The socket lives in the data dir, not in `$XDG_RUNTIME_DIR`, so the whole installation moves with a single `AMEM_HOME` variable.
- The import reads the live claude-mem database read-only, without `VACUUM INTO`: a 750 MB copy isn't needed, and it doesn't get in the worker's way.
- claude-mem's `gotcha` concept is not turned into the `gotcha` type: a dry run showed that this would produce 28k gotchas out of 108k records.
- Vectors are not kept fully in RAM. Memory holds only the sign bits (dim/8 bytes per record); candidates are picked by Hamming distance, then the top 256 are rescored exactly in int8 from SQLite. 110k records: 1.5-2.8 ms, 5-14 MB.
- Stem DF for IDF lives in the `stem_df` table: `fts5vocab` on a large index accounted for up to 40% of search time.
- MCP for Claude Code is registered through `claude mcp add`: `~/.claude.json` is a live state file and must not be rewritten from outside.
- The hook asks the adapter whether the runtime accepts context on this event. If not (Kimi on SessionStart, Cursor on `beforeSubmitPrompt`), recall doesn't run and the record isn't counted as shown.
- The daemon version includes the git revision: a new binary replaces the running daemon by itself.
- Kimi Code was added: `[[hooks]]` in `~/.kimi-code/config.toml`, context through stdout.

Measurements on the imported database (108k records): capture p99 13 ms, recall hook p99 40 ms, search p99 18 ms, import 1.5-2.5 minutes.

## Review fixes (5 Nov 2026)

A full review (three internal passes plus Codex) led to these changes after v1:

- Privacy: a fully `<private>` prompt makes its whole turn private in the session, so its tool calls and answer are never stored; tool input is cleaned after JSON decoding (escaped `<private>` tags were slipping through); hooks redact before writing the spool; private memories are never served to agents; the importer redacts, and `amem scrub` cleans existing data; HTTP MCP needs a bearer token and a loopback address.
- Durability: IMMEDIATE transactions, spool recovery of abandoned drain files, bounded spool locking, per-chunk observer commits, deterministic watcher event ids, persisted pending tool calls.
- Behavior: adapters declare `CanInject`, so memory counts as shown only when the runtime can deliver it and the hook received it; the daemon restarts only for a rebuilt copy of its own binary; config is validated and a broken file falls back to defaults; `ё` is folded to `е` with an automatic one-time FTS rebuild; memory file lookups use an indexed `base` column instead of `LIKE`.
- Providers: per-provider timeouts in a chain, process-group kill for CLI providers, `max_completion_tokens` for OpenAI, schema downgrade only on schema errors, backoff and give-up for failing batches and summaries.
- Runtimes: Gemini `prompt_response` and nested tool errors, Gemini MCP tool names, OpenCode context kept for the whole turn and assistant text from message events, recall after compaction.
- Installer: symlinks preserved, foreign `amem` MCP entries left alone, no files created on uninstall, change detection between read and write, strict shell and TOML quoting.
