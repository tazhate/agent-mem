package claudemem

import (
	"database/sql"
	"os"
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

func makeSource(t *testing.T, schema string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source.db")
	src, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	schema = strings.NewReplacer("GHPPREFIX", "ghp"+"_", "ANTPREFIX", "sk-"+"ant-").Replace(schema)
	if _, err := src.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return path
}

func legacySourceSchema() string {
	// Old claude-mem builds did not track merges: merged_into_project appears in neither
	// observations nor session_summaries, and the late columns of observations are absent too.
	r := strings.NewReplacer(
		"project TEXT, cwd TEXT, observed_model TEXT, started_at_epoch INTEGER", "project TEXT, started_at_epoch INTEGER",
		"INSERT INTO sdk_sessions VALUES (1, 'c1', 'm1', 'claude', 'proj/wt-x', NULL, NULL, 1000, 2000);",
		"INSERT INTO sdk_sessions VALUES (1, 'c1', 'm1', 'claude', 'proj/wt-x', 1000, 2000);",
		"CREATE TABLE work_state_entries (id INTEGER PRIMARY KEY, project TEXT, list_name TEXT, fields TEXT, created_at_epoch INTEGER);\n", "",
		"INSERT INTO work_state_entries VALUES (1, 'proj', 'todo', '{\"task\":\"x\",\"status\":\"todo\"}', 1300);\n", "",
		"CREATE TABLE observations (id INTEGER PRIMARY KEY, memory_session_id TEXT, project TEXT, merged_into_project TEXT, type TEXT,",
		"CREATE TABLE observations (id INTEGER PRIMARY KEY, memory_session_id TEXT, project TEXT, type TEXT,",
		"created_at_epoch INTEGER, generated_by_model TEXT, agent_type TEXT);", "created_at_epoch INTEGER);",
		"INSERT INTO observations VALUES (1, 'm1', 'proj/wt-x', NULL, 'bugfix', 'leaked GHPPREFIXabcdefghijklmnopqrstuvwxyz0123456789',",
		"INSERT INTO observations VALUES (1, 'm1', 'proj/wt-x', 'bugfix', 'leaked GHPPREFIXabcdefghijklmnopqrstuvwxyz0123456789',",
		"'narrative <private>hidden</private> visible', '[]', '[]', '[]', 1500, 'm', NULL);",
		"'narrative <private>hidden</private> visible', '[]', '[]', '[]', 1500);",
		"INSERT INTO observations VALUES (2, 'gone', 'proj', NULL, 'oddity', 'orphan memory', '', '[]', 'body', '[]', '[\"/a.go\"]', '[]', 1600, NULL, NULL);",
		"INSERT INTO observations VALUES (2, 'gone', 'proj', 'oddity', 'orphan memory', '', '[]', 'body', '[]', '[\"/a.go\"]', '[]', 1600);",
		"INSERT INTO session_summaries VALUES (1, 'm1', 'proj', NULL, 'req with ANTPREFIXapi03-abcdefghijklmnopqrstuvwxyz', '', '', '', '', '', 1700);",
		"INSERT INTO session_summaries VALUES (1, 'm1', 'proj', 'req with ANTPREFIXapi03-abcdefghijklmnopqrstuvwxyz', '', '', '', '', '', 1700);",
		"CREATE TABLE session_summaries (id INTEGER PRIMARY KEY, memory_session_id TEXT, project TEXT, merged_into_project TEXT,",
		"CREATE TABLE session_summaries (id INTEGER PRIMARY KEY, memory_session_id TEXT, project TEXT,",
	)
	return r.Replace(srcSchema)
}

func TestImportLegacySchema(t *testing.T) {
	srcPath := makeSource(t, legacySourceSchema())
	for _, dryRun := range []bool{false, true} {
		dst, err := store.Open(filepath.Join(t.TempDir(), "amem.db"))
		if err != nil {
			t.Fatal(err)
		}
		rep, err := Run(t.Context(), dst, Options{Source: srcPath, DryRun: dryRun})
		if err != nil {
			dst.Close()
			t.Fatalf("dry-run %v: %v", dryRun, err)
		}
		if rep.Sessions != 1 || rep.Memories != 2 || rep.Orphans != 1 || rep.Summaries != 1 || rep.Prompts != 2 || rep.Work != 0 {
			dst.Close()
			t.Fatalf("dry-run %v report: %+v", dryRun, rep)
		}
		if dryRun {
			var n int
			if err := dst.DB().QueryRow(`SELECT count(*) FROM projects`).Scan(&n); err != nil || n != 0 {
				t.Fatalf("dry-run wrote %d projects: %v", n, err)
			}
		} else {
			var cwd, model sql.NullString
			if err := dst.DB().QueryRow(`SELECT cwd, model FROM sessions WHERE id = 'claude-code:c1'`).Scan(&cwd, &model); err != nil {
				t.Fatal(err)
			}
			if cwd.Valid || model.Valid {
				t.Fatalf("missing source columns became cwd=%v model=%v", cwd, model)
			}
			again, err := Run(t.Context(), dst, Options{Source: srcPath})
			if err != nil || again.Memories != 0 || again.Sessions != 0 || again.Summaries != 0 || again.Prompts != 0 {
				t.Fatalf("re-run not idempotent: %+v, %v", again, err)
			}
		}
		dst.Close()
	}
}

func TestLocateAcceptsPlainDirectories(t *testing.T) {
	root := t.TempDir()
	plain := filepath.Join(root, "plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	im := importer{opt: Options{ProjectMap: map[string]string{"plain": plain}}}
	info, ok := im.locate("plain", nil)
	if !ok || info.Git || info.Key != plain || info.Root != plain {
		t.Fatalf("explicit plain directory: %+v, %v", info, ok)
	}
	im = importer{}
	info, ok = im.locate("plain", []string{plain})
	if !ok || info.Git || info.Key != plain {
		t.Fatalf("recorded plain cwd: %+v, %v", info, ok)
	}
	im = importer{opt: Options{SearchRoots: []string{root}}}
	if info, ok = im.locate("plain", nil); ok {
		t.Fatalf("search roots accepted a plain directory: %+v", info)
	}
	if err := os.Mkdir(filepath.Join(plain, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if info, ok = im.locate("plain", nil); !ok || !info.Git {
		t.Fatalf("search roots rejected git directory: %+v, %v", info, ok)
	}
}

func TestImportProjectMapPlainDirectory(t *testing.T) {
	srcPath := makeSource(t, srcSchema)
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	dst, err := store.Open(filepath.Join(t.TempDir(), "amem.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer dst.Close()
	rep, err := Run(t.Context(), dst, Options{Source: srcPath, ProjectMap: map[string]string{"proj": plain}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.MappedProjects != 1 || rep.LegacyProjects != 0 {
		t.Fatalf("project report: %+v", rep)
	}
	var key string
	if err := dst.DB().QueryRow(`SELECT p.key FROM projects p JOIN sessions s ON s.project_id = p.id WHERE s.id = 'claude-code:c1'`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != plain {
		t.Fatalf("session project key = %q, want %q", key, plain)
	}
}
