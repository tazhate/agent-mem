# Changelog

All notable changes to amem. A version is the release date, `YYYY.MM.DD`, tagged
`vYYYY.MM.DD`; a second release on the same day adds `.1`, `.2`. Any release may change
config keys or the database schema; migrations run when the database is opened.

## [Unreleased]

### Added

- `recall.imported_weight` scales the score of memories brought in by `amem import` during
  automatic injection (1 = like native memories, 0 = never injected). Explicit search is not
  affected. The default is 1.

## [2026.11.11] - 2026-11-11

### Fixed

- Keep sessions on their original project when later events report another working directory,
  and adopt matching legacy projects for non-git directories too.
- Preserve event timestamps during observer backfill for memories and summaries.
- Put the JSON schema in the system prompt for Anthropic-compatible gateways that ignore
  `output_config`.
- Reuse a stable working directory for exec-based observer providers.
- Support legacy claude-mem import schemas and print the import report only after success.

## [2026.11.09] - 2026-11-09

### Changed

- Versions are release dates instead of semver. The code is the same as 0.1.0.
- Install from source with `go install github.com/tazhate/agent-mem/cmd/amem@master`:
  Go modules only understand semver tags, so `@latest` would stay on v0.1.0.

## [0.1.0] - 2026-11-09

First release.

### Added

- Single static Go binary `amem`: hooks, the `amemd` daemon, MCP server and CLI. No cgo,
  no Python, no vector database.
- Capture from Claude Code, Codex, Gemini CLI, Cursor, OpenCode (plugin shim) and Kimi Code
  hooks, plus `amem watch` for Claude and Codex transcripts. Hooks always exit 0; when the
  daemon is down, events go to a spool and are replayed in order.
- One SQLite database (WAL) with FTS5 and Russian/English snowball stems.
- Active recall at session start, on prompts, before file reads and edits, after failed
  commands, after compaction and for subagents. Scoring by term rarity, memory type,
  recency and usage, with a per-session budget, dedupe and fatigue for memories that are
  shown but never used. `recall.mode = "shadow"` logs what would be injected.
- Session-start summary for the user (project counts, last session, open work, recent
  memories), colored in Claude Code.
- Observer that turns each turn into memories and each session into a summary, through the
  Anthropic API, any OpenAI-compatible API, or a subscription CLI (`claude -p`, `codex exec`,
  `gemini -p`), with fallback chains and retries that keep batches queued during outages.
- Optional vector search: built-in static model2vec embeddings
  (`potion-multilingual-128M`) or any OpenAI-compatible embeddings endpoint, with sign-bit
  prefilter and int8 rescoring in memory.
- MCP tools `memory_search`, `memory_get`, `memory_timeline`, `memory_save`, `work_state`.
- `amem import claude-mem`: idempotent migration of observations, summaries, prompts and
  work state from a claude-mem database, read from a snapshot.
- `amem install` / `--uninstall` for every runtime (backups, symlink-safe, leaves foreign
  hooks alone), `amem init`, `doctor`, `search`, `show`, `save`, `stats`, `scrub`,
  `shadow-report`, `embed pull`, `bench`.
- Redaction of secrets (cloud keys, tokens, PEM keys, URL passwords, keyed values) and
  `<private>` blocks before anything is stored; private prompts keep their whole turn out
  of the database.

[2026.11.11]: https://github.com/tazhate/agent-mem/releases/tag/v2026.11.11
[2026.11.09]: https://github.com/tazhate/agent-mem/releases/tag/v2026.11.09
[0.1.0]: https://github.com/tazhate/agent-mem/releases/tag/v0.1.0
