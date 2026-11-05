// Package observer turns raw captured events into memories. It batches per turn instead of per
// tool call, filters noise before the LLM sees anything, asks for JSON by schema, and lets the
// model replace outdated memories instead of piling up new ones.
package observer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"agent-mem/internal/config"
	"agent-mem/internal/event"
	"agent-mem/internal/provider"
	"agent-mem/internal/recall"
	"agent-mem/internal/store"
)

type kick struct {
	session string
	kind    event.Kind
}

type Observer struct {
	st      *store.Store
	cfg     config.ObserverConfig
	rec     *recall.Engine
	log     *slog.Logger
	kicks   chan kick
	prov    provider.Provider
	provMu  sync.Mutex
	provErr time.Time

	backoff map[string]time.Time
	fails   map[string]int
}

func New(st *store.Store, cfg config.ObserverConfig, rec *recall.Engine, log *slog.Logger) *Observer {
	return &Observer{st: st, cfg: cfg, rec: rec, log: log, kicks: make(chan kick, 256),
		backoff: map[string]time.Time{}, fails: map[string]int{}}
}

// SetProvider overrides provider construction (tests, `amem observe --dry-run`).
func (o *Observer) SetProvider(p provider.Provider) { o.prov = p }

func (o *Observer) Kick(session string, kind event.Kind) {
	select {
	case o.kicks <- kick{session, kind}:
	default: // the periodic scan will pick it up
	}
}

func (o *Observer) Run(ctx context.Context) {
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case k := <-o.kicks:
			switch k.kind {
			case event.TurnEnd, event.SubagentStop, event.Compact:
				o.process(ctx, k.session, false)
			case event.SessionEnd:
				o.process(ctx, k.session, true)
				o.summarize(ctx, k.session)
			}
		case <-t.C:
			o.scan(ctx)
		}
	}
}

func (o *Observer) provider(ctx context.Context) (provider.Provider, error) {
	o.provMu.Lock()
	defer o.provMu.Unlock()
	if o.prov != nil {
		return o.prov, nil
	}
	if time.Since(o.provErr) < 5*time.Minute {
		return nil, fmt.Errorf("observer provider unavailable")
	}
	ch, err := provider.NewChain(ctx, o.cfg.Providers)
	if err != nil {
		o.provErr = time.Now()
		o.log.Warn("observer has no provider; events stay queued", "err", err)
		return nil, err
	}
	o.prov = ch
	o.log.Info("observer provider ready", "chain", ch.Name())
	return ch, nil
}

// scan flushes sessions that went idle or grew large, and summarizes finished sessions.
func (o *Observer) scan(ctx context.Context) {
	ids, err := o.st.SessionsWithPending(ctx)
	if err != nil {
		o.log.Warn("observer scan", "err", err)
		return
	}
	for _, sid := range ids {
		if ctx.Err() != nil {
			return
		}
		o.process(ctx, sid, false)
	}
	idle := time.Now().Add(-15 * time.Minute).UnixMilli()
	done, err := o.st.UnsummarizedSessions(ctx, idle, 3)
	if err != nil {
		o.log.Warn("observer summary scan", "err", err)
		return
	}
	for _, sid := range done {
		o.summarize(ctx, sid)
	}
}

// process sends complete turns of a session to the LLM. force also takes an unfinished turn.
func (o *Observer) process(ctx context.Context, sid string, force bool) {
	if !o.cfg.Enabled || time.Now().Before(o.backoff[sid]) {
		return
	}
	evs, err := o.st.PendingEvents(ctx, sid, 2000)
	if err != nil || len(evs) == 0 {
		return
	}
	idle := time.Since(time.UnixMilli(evs[len(evs)-1].TS)) > o.cfg.IdleFlush.Duration
	cut := lastTurnEnd(evs)
	switch {
	case force || idle:
		cut = len(evs)
	case cut == 0 && (len(evs) >= o.cfg.BatchEvents || size(evs) >= o.cfg.BatchChars):
		cut = len(evs) // a long turn: do not wait for its end
	}
	if cut == 0 {
		return
	}
	batch := evs[:cut]
	ids := make([]int64, len(batch))
	for i, e := range batch {
		ids[i] = e.ID
	}
	turns := buildTurns(batch)
	if len(turns) == 0 {
		_ = o.st.MarkProcessed(ctx, ids)
		return
	}
	sess, err := o.st.GetSession(ctx, sid)
	if err != nil {
		o.log.Warn("observer session", "session", sid, "err", err)
		return
	}
	prov, err := o.provider(ctx)
	if err != nil {
		return
	}
	for _, chunk := range chunks(turns, o.cfg.BatchChars) {
		if err := o.observe(ctx, prov, sess, chunk); err != nil {
			o.fails[sid]++
			wait := min(time.Duration(1<<min(o.fails[sid], 6))*30*time.Second, 30*time.Minute)
			o.backoff[sid] = time.Now().Add(wait)
			o.log.Warn("observer failed; will retry", "session", sid, "retry_in", wait, "err", err)
			return
		}
	}
	delete(o.fails, sid)
	delete(o.backoff, sid)
	if err := o.st.MarkProcessed(ctx, ids); err != nil {
		o.log.Warn("mark processed", "err", err)
	}
}

func (o *Observer) observe(ctx context.Context, prov provider.Provider, sess store.Session, turns []turn) error {
	proj, _ := o.st.ProjectByID(ctx, sess.ProjectID)
	existing := o.related(ctx, sess.ProjectID, turns)
	user := renderBatch(proj.Name, turns, existing)
	ctx, cancel := context.WithTimeout(ctx, o.cfg.Timeout.Duration)
	defer cancel()
	start := time.Now()
	raw, err := prov.Complete(ctx, provider.Request{
		System: systemPrompt(o.cfg.Language), User: user, Schema: memorySchema, SchemaName: "memories", MaxTokens: 4096,
	})
	if err != nil {
		return err
	}
	var out struct {
		Memories []draft `json:"memories"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("decode observer output: %w", err)
	}
	shown := map[int64]bool{}
	for _, m := range existing {
		shown[m.ID] = true
	}
	stored := 0
	for _, d := range out.Memories {
		m := d.memory(sess, turns[len(turns)-1].n)
		if m == nil {
			continue
		}
		id, created, err := o.st.InsertMemory(ctx, m)
		if err != nil {
			return err
		}
		if !created {
			continue
		}
		stored++
		var old []int64
		for _, r := range d.Replaces {
			if shown[r] {
				old = append(old, r)
			}
		}
		if len(old) > 0 {
			if err := o.st.Supersede(ctx, id, old); err != nil {
				return err
			}
		}
	}
	o.log.Info("observed", "session", sess.ID, "turns", len(turns), "chars", len(user), "memories", stored,
		"took", time.Since(start).Round(time.Millisecond))
	return nil
}

// related finds existing memories the batch may update: same files, then text similarity.
func (o *Observer) related(ctx context.Context, projectID int64, turns []turn) []*store.Memory {
	var files []string
	var text strings.Builder
	for _, t := range turns {
		text.WriteString(t.prompt + " ")
		files = append(files, t.files...)
	}
	sc := store.Scope{ProjectID: projectID}
	out, _ := o.st.ByFiles(ctx, uniq(files), sc, 5)
	if hits, err := o.rec.Search(ctx, text.String(), sc, 5); err == nil {
		for _, h := range hits {
			if len(out) >= 8 {
				break
			}
			if !slices.ContainsFunc(out, func(m *store.Memory) bool { return m.ID == h.Mem.ID }) && h.Rel > 0.2 {
				out = append(out, h.Mem)
			}
		}
	}
	return out
}

func (o *Observer) summarize(ctx context.Context, sid string) {
	if !o.cfg.Enabled {
		return
	}
	prompts, err := o.st.SessionPrompts(ctx, sid, 100)
	if err != nil || len(prompts) == 0 {
		_ = o.st.MarkSummarized(ctx, sid)
		return
	}
	sess, err := o.st.GetSession(ctx, sid)
	if err != nil {
		return
	}
	mems, _ := o.st.SessionMemories(ctx, sid, 40)
	last, _ := o.st.LastAssistant(ctx, sid)
	prov, err := o.provider(ctx)
	if err != nil {
		return
	}
	var b strings.Builder
	b.WriteString("USER REQUESTS IN THIS SESSION:\n")
	for _, p := range prompts {
		b.WriteString("- " + clip(oneLine(p), 400) + "\n")
	}
	if len(mems) > 0 {
		b.WriteString("\nMEMORIES RECORDED:\n")
		for _, m := range slices.Backward(mems) {
			b.WriteString("- " + m.Type + ": " + m.Title + "\n")
		}
	}
	if last != "" {
		b.WriteString("\nLAST ASSISTANT MESSAGE:\n" + clip(last, 3000) + "\n")
	}
	ctx, cancel := context.WithTimeout(ctx, o.cfg.Timeout.Duration)
	defer cancel()
	raw, err := prov.Complete(ctx, provider.Request{System: summaryPrompt(o.cfg.Language), User: b.String(), Schema: summarySchema, SchemaName: "summary", MaxTokens: 2048})
	if err != nil {
		o.log.Warn("summary failed", "session", sid, "err", err)
		return
	}
	var su store.Summary
	if err := json.Unmarshal(raw, &su); err != nil {
		o.log.Warn("summary decode", "err", err)
		return
	}
	su.SessionID, su.ProjectID, su.ID = sid, sess.ProjectID, 0
	if err := o.st.InsertSummary(ctx, &su); err != nil {
		o.log.Warn("summary store", "err", err)
		return
	}
	o.log.Info("summarized", "session", sid)
}

type draft struct {
	Type     string   `json:"type"`
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Facts    []string `json:"facts"`
	Files    []string `json:"files"`
	Global   bool     `json:"global"`
	Replaces []int64  `json:"replaces"`
}

func (d draft) memory(sess store.Session, turn int) *store.Memory {
	title := clip(oneLine(d.Title), 120)
	if title == "" {
		return nil
	}
	typ := strings.ToLower(strings.TrimSpace(d.Type))
	if !store.ValidType(typ) {
		typ = "discovery"
	}
	m := &store.Memory{
		ProjectID: sess.ProjectID, SessionID: sess.ID, Turn: turn, Type: typ, Title: title,
		Body: clip(strings.TrimSpace(d.Body), 1500), Facts: clipList(d.Facts, 10, 300), Files: clipList(d.Files, 20, 400),
		Origin: "observer",
	}
	if d.Global && typ == "preference" {
		m.ProjectID = 0
	}
	return m
}

var memorySchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"memories"},
	"properties": map[string]any{
		"memories": map[string]any{
			"type": "array",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"type", "title", "body", "facts", "files", "global", "replaces"},
				"properties": map[string]any{
					"type":     map[string]any{"type": "string", "enum": store.Types},
					"title":    map[string]any{"type": "string"},
					"body":     map[string]any{"type": "string"},
					"facts":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"files":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"global":   map[string]any{"type": "boolean"},
					"replaces": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
				},
			},
		},
	},
}

var summarySchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"request", "investigated", "learned", "completed", "next_steps"},
	"properties": map[string]any{
		"request":      map[string]any{"type": "string"},
		"investigated": map[string]any{"type": "string"},
		"learned":      map[string]any{"type": "string"},
		"completed":    map[string]any{"type": "string"},
		"next_steps":   map[string]any{"type": "string"},
	},
}

func systemPrompt(lang string) string {
	return `You maintain long-term memory for an AI coding agent. You read what the agent and the user did in one or more turns of a work session and record what future sessions should know. You never talk to the agent; you only return JSON.

Memory types:
- decision: a choice and its reason ("use X because Y; rejected Z because W")
- bugfix: what broke, the root cause, the fix
- gotcha: a trap, surprising behavior, environment quirk or constraint that will bite again
- change: what was built, changed, deployed or configured, and where
- discovery: how something works (architecture, data flow, where things live) that took effort to learn
- preference: how the user wants things done (style, tools, process); set global=true when it applies beyond this project
- todo: work explicitly left for later

Rules:
- Record durable facts, not narration. Good: "SessionStart hook needs timeout >= 60s: npm install there takes ~40s". Bad: "The agent analyzed the hook configuration".
- Skip noise: routine reads and listings, status checks, attempts that taught nothing, anything already covered by EXISTING MEMORIES.
- One memory per distinct insight, 0-5 per batch. An empty list is a good answer for routine work.
- title: at most 80 characters, specific and searchable (names, error strings, file names, versions).
- body: 1-4 sentences that include the why.
- facts: short atomic statements with concrete values (paths, versions, numbers, commands, error text).
- files: the paths the memory is about.
- If an EXISTING MEMORY is now wrong or outdated, put its id into "replaces" of the memory that supersedes it.
- Never copy secrets, tokens or passwords.
- Write titles, bodies and facts in ` + lang + `. Keep code, paths, commands and error messages verbatim.`
}

func summaryPrompt(lang string) string {
	return `You write a short handoff summary of one AI coding session for the next session. Fields:
request: what the user wanted (1 sentence); investigated: what was examined; learned: key findings;
completed: what was actually done; next_steps: what remains, or empty. Each field at most 3 sentences,
concrete (names, files, results). Write in ` + lang + `.`
}

func uniq(s []string) []string {
	slices.Sort(s)
	return slices.Compact(s)
}

func clipList(s []string, n, each int) []string {
	var out []string
	for _, v := range s {
		if v = strings.TrimSpace(v); v != "" && len(out) < n {
			out = append(out, clip(v, each))
		}
	}
	return out
}
