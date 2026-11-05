# amem

[![ci](https://github.com/tazhate/agent-mem/actions/workflows/ci.yml/badge.svg)](https://github.com/tazhate/agent-mem/actions/workflows/ci.yml)

Cross-session memory for AI agents: Claude Code, Codex, Gemini CLI, Cursor, OpenCode, Kimi Code. One static Go binary, one SQLite file, no Node, no Python, no vector database. A drop-in replacement for claude-mem that brings your data along.

What it does. Hooks send session events to a local daemon. Once per turn, the observer compresses them into short memories with a model of your choice. Recall hands the agent what it needs at the moment it needs it: at session start, on a prompt, before reading a file that has history, on a failed command, after compact. Search is local, p99 18 ms on 108k memories, so it can run often and never makes the agent wait.

The concept and the reasoning behind the design are in [DESIGN.md](DESIGN.md).

## Installation

Prebuilt binaries for Linux and macOS (amd64, arm64) are on the [releases](https://github.com/tazhate/agent-mem/releases) page. Or build from source, which needs Go 1.26:

```bash
go install github.com/tazhate/agent-mem/cmd/amem@latest
amem init
```

Windows is not supported: the daemon relies on a unix socket and flock.

`amem init` asks four questions:

- **Observer** - what writes the memories. A subscription CLI (`claude`, `codex`, `gemini`: your own login, shared limits) or an API key (`anthropic`, or `openai`, which means any OpenAI-compatible server: OpenRouter, Ollama, vLLM, DeepSeek). The key is stored as a reference: `env:NAME`, `cmd:vault kv get ...`, `file:path`.
- **Embedder** - `static` (default: a built-in multilingual model2vec model, no service, one 530 MB download, about 3 minutes for 100k memories), `none` (full-text search only, with ru/en stemming), Ollama, or any `/v1/embeddings` endpoint. `amem embed pull` downloads the static model and checks its sha256.
- **Language** of the memories.
- **Recall mode**: `on`, `shadow` (compute what would be injected, but don't inject it) or `off`.

The same setup works without prompts through flags:

```bash
amem init --yes --observer claude --embed static --language Russian --recall shadow --runtimes claude-code,codex
```

Hooks are installed separately and can be removed again:

```bash
amem install claude-code --dry-run   # show the diff
amem install claude-code
amem install claude-code --uninstall
amem install detected                # every runtime found
```

The installer adds only its own entries and leaves other hooks and key order alone. Every file it changes gets a `*.bak-amem-<timestamp>` copy next to it. After installation Codex will ask once to confirm trust in the new hooks.

## Migrating from claude-mem

```bash
amem import claude-mem --dry-run
amem import claude-mem
```

The source database is opened read-only, so the claude-mem worker can keep running. Import is idempotent: a repeated run adds only what is new, so you can rerun it while both systems live side by side.

During the migration:

- the 100+ ad hoc claude-mem types are reduced to seven: `decision`, `bugfix`, `gotcha`, `change`, `discovery`, `preference`, `todo`. The original type is kept in `meta.orig_type`;
- observations without a live session get a synthetic session per day;
- worktree projects (`api/wt-feature`) are merged into their repository;
- projects with no recorded path become `legacy:<name>` and merge on their own once a session starts in a repository with that name.

Chroma is not migrated. MiniLM vectors do poorly on Russian, and the embedder you pick recomputes them.

On a 750 MB database the import takes 1.5-2.5 minutes. Every imported text field goes through the same redaction as live capture. If you imported with an older version, run `amem scrub --dry-run` and then `amem scrub` to clean what is already stored.

Switchover order:

1. `amem init --recall shadow` and `amem install claude-code`. For a week amem writes and computes but injects nothing.
2. `amem shadow-report` - what it would have injected.
3. Set `recall.mode = "on"` in the config and disable the claude-mem plugin.
4. A final `amem import claude-mem` to catch up.

## Commands

| Command | Purpose |
|---|---|
| `amem search [--project P\|all] [--type T] query` | search, the way the agent sees it |
| `amem show 123 456` | full memories |
| `amem save [--global] [--type T] text` | manual note; `--global` makes it visible in every project |
| `amem doctor [--probe]` | daemon, hooks, provider, embedder, spool |
| `amem embed pull [--model repo]` | download a model2vec model for `embed.kind = "static"` |
| `amem stats` | database counters |
| `amem watch --preset claude\|codex` | transcripts for runtimes without hooks (`--subagents-only` works with `codex` only) |
| `amem scrub [--dry-run]` | re-apply the current redaction rules to everything already stored |
| `amem bench recall\|hook` | latency on your own database (`hook` runs against a scratch daemon) |

MCP tools for the agent: `memory_search`, `memory_get`, `memory_timeline`, `memory_save`, `work_state`. Private memories are never returned to agents; `amem show` still prints them for you.

The daemon can also serve the tools over streamable HTTP if `daemon.http_addr` is set in the config. Only loopback addresses are accepted, and every request needs `Authorization: Bearer <token>` with the token from `~/.local/share/amem/http.token` (created on first start, mode 0600).

## Files

| Path | Contents |
|---|---|
| `~/.config/amem/config.toml` | config, mode 0600 |
| `~/.local/share/amem/amem.db` | data, FTS5 index and vectors |
| `~/.local/share/amem/amemd.sock` | daemon socket |
| `~/.local/share/amem/spool.jsonl` | events while the daemon is unavailable |
| `~/.local/share/amem/amemd.log` | daemon log |
| `~/.local/share/amem/http.token` | bearer token for HTTP MCP, only when `daemon.http_addr` is set |

Database files are kept at mode 0600. `AMEM_HOME` moves everything to another directory: config, database, socket. That makes it easy to run tests next to a working installation. `AMEM_DISABLE=1` turns hooks off in the current environment; the observer sets it for its own CLI processes to avoid recursion.

## How it works

```text
runtime hook -> amem hook (2-10 ms) -> unix socket -> amemd -> SQLite (WAL)
                    |                                   |-- recall: FTS5 + stemming + vectors, < 20 ms
                    +-- no daemon: spool + spawn        |-- observer: batch per turn -> LLM -> JSON schema
                                                        +-- embedder: background, pauses while hooks run
```

A hook never breaks the agent's session: any error gives exit code 0 and an empty response, and the event goes to the spool. Redelivery is filtered out by event id. The `scripts/durability.sh` test checks this by sending `kill -9` to the daemon in the middle of 200 parallel hooks.

## Development

```bash
go test -race ./...
AMEM_HOME=/tmp/amem-dur ./scripts/durability.sh
AMEM_QUALITY_DB=/path/amem.db go test ./internal/recall -run Manual -v   # eyeball recall quality
```
