package claudemem

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tazhate/agent-mem/internal/redact"
	"github.com/tazhate/agent-mem/internal/store"
)

const srcSchema = `
CREATE TABLE sdk_sessions (id INTEGER PRIMARY KEY, content_session_id TEXT, memory_session_id TEXT, platform_source TEXT,
	project TEXT, cwd TEXT, observed_model TEXT, started_at_epoch INTEGER, completed_at_epoch INTEGER);
CREATE TABLE observations (id INTEGER PRIMARY KEY, memory_session_id TEXT, project TEXT, merged_into_project TEXT, type TEXT,
	title TEXT, subtitle TEXT, facts TEXT, narrative TEXT, concepts TEXT, files_read TEXT, files_modified TEXT,
	created_at_epoch INTEGER, generated_by_model TEXT, agent_type TEXT);
CREATE TABLE session_summaries (id INTEGER PRIMARY KEY, memory_session_id TEXT, project TEXT, merged_into_project TEXT,
	request TEXT, investigated TEXT, learned TEXT, completed TEXT, next_steps TEXT, notes TEXT, created_at_epoch INTEGER);
CREATE TABLE user_prompts (id INTEGER PRIMARY KEY, content_session_id TEXT, prompt_number INTEGER, prompt_text TEXT, created_at_epoch INTEGER);
CREATE TABLE work_state_entries (id INTEGER PRIMARY KEY, project TEXT, list_name TEXT, fields TEXT, created_at_epoch INTEGER);
CREATE TABLE tool_uses (id INTEGER PRIMARY KEY, content_session_id TEXT, tool_name TEXT, tool_input TEXT, tool_response TEXT,
	prompt_number INTEGER, created_at_epoch INTEGER);
INSERT INTO sdk_sessions VALUES (1, 'c1', 'm1', 'claude', 'proj/wt-x', NULL, NULL, 1000, 2000);
INSERT INTO observations VALUES (1, 'm1', 'proj/wt-x', NULL, 'bugfix', 'leaked GHPPREFIXabcdefghijklmnopqrstuvwxyz0123456789',
	'', '["password=hunter2hunter2"]', 'narrative <private>hidden</private> visible', '[]', '[]', '[]', 1500, 'm', NULL);
INSERT INTO observations VALUES (2, 'gone', 'proj', NULL, 'oddity', 'orphan memory', '', '[]', 'body', '[]', '["/a.go"]', '[]', 1600, NULL, NULL);
INSERT INTO session_summaries VALUES (1, 'm1', 'proj', NULL, 'req with ANTPREFIXapi03-abcdefghijklmnopqrstuvwxyz', '', '', '', '', '', 1700);
INSERT INTO user_prompts VALUES (1, 'c1', 1, 'my db is postgres://u:Sup3rS3cret@h/db', 1100);
INSERT INTO user_prompts VALUES (2, 'c1', 2, '<private>only private</private>', 1200);
INSERT INTO work_state_entries VALUES (1, 'proj', 'todo', '{"task":"x","status":"todo"}', 1300);
`

func TestImportRedactsAndMaps(t *testing.T) {
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "cm.db")
	src, err := sql.Open("sqlite", srcPath)
	if err != nil {
		t.Fatal(err)
	}
	// Token prefixes are assembled at runtime so secret scanners do not flag the fixtures.
	schema := strings.NewReplacer("GHPPREFIX", "ghp"+"_", "ANTPREFIX", "sk-"+"ant-").Replace(srcSchema)
	if _, err := src.Exec(schema); err != nil {
		t.Fatal(err)
	}
	src.Close()
	dst, err := store.Open(filepath.Join(dir, "amem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	r := redact.New(true)
	rep, err := Run(t.Context(), dst, Options{Source: srcPath, Clean: r.Clean})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Memories != 2 || rep.Orphans != 1 || rep.Summaries != 1 || rep.Prompts != 2 || rep.Work != 1 {
		t.Fatalf("report: %+v", rep)
	}
	var dump string
	dst.DB().QueryRow(`SELECT (SELECT group_concat(title || body || facts) FROM memories) ||
		(SELECT group_concat(request) FROM summaries) || (SELECT group_concat(coalesce(input, '')) FROM events)`).Scan(&dump)
	for _, leak := range []string{"ghp_abcdef", "hunter2", "hidden", "sk-ant-api03", "Sup3rS3cret", "only private"} {
		if strings.Contains(dump, leak) {
			t.Fatalf("imported secret %q: %s", leak, dump)
		}
	}
	var typ string
	dst.DB().QueryRow(`SELECT type FROM memories WHERE origin_id = '2'`).Scan(&typ)
	if typ != "discovery" {
		t.Fatalf("unknown type mapped to %q", typ)
	}
	again, _ := Run(t.Context(), dst, Options{Source: srcPath, Clean: r.Clean})
	if again.Memories != 0 || again.Prompts != 0 || again.Summaries != 0 {
		t.Fatalf("re-run not idempotent: %+v", again)
	}
}
