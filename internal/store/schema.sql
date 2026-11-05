CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS projects (
	id         INTEGER PRIMARY KEY,
	key        TEXT NOT NULL UNIQUE,
	name       TEXT NOT NULL,
	root       TEXT,
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS projects_name ON projects(name);

-- Alternative keys that point at a project: legacy claude-mem names, merged duplicates.
CREATE TABLE IF NOT EXISTS project_aliases (
	alias      TEXT PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects(id)
);

CREATE TABLE IF NOT EXISTS sessions (
	id            TEXT PRIMARY KEY,          -- runtime:native_id
	runtime       TEXT NOT NULL,
	native_id     TEXT NOT NULL,
	project_id    INTEGER REFERENCES projects(id),
	cwd           TEXT,
	model         TEXT,
	turn          INTEGER NOT NULL DEFAULT 0,
	started_at    INTEGER NOT NULL,
	last_event_at INTEGER NOT NULL,
	ended_at      INTEGER,
	summarized_at INTEGER,
	origin        TEXT
);
CREATE INDEX IF NOT EXISTS sessions_project ON sessions(project_id, last_event_at);

-- Raw captured events. The observer consumes unprocessed rows; old processed rows are pruned.
CREATE TABLE IF NOT EXISTS events (
	id         INTEGER PRIMARY KEY,
	session_id TEXT NOT NULL,
	turn       INTEGER NOT NULL DEFAULT 0,
	kind       TEXT NOT NULL,
	tool       TEXT,
	input      TEXT,
	output     TEXT,
	error      TEXT,
	exit_code  INTEGER,
	ts         INTEGER NOT NULL,
	processed  INTEGER NOT NULL DEFAULT 0,
	origin     TEXT,
	origin_id  TEXT
);
CREATE INDEX IF NOT EXISTS events_session ON events(session_id, id);
CREATE INDEX IF NOT EXISTS events_pending ON events(session_id) WHERE processed = 0;
CREATE UNIQUE INDEX IF NOT EXISTS events_origin ON events(origin, origin_id) WHERE origin IS NOT NULL;

CREATE TABLE IF NOT EXISTS memories (
	id            INTEGER PRIMARY KEY,
	project_id    INTEGER REFERENCES projects(id),   -- NULL = global (preferences, working rules)
	session_id    TEXT,
	turn          INTEGER,
	type          TEXT NOT NULL,
	title         TEXT NOT NULL,
	body          TEXT NOT NULL DEFAULT '',
	facts         TEXT NOT NULL DEFAULT '[]',
	files         TEXT NOT NULL DEFAULT '[]',
	concepts      TEXT NOT NULL DEFAULT '[]',
	hash          TEXT NOT NULL UNIQUE,
	private       INTEGER NOT NULL DEFAULT 0,
	created_at    INTEGER NOT NULL,
	updated_at    INTEGER NOT NULL,
	last_used_at  INTEGER,
	use_count     INTEGER NOT NULL DEFAULT 0,
	inject_count  INTEGER NOT NULL DEFAULT 0,
	superseded_by INTEGER,
	origin        TEXT,
	origin_id     TEXT,
	meta          TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS memories_project ON memories(project_id, created_at);
CREATE INDEX IF NOT EXISTS memories_session ON memories(session_id);
-- Covering index for recall filters, so FTS hits are scoped without reading the wide rows.
CREATE INDEX IF NOT EXISTS memories_scope ON memories(id, project_id, type, private, superseded_by, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS memories_origin ON memories(origin, origin_id) WHERE origin IS NOT NULL;

CREATE TABLE IF NOT EXISTS memory_files (
	memory_id INTEGER NOT NULL,
	path      TEXT NOT NULL,
	PRIMARY KEY (memory_id, path)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS memory_files_path ON memory_files(path);

-- Stemmed full-text index; rowid = memories.id. Stems are computed in Go (snowball ru/en).
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
	title, body,
	content = '', contentless_delete = 1,
	tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TABLE IF NOT EXISTS summaries (
	id           INTEGER PRIMARY KEY,
	session_id   TEXT NOT NULL,
	project_id   INTEGER,
	request      TEXT NOT NULL DEFAULT '',
	investigated TEXT NOT NULL DEFAULT '',
	learned      TEXT NOT NULL DEFAULT '',
	completed    TEXT NOT NULL DEFAULT '',
	next_steps   TEXT NOT NULL DEFAULT '',
	notes        TEXT NOT NULL DEFAULT '',
	created_at   INTEGER NOT NULL,
	origin       TEXT,
	origin_id    TEXT
);
CREATE INDEX IF NOT EXISTS summaries_project ON summaries(project_id, created_at);
CREATE INDEX IF NOT EXISTS summaries_session ON summaries(session_id, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS summaries_origin ON summaries(origin, origin_id) WHERE origin IS NOT NULL;

-- What recall injected into which session; drives dedupe, budgets and usefulness feedback.
CREATE TABLE IF NOT EXISTS injections (
	id         INTEGER PRIMARY KEY,
	session_id TEXT NOT NULL,
	memory_id  INTEGER NOT NULL,
	trigger    TEXT NOT NULL,
	tokens     INTEGER NOT NULL DEFAULT 0,
	ts         INTEGER NOT NULL,
	used       INTEGER NOT NULL DEFAULT 0,
	shadow     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS injections_session ON injections(session_id, memory_id);

-- Append-only log of work-state writes; folded on read (latest key wins, null clears, done closes).
CREATE TABLE IF NOT EXISTS work_entries (
	id         INTEGER PRIMARY KEY,
	project_id INTEGER,
	list       TEXT NOT NULL,
	fields     TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	origin     TEXT,
	origin_id  TEXT
);
CREATE INDEX IF NOT EXISTS work_entries_list ON work_entries(project_id, list, id);
CREATE UNIQUE INDEX IF NOT EXISTS work_entries_origin ON work_entries(origin, origin_id) WHERE origin IS NOT NULL;

CREATE TABLE IF NOT EXISTS vectors (
	memory_id INTEGER PRIMARY KEY,
	model     TEXT NOT NULL,
	dim       INTEGER NOT NULL,
	scale     REAL NOT NULL,
	q         BLOB NOT NULL
);

-- Read position of transcript files followed by `amem watch`.
CREATE TABLE IF NOT EXISTS watch_state (
	path   TEXT PRIMARY KEY,
	offset INTEGER NOT NULL,
	size   INTEGER NOT NULL
);

-- Document frequency per stem, for IDF-weighted relevance in recall.
CREATE VIRTUAL TABLE IF NOT EXISTS memories_vocab USING fts5vocab(memories_fts, 'row');

-- Materialized document frequency per stem (fts5vocab seeks are slow on big indexes).
CREATE TABLE IF NOT EXISTS stem_df (
	term TEXT PRIMARY KEY,
	doc  INTEGER NOT NULL
) WITHOUT ROWID;
