package daemon

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tazhate/agent-mem/internal/event"
	"github.com/tazhate/agent-mem/internal/redact"
	"github.com/tazhate/agent-mem/internal/store"
)

// SearchHit is one row of a search answer: the index line plus enough to decide on fetching.
type SearchHit struct {
	ID      int64   `json:"id"`
	Type    string  `json:"type"`
	Project string  `json:"project,omitempty"`
	Title   string  `json:"title"`
	Date    string  `json:"date"`
	Tokens  int     `json:"tokens"`
	Score   float64 `json:"score"`
}

func (s *Server) scope(ctx context.Context, cwd, project string) (store.Scope, error) {
	switch {
	case project != "":
		ids, err := s.st.ProjectsByName(ctx, project)
		if err != nil {
			return store.Scope{}, err
		}
		if len(ids) == 0 {
			return store.Scope{}, fmt.Errorf("unknown project %q", project)
		}
		return store.Scope{ProjectID: ids[0]}, nil
	case cwd != "":
		id, _, err := s.projectFor(ctx, cwd)
		return store.Scope{ProjectID: id}, err
	}
	return store.Scope{All: true}, nil
}

func (s *Server) api(ctx context.Context, op string, raw json.RawMessage) (any, error) {
	switch op {
	case OpSearch:
		var p SearchParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		sc, err := s.scope(ctx, p.Cwd, p.Project)
		if err != nil {
			return nil, err
		}
		sc.Types = p.Types
		for _, t := range p.Types {
			if !store.ValidType(t) {
				return nil, fmt.Errorf("unknown type %q (valid: %s)", t, strings.Join(store.Types, ", "))
			}
		}
		limit := p.Limit
		if limit <= 0 {
			limit = 10
		}
		limit = min(limit, 50)
		res, err := s.rec.Search(ctx, p.Query, sc, limit)
		if err != nil {
			return nil, err
		}
		out := make([]SearchHit, 0, len(res))
		for _, r := range res {
			out = append(out, SearchHit{ID: r.Mem.ID, Type: r.Mem.Type, Project: r.Mem.Project, Title: r.Mem.Title,
				Date: time.UnixMilli(r.Mem.CreatedAt).Format("2006-01-02"), Tokens: r.Mem.Tokens(), Score: round2(r.Score)})
		}
		return out, nil
	case OpGet:
		var p GetParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if len(p.IDs) > 50 {
			p.IDs = p.IDs[:50]
		}
		get := s.st.GetMemories
		if p.IncludePrivate {
			get = s.st.GetMemoriesAll
		}
		mems, err := get(ctx, p.IDs)
		if err != nil {
			return nil, err
		}
		if p.Session != "" {
			_, _ = s.st.MarkUsed(ctx, p.Session, p.IDs)
		}
		return mems, nil
	case OpTimeline:
		var p TimelineParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		mems, err := s.st.Timeline(ctx, p.Anchor, min(cmp.Or(p.Before, 5), 50), min(cmp.Or(p.After, 5), 50))
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("memory #%d not found", p.Anchor)
		}
		return mems, err
	case OpSave:
		var p SaveParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if strings.TrimSpace(p.Text) == "" {
			return nil, errors.New("text is required")
		}
		if redact.IsFullyPrivate(p.Text) {
			return nil, errors.New("text is entirely <private>; nothing to save")
		}
		if p.Type != "" && !store.ValidType(p.Type) {
			return nil, fmt.Errorf("unknown type %q (valid: %s)", p.Type, strings.Join(store.Types, ", "))
		}
		m := &store.Memory{Type: p.Type, Title: s.red.Clean(p.Title), Body: s.red.Clean(p.Text), Files: p.Files, Origin: "manual"}
		if m.Type == "" {
			m.Type = "discovery"
			if p.Global {
				m.Type = "preference"
			}
		}
		if m.Title == "" {
			m.Title = firstLine(m.Body, 80)
		}
		if !p.Global {
			id, _, err := s.projectFor(ctx, p.Cwd)
			if err != nil {
				return nil, err
			}
			m.ProjectID = id
		}
		m.OriginID = fmt.Sprintf("%d", time.Now().UnixNano())
		id, created, err := s.st.InsertMemory(ctx, m)
		if err != nil {
			return nil, err
		}
		s.kickEmbed()
		return map[string]any{"id": id, "created": created}, nil
	case OpWorkW:
		var p WorkParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if p.List == "" || len(p.Fields) == 0 {
			return nil, errors.New("list and fields are required")
		}
		id, _, err := s.projectFor(ctx, p.Cwd)
		if err != nil {
			return nil, err
		}
		// Work state comes straight from MCP, so it gets the same cleaning as captured text.
		for k, v := range p.Fields {
			if str, ok := v.(string); ok {
				p.Fields[k] = s.red.Clean(str)
			}
		}
		if err := s.st.AppendWork(ctx, id, s.red.Clean(p.List), p.Fields); err != nil {
			return nil, err
		}
		return s.st.WorkLists(ctx, id, p.List, false)
	case OpWorkR:
		var p WorkParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		id, _, err := s.projectFor(ctx, p.Cwd)
		if err != nil {
			return nil, err
		}
		return s.st.WorkLists(ctx, id, p.List, p.IncludeClosed)
	case OpStats:
		st, err := s.st.Stats(ctx)
		return map[string]any{"version": Version, "stats": st, "recall_mode": s.cfg.Recall.Mode,
			"embed": s.cfg.Embed.Kind, "observer": s.cfg.Observer.Enabled}, err
	case OpShadow:
		return s.st.ShadowLog(ctx, time.Now().Add(-7*24*time.Hour).UnixMilli(), 200)
	}
	return nil, fmt.Errorf("unknown op %q", op)
}

func (s *Server) kickEmbed() {
	for _, w := range s.workers {
		w.Kick("", event.Kind("memory_saved"))
	}
}

func firstLine(s string, n int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// CallLocal runs an API op in process (used by the daemon's HTTP MCP endpoint).
func (s *Server) CallLocal(ctx context.Context, op string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	s.touch()
	data, err := s.api(ctx, op, raw)
	if err != nil || out == nil {
		return err
	}
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
