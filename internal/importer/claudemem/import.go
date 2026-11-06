// Package claudemem imports a claude-mem database (v10+ schema) into amem. The source is opened
// read-only and never written; every imported row carries origin/origin_id, so re-running the
// import only adds what is new.
package claudemem

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tazhate/agent-mem/internal/project"
	"github.com/tazhate/agent-mem/internal/redact"
	"github.com/tazhate/agent-mem/internal/store"
)

const origin = "claude-mem"

type Options struct {
	Source string
	DryRun bool
	// RawSince imports tool_uses newer than this unix ms time; 0 skips raw tool uses, -1 imports all.
	RawSince int64
	// ProjectMap maps claude-mem project names to directories, overriding detection.
	ProjectMap map[string]string
	// SearchRoots are scanned for <root>/<name>/.git to place projects without a recorded cwd.
	SearchRoots []string
	Log         io.Writer
	// Clean is applied to every imported text field (redaction + size limit), like live capture.
	Clean func(string) string
}

type Report struct {
	Sessions, OrphanSessions      int
	Memories, Duplicates, Skipped int
	Orphans                       int
	Summaries, Prompts, Work, Raw int
	Types                         map[string]int
	Projects                      map[string]string // claude-mem base name -> amem project key
	LegacyProjects, GitProjects   int
	Elapsed                       time.Duration
}

func (r *Report) Print(w io.Writer) {
	fmt.Fprintf(w, "sessions:   %d (+%d synthetic for orphaned observations)\n", r.Sessions, r.OrphanSessions)
	fmt.Fprintf(w, "memories:   %d new, %d duplicates dropped, %d skipped (empty), %d were orphans\n", r.Memories, r.Duplicates, r.Skipped, r.Orphans)
	fmt.Fprintf(w, "summaries:  %d   prompts: %d   work entries: %d   raw tool uses: %d\n", r.Summaries, r.Prompts, r.Work, r.Raw)
	fmt.Fprintf(w, "projects:   %d mapped to git repos, %d kept as legacy:<name> (merge automatically when a session starts in a repo of that name)\n", r.GitProjects, r.LegacyProjects)
	fmt.Fprintln(w, "types:")
	for _, t := range slices.Sorted(maps.Keys(r.Types)) {
		fmt.Fprintf(w, "  %-11s %d\n", t, r.Types[t])
	}
	fmt.Fprintf(w, "elapsed:    %s\n", r.Elapsed.Round(time.Millisecond))
}

type importer struct {
	opt  Options
	src  *sql.DB
	dst  *store.Store
	rep  *Report
	log  io.Writer
	proj map[string]int64 // base name -> amem project id
	// sessions keyed by claude-mem memory_session_id and content_session_id
	byMemory  map[string]sessionInfo
	byContent map[string]sessionInfo
}

type sessionInfo struct {
	ID      string // amem session id
	Project string // base project name
}

func Run(ctx context.Context, dst *store.Store, opt Options) (*Report, error) {
	start := time.Now()
	src, err := sql.Open("sqlite", "file:"+opt.Source+"?mode=ro&_pragma=busy_timeout(30000)")
	if err != nil {
		return nil, err
	}
	defer src.Close()
	if err := src.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("open %s: %w", opt.Source, err)
	}
	if opt.Clean == nil {
		opt.Clean = func(s string) string { return s }
	}
	im := &importer{
		opt: opt, src: src, dst: dst, log: cmp.Or(opt.Log, io.Writer(io.Discard)),
		rep:       &Report{Types: map[string]int{}, Projects: map[string]string{}},
		proj:      map[string]int64{},
		byMemory:  map[string]sessionInfo{},
		byContent: map[string]sessionInfo{},
	}
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"projects", im.projects},
		{"sessions", im.sessions},
		{"observations", im.observations},
		{"summaries", im.summaries},
		{"prompts", im.prompts},
		{"work state", im.work},
		{"raw tool uses", im.raw},
	}
	for _, s := range steps {
		t := time.Now()
		if err := s.fn(ctx); err != nil {
			return im.rep, fmt.Errorf("%s: %w", s.name, err)
		}
		fmt.Fprintf(im.log, "  %-14s %s\n", s.name, time.Since(t).Round(time.Millisecond))
	}
	if !opt.DryRun {
		t := time.Now()
		if err := dst.OptimizeFTS(ctx); err != nil {
			return im.rep, fmt.Errorf("optimize index: %w", err)
		}
		if err := dst.RebuildDF(ctx); err != nil {
			return im.rep, fmt.Errorf("rebuild df: %w", err)
		}
		fmt.Fprintf(im.log, "  %-14s %s\n", "fts optimize", time.Since(t).Round(time.Millisecond))
	}
	im.rep.Elapsed = time.Since(start)
	return im.rep, nil
}

// baseProject strips the worktree suffix claude-mem appends ("api/wt-feature" -> "api").
func baseProject(project, merged string) string {
	p := cmp.Or(strings.TrimSpace(merged), strings.TrimSpace(project))
	if i := strings.IndexByte(p, '/'); i > 0 {
		p = p[:i]
	}
	return cmp.Or(p, "unknown")
}

// write runs fn in a destination transaction, or not at all in dry-run mode.
func (im *importer) write(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if im.opt.DryRun {
		return nil
	}
	return im.dst.Write(ctx, fn)
}

func (im *importer) projects(ctx context.Context) error {
	rows, err := im.src.QueryContext(ctx, `
		SELECT project, coalesce(merged_into_project,''), '' FROM observations GROUP BY 1, 2
		UNION SELECT project, '', coalesce(cwd,'') FROM sdk_sessions
		UNION SELECT project, coalesce(merged_into_project,''), '' FROM session_summaries GROUP BY 1, 2
		UNION SELECT project, '', '' FROM work_state_entries GROUP BY 1`)
	if err != nil {
		return err
	}
	cwds := map[string][]string{}
	originals := map[string][]string{}
	for rows.Next() {
		var p, merged, cwd string
		if err := rows.Scan(&p, &merged, &cwd); err != nil {
			rows.Close()
			return err
		}
		base := baseProject(p, merged)
		if cwd != "" {
			cwds[base] = append(cwds[base], cwd)
		}
		if p != "" && !slices.Contains(originals[base], p) {
			originals[base] = append(originals[base], p)
		}
		if _, ok := cwds[base]; !ok {
			cwds[base] = nil
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, base := range slices.Sorted(maps.Keys(cwds)) {
		info, ok := im.locate(base, cwds[base])
		key, name, root := "legacy:"+base, base, ""
		if ok {
			key, name, root = info.Key, info.Name, info.Root
			im.rep.GitProjects++
		} else {
			im.rep.LegacyProjects++
		}
		im.rep.Projects[base] = key
		if im.opt.DryRun {
			im.proj[base] = int64(len(im.proj) + 1)
			continue
		}
		id, err := im.dst.EnsureProject(ctx, key, name, root)
		if err != nil {
			return err
		}
		im.proj[base] = id
		if err := im.write(ctx, func(tx *sql.Tx) error {
			for _, o := range append(originals[base], base) {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO project_aliases(alias, project_id) VALUES (?, ?)`, "claude-mem:"+o, id); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// locate finds the git repository of a claude-mem project: explicit map, recorded cwds, then
// <root>/<name> under the search roots.
func (im *importer) locate(base string, cwds []string) (project.Info, bool) {
	var cands []string
	if p, ok := im.opt.ProjectMap[base]; ok {
		cands = append(cands, p)
	}
	cands = append(cands, cwds...)
	for _, r := range im.opt.SearchRoots {
		cands = append(cands, filepath.Join(r, base))
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err != nil || !st.IsDir() {
			continue
		}
		info := project.Resolve(c)
		if info.Git {
			return info, true
		}
	}
	return project.Info{}, false
}

func runtimeName(platform string) string {
	switch platform {
	case "claude", "":
		return "claude-code"
	}
	return platform
}

func (im *importer) sessions(ctx context.Context) error {
	rows, err := im.src.QueryContext(ctx, `SELECT content_session_id, coalesce(memory_session_id,''), coalesce(platform_source,''),
		coalesce(project,''), coalesce(cwd,''), coalesce(observed_model,''), started_at_epoch, coalesce(completed_at_epoch, 0)
		FROM sdk_sessions`)
	if err != nil {
		return err
	}
	type row struct {
		content, memory, platform, project, cwd, model string
		started, completed                             int64
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.content, &r.memory, &r.platform, &r.project, &r.cwd, &r.model, &r.started, &r.completed); err != nil {
			rows.Close()
			return err
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range all {
		info := sessionInfo{ID: runtimeName(r.platform) + ":" + r.content, Project: baseProject(r.project, "")}
		im.byContent[r.content] = info
		if r.memory != "" {
			im.byMemory[r.memory] = info
		}
	}
	if im.opt.DryRun {
		im.rep.Sessions = len(all)
		return nil
	}
	return im.write(ctx, func(tx *sql.Tx) error {
		for _, r := range all {
			rt := runtimeName(r.platform)
			info := im.byContent[r.content]
			var ended any
			if r.completed > 0 {
				ended = r.completed
			}
			res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO sessions(id, runtime, native_id, project_id, cwd, model, started_at, last_event_at, ended_at, summarized_at, origin)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, info.ID, rt, r.content, im.proj[info.Project], nullStr(r.cwd), nullStr(r.model),
				r.started, max(r.started, r.completed), ended, max(r.started, r.completed), origin)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				im.rep.Sessions++
			}
		}
		return nil
	})
}

// mapType folds claude-mem's open-ended types (100+ in practice) into amem's closed set.
func mapType(t string) (string, bool) {
	t = strings.ToLower(strings.TrimSpace(t))
	switch t {
	case "sensitive":
		return "discovery", true
	case "bugfix", "fix", "bug", "fix-applied", "problem-solution", "defect-found":
		return "bugfix", false
	case "decision":
		return "decision", false
	case "security_alert", "security_note", "security-alert", "gotcha":
		return "gotcha", false
	case "feature", "refactor", "change", "implementation", "code-edit", "code-change", "deploy", "deployment",
		"file-modification", "git-commit", "commit", "configuration", "integration", "improvement", "polish", "cleanup":
		return "change", false
	case "todo", "next-steps":
		return "todo", false
	}
	// The "gotcha" concept is not a type signal: claude-mem tags a quarter of all observations
	// with it. It stays in concepts, where search still sees it.
	return "discovery", false
}

func (im *importer) orphanSession(tx *sql.Tx, ctx context.Context, base string, ts int64) (string, error) {
	day := time.UnixMilli(ts).UTC().Format("2006-01-02")
	id := fmt.Sprintf("claude-mem:orphan:%s:%s", base, day)
	if _, ok := im.byMemory[id]; ok {
		return id, nil
	}
	im.byMemory[id] = sessionInfo{ID: id, Project: base}
	if tx == nil {
		im.rep.OrphanSessions++
		return id, nil
	}
	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO sessions(id, runtime, native_id, project_id, started_at, last_event_at, ended_at, summarized_at, origin)
		VALUES (?, 'claude-mem', ?, ?, ?, ?, ?, ?, ?)`, id, "orphan:"+base+":"+day, im.proj[base], ts, ts, ts, ts, origin)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		im.rep.OrphanSessions++
	}
	return id, nil
}

type obsRow struct {
	id                                              int64
	memSession, project, merged, typ, title, sub    string
	facts, narrative, concepts, filesRead, filesMod string
	created                                         int64
	model, agentType                                string
}

func (im *importer) observations(ctx context.Context) error {
	var last int64
	for {
		rows, err := im.src.QueryContext(ctx, `SELECT id, coalesce(memory_session_id,''), coalesce(project,''), coalesce(merged_into_project,''),
			coalesce(type,''), coalesce(title,''), coalesce(subtitle,''), coalesce(facts,'[]'), coalesce(narrative,''),
			coalesce(concepts,'[]'), coalesce(files_read,'[]'), coalesce(files_modified,'[]'), created_at_epoch,
			coalesce(generated_by_model,''), coalesce(agent_type,'')
			FROM observations WHERE id > ? ORDER BY id LIMIT 2000`, last)
		if err != nil {
			return err
		}
		var batch []obsRow
		for rows.Next() {
			var r obsRow
			if err := rows.Scan(&r.id, &r.memSession, &r.project, &r.merged, &r.typ, &r.title, &r.sub, &r.facts, &r.narrative,
				&r.concepts, &r.filesRead, &r.filesMod, &r.created, &r.model, &r.agentType); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		last = batch[len(batch)-1].id
		if im.opt.DryRun {
			for _, r := range batch {
				im.convert(ctx, nil, r)
			}
			continue
		}
		if err := im.dst.Write(ctx, func(tx *sql.Tx) error {
			for _, r := range batch {
				if err := im.convert(ctx, tx, r); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if im.rep.Memories%20000 < len(batch) {
			fmt.Fprintf(im.log, "  observations   %d imported\n", im.rep.Memories)
		}
	}
}

func (im *importer) convert(ctx context.Context, tx *sql.Tx, r obsRow) error {
	concepts := list(r.concepts)
	typ, private := mapType(r.typ)
	c := im.opt.Clean
	title := strings.TrimSpace(c(r.title))
	body := strings.TrimSpace(c(r.narrative))
	if body == "" {
		body = strings.TrimSpace(c(r.sub))
	}
	if title == "" {
		title = firstLine(body, 100)
	}
	if title == "" {
		im.rep.Skipped++
		return nil
	}
	base := baseProject(r.project, r.merged)
	sess, ok := im.byMemory[r.memSession]
	if !ok {
		im.rep.Orphans++
		id, err := im.orphanSession(tx, ctx, base, r.created)
		if err != nil {
			return err
		}
		sess = sessionInfo{ID: id}
	}
	files := slices.Compact(slices.Sorted(slices.Values(append(list(r.filesRead), list(r.filesMod)...))))
	meta := map[string]string{"orig_type": r.typ}
	if sub := c(r.sub); sub != "" && sub != body {
		meta["subtitle"] = sub
	}
	if r.model != "" {
		meta["model"] = r.model
	}
	if r.agentType != "" {
		meta["agent_type"] = r.agentType
	}
	if r.project != base {
		meta["orig_project"] = r.project
	}
	m := &store.Memory{
		ProjectID: im.proj[base], SessionID: sess.ID, Type: typ, Title: title, Body: body,
		Facts: cleanList(list(r.facts), c), Files: files, Concepts: concepts, Private: private,
		CreatedAt: r.created, Origin: origin, OriginID: fmt.Sprint(r.id), Meta: meta,
	}
	if tx == nil {
		im.rep.Memories++
		im.rep.Types[typ]++
		return nil
	}
	_, created, err := store.InsertMemoryTx(ctx, tx, m)
	if err != nil {
		return err
	}
	if created {
		im.rep.Memories++
		im.rep.Types[typ]++
	} else {
		im.rep.Duplicates++
	}
	return nil
}

func (im *importer) summaries(ctx context.Context) error {
	rows, err := im.src.QueryContext(ctx, `SELECT id, coalesce(memory_session_id,''), coalesce(project,''), coalesce(merged_into_project,''),
		coalesce(request,''), coalesce(investigated,''), coalesce(learned,''), coalesce(completed,''), coalesce(next_steps,''),
		coalesce(notes,''), created_at_epoch FROM session_summaries ORDER BY id`)
	if err != nil {
		return err
	}
	var all []store.Summary
	var bases []string
	var memSess []string
	for rows.Next() {
		var su store.Summary
		var id int64
		var ms, p, merged string
		if err := rows.Scan(&id, &ms, &p, &merged, &su.Request, &su.Investigated, &su.Learned, &su.Completed, &su.NextSteps, &su.Notes, &su.CreatedAt); err != nil {
			rows.Close()
			return err
		}
		su.Origin, su.OriginID = origin, fmt.Sprint(id)
		for _, f := range []*string{&su.Request, &su.Investigated, &su.Learned, &su.Completed, &su.NextSteps, &su.Notes} {
			*f = im.opt.Clean(*f)
		}
		all = append(all, su)
		bases = append(bases, baseProject(p, merged))
		memSess = append(memSess, ms)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if im.opt.DryRun {
		im.rep.Summaries = len(all)
		return nil
	}
	return im.write(ctx, func(tx *sql.Tx) error {
		for i := range all {
			su := &all[i]
			su.ProjectID = im.proj[bases[i]]
			if s, ok := im.byMemory[memSess[i]]; ok {
				su.SessionID = s.ID
			} else {
				id, err := im.orphanSession(tx, ctx, bases[i], su.CreatedAt)
				if err != nil {
					return err
				}
				su.SessionID = id
			}
			created, err := store.InsertSummaryTx(ctx, tx, su)
			if err != nil {
				return err
			}
			if created {
				im.rep.Summaries++
			}
		}
		return nil
	})
}

func (im *importer) prompts(ctx context.Context) error {
	rows, err := im.src.QueryContext(ctx, `SELECT id, content_session_id, prompt_number, prompt_text, created_at_epoch FROM user_prompts ORDER BY id`)
	if err != nil {
		return err
	}
	type pr struct {
		id      int64
		content string
		num     int
		text    string
		ts      int64
	}
	var all []pr
	for rows.Next() {
		var p pr
		if err := rows.Scan(&p.id, &p.content, &p.num, &p.text, &p.ts); err != nil {
			rows.Close()
			return err
		}
		all = append(all, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if im.opt.DryRun {
		im.rep.Prompts = len(all)
		return nil
	}
	return im.write(ctx, func(tx *sql.Tx) error {
		for _, p := range all {
			s, ok := im.byContent[p.content]
			if !ok {
				continue
			}
			var text, errMark any = im.opt.Clean(p.text), nil
			if redact.IsFullyPrivate(p.text) {
				text, errMark = nil, "private"
			}
			created, err := store.InsertReturning(ctx, tx, `INSERT INTO events(session_id, turn, kind, input, error, ts, processed, origin, origin_id)
				VALUES (?, ?, 'prompt', ?, ?, ?, 1, ?, ?) ON CONFLICT DO NOTHING`, s.ID, p.num, text, errMark, p.ts, origin+":prompt", fmt.Sprint(p.id))
			if err != nil {
				return err
			}
			if created {
				im.rep.Prompts++
			}
		}
		return nil
	})
}

func (im *importer) work(ctx context.Context) error {
	rows, err := im.src.QueryContext(ctx, `SELECT id, coalesce(project,''), list_name, fields, created_at_epoch FROM work_state_entries ORDER BY id`)
	if err != nil {
		return err
	}
	type we struct {
		id            int64
		project, list string
		fields        string
		ts            int64
	}
	var all []we
	for rows.Next() {
		var w we
		if err := rows.Scan(&w.id, &w.project, &w.list, &w.fields, &w.ts); err != nil {
			rows.Close()
			return err
		}
		all = append(all, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if im.opt.DryRun {
		im.rep.Work = len(all)
		return nil
	}
	return im.write(ctx, func(tx *sql.Tx) error {
		for _, w := range all {
			var f map[string]any
			if json.Unmarshal(redact.CleanJSON([]byte(w.fields), im.opt.Clean), &f) != nil {
				continue
			}
			created, err := store.AppendWorkTx(ctx, tx, im.proj[baseProject(w.project, "")], im.opt.Clean(w.list), f, w.ts, origin, fmt.Sprint(w.id))
			if err != nil {
				return err
			}
			if created {
				im.rep.Work++
			}
		}
		return nil
	})
}

func (im *importer) raw(ctx context.Context) error {
	if im.opt.RawSince == 0 || im.opt.DryRun {
		return nil
	}
	since := max(im.opt.RawSince, 0)
	// Tool uses of fully private turns are not imported at all.
	private := map[string]bool{}
	if rows, err := im.src.QueryContext(ctx, `SELECT content_session_id, prompt_number, prompt_text FROM user_prompts`); err == nil {
		for rows.Next() {
			var cs, text string
			var n int
			if rows.Scan(&cs, &n, &text) == nil && redact.IsFullyPrivate(text) {
				private[fmt.Sprint(cs, "/", n)] = true
			}
		}
		rows.Close()
	}
	var last int64
	for {
		rows, err := im.src.QueryContext(ctx, `SELECT id, content_session_id, tool_name, coalesce(tool_input,''), coalesce(tool_response,''),
			coalesce(prompt_number, 0), created_at_epoch FROM tool_uses WHERE id > ? AND created_at_epoch >= ? ORDER BY id LIMIT 5000`, last, since)
		if err != nil {
			return err
		}
		type tu struct {
			id                     int64
			content, tool, in, out string
			turn                   int
			ts                     int64
		}
		var batch []tu
		for rows.Next() {
			var t tu
			if err := rows.Scan(&t.id, &t.content, &t.tool, &t.in, &t.out, &t.turn, &t.ts); err != nil {
				rows.Close()
				return err
			}
			batch = append(batch, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}
		last = batch[len(batch)-1].id
		if err := im.dst.Write(ctx, func(tx *sql.Tx) error {
			for _, t := range batch {
				s, ok := im.byContent[t.content]
				if !ok || private[fmt.Sprint(t.content, "/", t.turn)] {
					continue
				}
				created, err := store.InsertReturning(ctx, tx, `INSERT INTO events(session_id, turn, kind, tool, input, output, ts, processed, origin, origin_id)
					VALUES (?, ?, 'tool_post', ?, ?, ?, ?, 1, ?, ?) ON CONFLICT DO NOTHING`,
					s.ID, t.turn, t.tool, cleanMaybeJSON(t.in, im.opt.Clean), cleanMaybeJSON(t.out, im.opt.Clean), t.ts, origin+":tool", fmt.Sprint(t.id))
				if err != nil {
					return err
				}
				if created {
					im.rep.Raw++
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
}

func list(s string) []string {
	var v []string
	if json.Unmarshal([]byte(s), &v) != nil {
		return nil
	}
	out := v[:0]
	for _, x := range v {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func cleanList(v []string, c func(string) string) []string {
	for i := range v {
		v[i] = c(v[i])
	}
	return v
}

// cleanMaybeJSON cleans JSON values string by string (escaped tags decode first) and plain
// text as a whole.
func cleanMaybeJSON(v string, c func(string) string) string {
	if t := strings.TrimSpace(v); (strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[")) && json.Valid([]byte(t)) {
		return string(redact.CleanJSON([]byte(v), c))
	}
	return c(v)
}
