// Package recall decides what memory to inject at each hook: it searches locally (no LLM, no
// network), scores by relevance, type, age and proven usefulness, and respects per-session
// dedupe and token budgets.
package recall

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"agent-mem/internal/config"
	"agent-mem/internal/event"
	"agent-mem/internal/stem"
	"agent-mem/internal/store"
)

// VectorSearcher is implemented by the embedding index when one is configured.
type VectorSearcher interface {
	Search(ctx context.Context, text string, scope store.Scope, k int) ([]VecHit, error)
}

type VecHit struct {
	ID  int64
	Sim float32
}

type Engine struct {
	st  *store.Store
	cfg config.RecallConfig
	vec VectorSearcher
}

func New(st *store.Store, cfg config.RecallConfig, vec VectorSearcher) *Engine {
	return &Engine{st: st, cfg: cfg, vec: vec}
}

func (e *Engine) SetVectors(v VectorSearcher) { e.vec = v }

type Request struct {
	Event       *event.Event
	SessionID   string
	ProjectID   int64
	ProjectName string
}

type Result struct {
	Trigger string
	Text    string
	Items   []store.Injection
}

// Scored is a ranked candidate.
type Scored struct {
	Mem   *store.Memory
	Rel   float64
	Score float64
}

// Recall returns what to inject for the event, or an empty result.
func (e *Engine) Recall(ctx context.Context, req Request) (Result, error) {
	ev := req.Event
	seen, spent, err := e.st.Injected(ctx, req.SessionID)
	if err != nil {
		return Result{}, err
	}
	// Past the session budget only clearly relevant memories get through.
	strict := 1.0
	if spent >= e.cfg.SessionBudgetTokens {
		strict = 1.5
	}
	scope := store.Scope{ProjectID: req.ProjectID}
	switch ev.Kind {
	case event.SessionStart:
		if ev.Source == "compact" || ev.Source == "resume" {
			return e.sessionState(ctx, req)
		}
		return e.sessionStart(ctx, req, seen)
	case event.Compact:
		return e.sessionState(ctx, req)
	case event.Prompt:
		if len([]rune(ev.Prompt)) < e.cfg.MinPromptChars {
			return Result{}, nil
		}
		hits, err := e.Search(ctx, ev.Prompt, scope, e.cfg.PromptLimit*4)
		if err != nil {
			return Result{}, err
		}
		picked := pick(hits, seen, e.cfg.MinScore*strict, e.cfg.PromptLimit)
		return render("prompt", "Relevant memory for this request", picked), nil
	case event.ToolPre:
		paths := FilePaths(ev.Tool)
		if len(paths) == 0 {
			return Result{}, nil
		}
		mems, err := e.st.ByFiles(ctx, paths, scope, 20)
		if err != nil {
			return Result{}, err
		}
		var picked []Scored
		for _, m := range mems {
			if seen[m.ID] {
				continue
			}
			picked = append(picked, Scored{Mem: m, Rel: 1, Score: typeWeight(m.Type) * recency(m) * usage(m)})
		}
		slices.SortFunc(picked, func(a, b Scored) int { return cmp.Compare(b.Score, a.Score) })
		picked = picked[:min(len(picked), e.cfg.FileLimit)]
		return render("file", "Memory about "+shortPath(paths[0]), picked), nil
	case event.ToolPost, event.ToolFail:
		if !ev.Tool.Failed() && ev.Kind != event.ToolFail {
			return Result{}, nil
		}
		q := errorQuery(ev.Tool)
		if q == "" {
			return Result{}, nil
		}
		sc := scope
		sc.Types = []string{"bugfix", "gotcha", "decision"}
		hits, err := e.Search(ctx, q, sc, 20)
		if err != nil {
			return Result{}, err
		}
		picked := pick(hits, seen, (e.cfg.MinScore+0.1)*strict, e.cfg.ErrorLimit)
		return render("error", "This failure looks like something seen before", picked), nil
	case event.SubagentStart:
		if ev.Prompt == "" {
			return Result{}, nil
		}
		hits, err := e.Search(ctx, ev.Prompt, scope, 12)
		if err != nil {
			return Result{}, err
		}
		return render("subagent", "Relevant memory for this task", pick(hits, seen, e.cfg.MinScore*strict, 3)), nil
	}
	return Result{}, nil
}

// Search ranks memories for free text: FTS5 candidates (plus vector hits when configured),
// relevance as IDF-weighted coverage of query stems, then type, recency and usage modifiers.
func (e *Engine) Search(ctx context.Context, text string, scope store.Scope, limit int) ([]Scored, error) {
	qstems := stem.Query(text)
	if len(qstems) == 0 {
		return nil, nil
	}
	df, total, err := e.st.DocFreq(ctx, qstems)
	if err != nil {
		return nil, err
	}
	idf := func(s string) float64 {
		return math.Log(1 + float64(total+1)/float64(df[s]+1))
	}
	// Long prompts: keep the most specific stems only.
	if len(qstems) > 24 {
		slices.SortFunc(qstems, func(a, b string) int { return cmp.Compare(idf(b), idf(a)) })
		qstems = qstems[:24]
	}
	var idfSum float64
	for _, s := range qstems {
		idfSum += idf(s)
	}
	candLimit := max(limit*3, 30)
	hits, err := e.st.SearchFTS(ctx, qstems, scope, candLimit)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(hits))
	for _, h := range hits {
		ids = append(ids, h.ID)
	}
	vecSim := map[int64]float32{}
	if e.vec != nil {
		if vh, err := e.vec.Search(ctx, text, scope, candLimit); err == nil {
			for _, h := range vh {
				vecSim[h.ID] = h.Sim
				if !slices.Contains(ids, h.ID) {
					ids = append(ids, h.ID)
				}
			}
		}
	}
	mems, err := e.st.GetMemories(ctx, ids)
	if err != nil {
		return nil, err
	}
	out := make([]Scored, 0, len(mems))
	for _, m := range mems {
		have := map[string]bool{}
		for _, t := range stem.Tokens(m.Title + " " + m.Body + " " + strings.Join(m.Facts, " ") + " " + strings.Join(m.Files, " ") + " " + m.Type) {
			have[stem.Word(t)] = true
		}
		var got float64
		matched := 0
		for _, s := range qstems {
			if have[s] {
				got += idf(s)
				matched++
			}
		}
		rel := got / idfSum
		// A single shared word in a multi-word query is a coincidence, not relevance.
		if len(qstems) >= 3 && matched < 2 {
			rel *= 0.4
		}
		if sim, ok := vecSim[m.ID]; ok {
			rel = max(rel, clamp01((float64(sim)-0.45)*2))
		}
		out = append(out, Scored{Mem: m, Rel: rel, Score: rel * typeWeight(m.Type) * recency(m) * usage(m)})
	}
	slices.SortFunc(out, func(a, b Scored) int { return cmp.Compare(b.Score, a.Score) })
	return out[:min(len(out), limit)], nil
}

func pick(hits []Scored, seen map[int64]bool, minScore float64, limit int) []Scored {
	var out []Scored
	for _, h := range hits {
		if len(out) >= limit {
			break
		}
		if seen[h.Mem.ID] || h.Score < minScore {
			continue
		}
		out = append(out, h)
	}
	return out
}

func (e *Engine) sessionStart(ctx context.Context, req Request, seen map[int64]bool) (Result, error) {
	var b strings.Builder
	var items []store.Injection
	budget := e.cfg.StartTokens * 4 // chars
	fmt.Fprintf(&b, "# amem memory: %s\n", req.ProjectName)

	if lists, err := e.st.WorkLists(ctx, req.ProjectID, "", false); err == nil && len(lists) > 0 {
		b.WriteString("Open work:\n")
		for _, l := range lists {
			line := "- " + l.List
			if st := compactFields(l.State); st != "" {
				line += ": " + st
			}
			b.WriteString(clip(line, 200) + "\n")
			for _, it := range l.Items {
				b.WriteString(clip("  - "+it.Task+" "+compactFields(it.Fields), 160) + "\n")
			}
			if b.Len() > budget/2 {
				break
			}
		}
	}
	if su, err := e.st.LatestSummary(ctx, req.ProjectID, req.SessionID); err == nil && su != nil {
		fmt.Fprintf(&b, "Last session (%s): %s", time.UnixMilli(su.CreatedAt).Format("2006-01-02"), clip(oneLine(su.Request), 160))
		if su.Completed != "" {
			b.WriteString(" | done: " + clip(oneLine(su.Completed), 200))
		}
		if su.NextSteps != "" {
			b.WriteString(" | next: " + clip(oneLine(su.NextSteps), 200))
		}
		b.WriteString("\n")
	}
	durable, err := e.st.Durable(ctx, store.Scope{ProjectID: req.ProjectID}, 30)
	if err != nil {
		return Result{}, err
	}
	recent, err := e.st.Recent(ctx, store.Scope{ProjectID: req.ProjectID}, 10)
	if err != nil {
		return Result{}, err
	}
	header := false
	for _, m := range append(durable, recent...) {
		if seen[m.ID] || slices.ContainsFunc(items, func(i store.Injection) bool { return i.MemoryID == m.ID }) {
			continue
		}
		line := Line(m)
		if b.Len()+len(line) > budget {
			break
		}
		if !header {
			b.WriteString("Key memories:\n")
			header = true
		}
		b.WriteString(line + "\n")
		items = append(items, store.Injection{MemoryID: m.ID, Tokens: len(line) / 4})
	}
	if b.Len() < 40 {
		return Result{}, nil
	}
	b.WriteString("Search: memory_search(query) · Full records: memory_get(ids)\n")
	return Result{Trigger: "session_start", Text: wrap(b.String()), Items: items}, nil
}

// sessionState re-injects what the current session already learned (after compaction or resume).
func (e *Engine) sessionState(ctx context.Context, req Request) (Result, error) {
	mems, err := e.st.SessionMemories(ctx, req.SessionID, 20)
	if err != nil {
		return Result{}, err
	}
	prompts, _ := e.st.SessionPrompts(ctx, req.SessionID, 50)
	var b strings.Builder
	budget := e.cfg.CompactTokens * 4
	b.WriteString("# amem: state of this session before compaction\n")
	if n := len(prompts); n > 0 {
		b.WriteString("Requests so far:\n")
		for _, p := range prompts[max(0, n-4):] {
			b.WriteString("- " + clip(oneLine(p), 200) + "\n")
		}
	}
	var items []store.Injection
	if len(mems) > 0 {
		b.WriteString("Recorded in this session:\n")
	}
	for _, m := range mems {
		line := Line(m)
		if b.Len()+len(line) > budget {
			break
		}
		b.WriteString(line + "\n")
		items = append(items, store.Injection{MemoryID: m.ID, Tokens: len(line) / 4})
	}
	if lists, err := e.st.WorkLists(ctx, req.ProjectID, "", false); err == nil {
		for _, l := range lists {
			for _, it := range l.Items {
				line := clip("- todo "+l.List+": "+it.Task+" "+compactFields(it.Fields), 160)
				if b.Len()+len(line) > budget {
					break
				}
				b.WriteString(line + "\n")
			}
		}
	}
	if len(prompts) == 0 && len(mems) == 0 {
		return Result{}, nil
	}
	return Result{Trigger: "compact", Text: wrap(b.String()), Items: items}, nil
}

func render(trigger, title string, picked []Scored) Result {
	if len(picked) == 0 {
		return Result{}
	}
	var b strings.Builder
	b.WriteString("[amem] " + title + " (full record: memory_get ids):\n")
	items := make([]store.Injection, 0, len(picked))
	for _, p := range picked {
		line := Line(p.Mem)
		b.WriteString(line + "\n")
		items = append(items, store.Injection{MemoryID: p.Mem.ID, Tokens: len(line) / 4})
	}
	return Result{Trigger: trigger, Text: wrap(b.String()), Items: items}
}

// Line is the index format: id, type, title, and what the full record costs.
func Line(m *store.Memory) string {
	scope := ""
	if m.ProjectID == 0 {
		scope = " (global)"
	}
	return fmt.Sprintf("#%d %s%s: %s (~%dt)", m.ID, m.Type, scope, clip(oneLine(m.Title), 140), m.Tokens())
}

func wrap(s string) string {
	return "<amem-context>\n" + strings.TrimRight(s, "\n") + "\n</amem-context>"
}

func typeWeight(t string) float64 {
	switch t {
	case "gotcha":
		return 1.25
	case "decision", "preference":
		return 1.2
	case "bugfix":
		return 1.1
	case "change":
		return 0.9
	case "discovery":
		return 0.85
	}
	return 1
}

// recency decays situational memories (half-life 30 days, floor 0.5); durable ones never decay.
func recency(m *store.Memory) float64 {
	switch m.Type {
	case "decision", "gotcha", "preference":
		return 1
	}
	days := float64(time.Now().UnixMilli()-m.CreatedAt) / 86_400_000
	return 0.5 + 0.5*math.Exp(-days*math.Ln2/30)
}

// usage rewards memories that proved useful and penalizes ones shown often but never used.
func usage(m *store.Memory) float64 {
	bonus := 1 + 0.15*math.Log1p(float64(m.UseCount))
	waste := max(0, m.InjectCount-3*m.UseCount)
	return max(0.5, bonus/(1+0.08*float64(waste)))
}

// FilePaths extracts file paths from a tool call input (Read/Edit/Write and friends).
func FilePaths(t *event.Tool) []string {
	if t == nil || len(t.Input) == 0 {
		return nil
	}
	var in map[string]json.RawMessage
	if json.Unmarshal(t.Input, &in) != nil {
		return nil
	}
	var out []string
	for _, k := range []string{"file_path", "path", "notebook_path", "filePath", "absolute_path"} {
		var s string
		if v, ok := in[k]; ok && json.Unmarshal(v, &s) == nil && s != "" && strings.ContainsAny(s, "/.") {
			out = append(out, s)
		}
	}
	var list []string
	if v, ok := in["filePaths"]; ok && json.Unmarshal(v, &list) == nil {
		out = append(out, list...)
	}
	return out
}

func errorQuery(t *event.Tool) string {
	if t == nil {
		return ""
	}
	var cmd struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(t.Input, &cmd)
	out := t.Output
	if len(out) > 600 {
		out = out[len(out)-600:]
	}
	return strings.TrimSpace(t.Error + " " + out + " " + clip(cmd.Command, 200))
}

func compactFields(m map[string]any) string {
	var parts []string
	for _, k := range slices.Sorted(func(yield func(string) bool) {
		for k := range m {
			if k != "task" && !yield(k) {
				return
			}
		}
	}) {
		parts = append(parts, fmt.Sprintf("%s=%v", k, m[k]))
	}
	return strings.Join(parts, " ")
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func shortPath(p string) string {
	parts := strings.Split(p, "/")
	if len(parts) > 2 {
		return strings.Join(parts[len(parts)-2:], "/")
	}
	return p
}

func clamp01(v float64) float64 { return max(0, min(1, v)) }
