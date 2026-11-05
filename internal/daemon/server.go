package daemon

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/tazhate/agent-mem/internal/adapter"
	"github.com/tazhate/agent-mem/internal/config"
	"github.com/tazhate/agent-mem/internal/event"
	"github.com/tazhate/agent-mem/internal/project"
	"github.com/tazhate/agent-mem/internal/recall"
	"github.com/tazhate/agent-mem/internal/redact"
	"github.com/tazhate/agent-mem/internal/store"
)

var ErrRunning = errors.New("daemon already running")

// Worker is background work driven by the daemon (observer, embedder). Kick signals new input.
type Worker interface {
	Run(ctx context.Context)
	Kick(sessionID string, kind event.Kind)
}

type Server struct {
	cfg     *config.Config
	st      *store.Store
	rec     *recall.Engine
	proj    *project.Resolver
	red     *redact.Redactor
	log     *slog.Logger
	workers []Worker
	// HTTP serves extra endpoints (streamable-HTTP MCP) on cfg.Daemon.HTTPAddr when set.
	HTTP http.Handler

	lastActivity atomic.Int64
	restart      atomic.Bool
	cancel       context.CancelFunc

	mu       sync.Mutex
	projects map[string]int64 // project key -> id
	adopted  map[int64]bool
}

// New prepares a server; callers attach workers before Run.
func New(cfg *config.Config, st *store.Store, log *slog.Logger) *Server {
	return &Server{
		cfg:      cfg,
		st:       st,
		rec:      recall.New(st, cfg.Recall, nil),
		proj:     project.NewResolver(),
		red:      redact.New(cfg.Capture.RedactGeneric),
		log:      log,
		projects: map[string]int64{},
		adopted:  map[int64]bool{},
	}
}

func (s *Server) Recall() *recall.Engine  { return s.rec }
func (s *Server) Store() *store.Store     { return s.st }
func (s *Server) AddWorker(w Worker)      { s.workers = append(s.workers, w) }
func (s *Server) LastActivity() time.Time { return time.UnixMilli(s.lastActivity.Load()) }

// Lock takes the single-instance lock; the returned file must stay open while the daemon runs.
func Lock(cfg *config.Config) (*os.File, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(cfg.LockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, ErrRunning
	}
	_ = f.Truncate(0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return f, nil
}

// Run serves until ctx ends, the idle timeout fires, or a newer binary asks for a restart.
func (s *Server) Run(ctx context.Context, foreground bool) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.cancel = cancel
	s.touch()

	sock := s.cfg.SocketPath()
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	_ = os.Chmod(sock, 0o600)
	defer os.Remove(sock)
	context.AfterFunc(ctx, func() { ln.Close() })

	s.drainSpool(ctx)

	var wg sync.WaitGroup
	for _, w := range s.workers {
		wg.Go(func() { w.Run(ctx) })
	}
	wg.Go(func() { s.janitor(ctx) })
	wg.Go(func() {
		// Hooks spool when they cannot reach us (startup races, restarts); pick that up quickly.
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			if SpoolPending(s.cfg.SpoolPath()) {
				s.drainSpool(ctx)
			}
		}
	})
	if !foreground && s.cfg.Daemon.IdleExit.Duration > 0 {
		wg.Go(func() { s.idleExit(ctx) })
	}
	if s.HTTP != nil && s.cfg.Daemon.HTTPAddr != "" {
		hs := &http.Server{Addr: s.cfg.Daemon.HTTPAddr, Handler: s.HTTP, ReadHeaderTimeout: 10 * time.Second}
		context.AfterFunc(ctx, func() { hs.Close() })
		wg.Go(func() {
			if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.log.Error("http", "err", err)
			}
		})
	}
	s.log.Info("amemd listening", "socket", sock, "http", s.cfg.Daemon.HTTPAddr, "version", Version, "pid", os.Getpid())

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			s.log.Error("accept", "err", err)
			time.Sleep(50 * time.Millisecond)
			continue
		}
		go s.serve(ctx, conn)
	}
	cancel()
	wg.Wait()
	return nil
}

func (s *Server) drainSpool(ctx context.Context) {
	n, err := SpoolDrain(s.cfg.SpoolPath(), func(ev *event.Event) error {
		_, _, err := s.ingest(ctx, ev, false)
		return err
	})
	if err != nil {
		s.log.Error("spool drain", "err", err)
	} else if n > 0 {
		s.log.Info("spool drained", "events", n)
	}
}

func (s *Server) touch() { s.lastActivity.Store(time.Now().UnixMilli()) }

func (s *Server) idleExit(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if time.Since(s.LastActivity()) > s.cfg.Daemon.IdleExit.Duration {
			s.log.Info("idle exit")
			s.cancel()
			return
		}
	}
}

func (s *Server) janitor(ctx context.Context) {
	if need, err := s.st.NeedsReindex(ctx); err == nil && need {
		start := time.Now()
		if n, err := s.st.Reindex(ctx); err != nil {
			s.log.Error("fts reindex", "err", err)
		} else {
			s.log.Info("fts reindexed for new tokenizer rules", "memories", n, "took", time.Since(start).Round(time.Millisecond))
		}
	}
	t := time.NewTicker(6 * time.Hour)
	defer t.Stop()
	for {
		if err := s.st.MergeFTS(ctx); err != nil {
			s.log.Warn("fts merge", "err", err)
		}
		if ret := s.cfg.Capture.RawRetention.Duration; ret > 0 {
			if n, err := s.st.PruneEvents(ctx, time.Now().Add(-ret).UnixMilli()); err == nil && n > 0 {
				s.log.Info("pruned raw events", "rows", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) serve(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	r := bufio.NewReaderSize(conn, 64<<10)
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return
	}
	var req Request
	resp := Response{V: ProtocolVersion}
	var after func()
	if err := json.Unmarshal(line, &req); err != nil {
		resp.Error = "bad request: " + err.Error()
	} else {
		s.touch()
		resp, after = s.handle(ctx, &req)
	}
	b, _ := json.Marshal(resp)
	if _, err := conn.Write(append(b, '\n')); err == nil && after != nil {
		// Only count memories as shown once the hook actually received them.
		after()
	}
	if s.shouldRestart(&req) && s.restart.CompareAndSwap(false, true) {
		s.log.Info("exiting so the new binary takes over", "client", req.Version, "daemon", Version, "op", req.Op)
		go func() { time.Sleep(200 * time.Millisecond); s.cancel() }()
	}
}

// shouldRestart: an explicit restart request, or a different build of this same binary file
// (it was replaced on disk). Other installed copies of amem never restart the daemon, so two
// versions side by side cannot make it flap.
func (s *Server) shouldRestart(req *Request) bool {
	if req.Op == OpRestart {
		return true
	}
	return req.Version != "" && req.Version != Version && req.Exe != "" && req.Exe == selfExe()
}

func (s *Server) handle(ctx context.Context, req *Request) (Response, func()) {
	resp := Response{V: ProtocolVersion, OK: true}
	var data any
	var err error
	var after func()
	switch req.Op {
	case OpPing:
		data = map[string]any{"version": Version, "pid": os.Getpid(), "exe": selfExe(), "warnings": s.cfg.Warnings}
	case OpRestart:
		data = map[string]any{"restarting": true}
	case OpEvent:
		if req.Event == nil {
			err = errors.New("missing event")
			break
		}
		resp.Context, after, err = s.ingest(ctx, req.Event, !req.NoContext)
	default:
		data, err = s.api(ctx, req.Op, req.Params)
	}
	if err != nil {
		s.log.Error("request failed", "op", req.Op, "err", err)
		return Response{V: ProtocolVersion, Error: err.Error()}, nil
	}
	if data != nil {
		resp.Data, _ = json.Marshal(data)
	}
	return resp, after
}

// projectFor resolves and caches the project id of a cwd.
func (s *Server) projectFor(ctx context.Context, cwd string) (int64, project.Info, error) {
	info := s.proj.Resolve(cwd)
	s.mu.Lock()
	id, ok := s.projects[info.Key]
	s.mu.Unlock()
	if ok {
		return id, info, nil
	}
	id, err := s.st.EnsureProject(ctx, info.Key, info.Name, info.Root)
	if err != nil {
		return 0, info, err
	}
	s.mu.Lock()
	s.projects[info.Key] = id
	first := !s.adopted[id]
	s.mu.Unlock()
	if first && info.Git {
		merged, err := s.st.AdoptLegacy(ctx, id, info.Name)
		if err == nil {
			s.mu.Lock()
			s.adopted[id] = true // only after success, so a failed merge is retried
			s.mu.Unlock()
		}
		if merged {
			s.log.Info("adopted imported project", "name", info.Name, "key", info.Key)
			for _, w := range s.workers {
				w.Kick("", event.Reindex) // vector index caches project ids
			}
		}
	}
	return id, info, nil
}

// ownTool matches amem's MCP tools under any runtime's naming: memory_get, mcp__amem__memory_get
// (Claude Code, Codex), mcp_amem_memory_get (Gemini CLI), amem_memory_get (OpenCode).
var ownTool = regexp.MustCompile(`(^|_)(memory_search|memory_get|memory_timeline|memory_save|work_state)$`)

// ingest stores an event and, when the runtime can take it, returns context to inject plus a
// commit func that records the injection once the answer reached the hook.
func (s *Server) ingest(ctx context.Context, ev *event.Event, live bool) (string, func(), error) {
	if ev.TS == 0 {
		ev.TS = event.NowMS()
	}
	sid := ev.SessionID()
	pid, info, err := s.projectFor(ctx, ev.Cwd)
	if err != nil {
		return "", nil, err
	}
	// Cheap duplicate check before the expensive cleaning (redelivery after a hook timeout).
	if seen, err := s.st.SeenEvent(ctx, ev.ID); err == nil && seen {
		return "", nil, nil
	}
	privatePrompt := s.clean(ev)

	isOwn := ev.Tool != nil && ownTool.MatchString(ev.Tool.Name)
	skip := ev.Tool != nil && (isOwn || slices.Contains(s.cfg.Capture.SkipTools, ev.Tool.Name))
	store := !skip && ev.Kind != event.ToolPre

	dup, inPrivate := false, false
	if err := s.st.Write(ctx, func(tx *sql.Tx) error {
		if seen, err := s.st.HasEvent(ctx, tx, ev.ID); err != nil || seen {
			dup = seen
			return err
		}
		turn, privTurn, err := s.st.TouchSession(ctx, tx, sid, ev.Runtime, ev.NativeSession, pid, ev.Cwd, ev.Model, ev.Kind == event.Prompt, ev.TS)
		if err != nil {
			return err
		}
		if ev.Kind == event.Prompt {
			// The whole turn of a fully private prompt stays out of storage, however the
			// observer later slices batches.
			privTurn = -1
			if privatePrompt {
				privTurn = turn
			}
			if err := s.st.SetPrivateTurn(ctx, tx, sid, privTurn); err != nil {
				return err
			}
		}
		inPrivate = ev.Kind != event.Prompt && privTurn >= 0 && turn == privTurn
		if ev.Kind == event.SessionEnd {
			if err := s.st.EndSession(ctx, tx, sid, ev.TS); err != nil {
				return err
			}
		}
		if inPrivate {
			switch ev.Kind {
			case event.TurnEnd, event.SubagentStop:
				ev.LastAssistant = "" // keep the turn boundary, drop the private answer
			default:
				store = false
			}
		}
		if !store {
			return nil
		}
		_, err = s.st.InsertEvent(ctx, tx, toRow(ev, sid, turn))
		return err
	}); err != nil {
		return "", nil, err
	}
	if dup {
		return "", nil, nil
	}

	if !inPrivate {
		s.feedback(ctx, ev, sid, isOwn)
	}

	if store {
		for _, w := range s.workers {
			w.Kick(sid, ev.Kind)
		}
	}

	if !live || !ev.Kind.WantsContext() || s.cfg.Recall.Mode == "off" || isOwn {
		return "", nil, nil
	}
	res, err := s.rec.Recall(ctx, recall.Request{Event: ev, SessionID: sid, ProjectID: pid, ProjectName: info.Name})
	if err != nil {
		s.log.Warn("recall", "err", err)
		return "", nil, nil
	}
	if len(res.Items) == 0 && res.Text == "" {
		return "", nil, nil
	}
	shadow := s.cfg.Recall.Mode == "shadow"
	commit := func() {
		if err := s.st.RecordInjections(context.WithoutCancel(ctx), sid, res.Trigger, res.Items, shadow); err != nil {
			s.log.Warn("record injections", "err", err)
		}
	}
	if shadow {
		commit()
		return "", nil, nil
	}
	return res.Text, commit, nil
}

func toRow(ev *event.Event, sid string, turn int) store.EventRow {
	row := store.EventRow{SessionID: sid, Turn: turn, Kind: string(ev.Kind), TS: ev.TS}
	if ev.ID != "" {
		row.Origin, row.OriginID = "hook", ev.ID
	}
	switch ev.Kind {
	case event.Prompt:
		row.Input = ev.Prompt
		if ev.Prompt == "" {
			row.Error = "private"
		}
	case event.TurnEnd, event.SubagentStop:
		row.Output = ev.LastAssistant
		row.Input = ev.TranscriptPath
	case event.SessionStart, event.Compact:
		row.Input = ev.Source
	}
	if t := ev.Tool; t != nil {
		row.Tool = t.Name
		row.Input = string(t.Input)
		row.Output = t.Output
		row.Error = t.Error
		row.ExitCode = t.ExitCode
	}
	if ev.AgentType != "" && row.Tool == "" {
		row.Tool = "agent:" + ev.AgentType
	}
	return row
}

// clean trims oversized fields, then strips private blocks and masks secrets. It reports
// whether the prompt was entirely private. Trimming first keeps redaction cost bounded on
// multi-megabyte tool outputs.
func (s *Server) clean(ev *event.Event) (privatePrompt bool) {
	fix := func(v string) string { return CleanText(s.red, s.cfg.Capture.Redact, v, s.cfg.Capture.MaxFieldChars) }
	if ev.Prompt != "" {
		if redact.IsFullyPrivate(ev.Prompt) {
			ev.Prompt, privatePrompt = "", true
		} else {
			ev.Prompt = fix(ev.Prompt)
		}
	}
	if ev.Kind == event.TurnEnd && ev.LastAssistant == "" && ev.TranscriptPath != "" {
		ev.LastAssistant = adapter.LastAssistantFromTranscript(ev.TranscriptPath)
	}
	ev.LastAssistant = fix(ev.LastAssistant)
	if t := ev.Tool; t != nil {
		t.Output = fix(t.Output)
		t.Error = fix(t.Error)
		t.Input = CleanJSON(t.Input, fix)
	}
	return privatePrompt
}

// CleanText is the capture cleaning used by the daemon and by hooks before spooling.
func CleanText(r *redact.Redactor, doRedact bool, v string, limit int) string {
	v = Elide(v, limit)
	if doRedact {
		return r.Clean(v)
	}
	return redact.StripTags(v)
}

// CleanJSON applies fix to every string inside a JSON value (see redact.CleanJSON).
func CleanJSON(raw json.RawMessage, fix func(string) string) json.RawMessage {
	return redact.CleanJSON(raw, fix)
}

var citeRe = regexp.MustCompile(`#(\d{1,9})\b`)

// feedback marks injected memories as used: fetched via memory_get, cited as #id, or whose
// files the agent edits afterwards.
func (s *Server) feedback(ctx context.Context, ev *event.Event, sid string, own bool) {
	var ids []int64
	switch {
	case own && ev.Tool != nil && strings.HasSuffix(ev.Tool.Name, "memory_get") && (ev.Kind == event.ToolPost || ev.Kind == event.ToolPre):
		var in struct {
			IDs []int64 `json:"ids"`
		}
		_ = json.Unmarshal(ev.Tool.Input, &in)
		ids = in.IDs
	case ev.Kind == event.TurnEnd && ev.LastAssistant != "":
		seen, _, _ := s.st.Injected(ctx, sid)
		for _, m := range citeRe.FindAllStringSubmatch(ev.LastAssistant, -1) {
			if id, err := strconv.ParseInt(m[1], 10, 64); err == nil && seen[id] {
				ids = append(ids, id)
			}
		}
	case ev.Kind == event.ToolPost && ev.Tool != nil && isEdit(ev.Tool.Name):
		ids, _ = s.st.InjectedWithFiles(ctx, sid, recall.FilePaths(ev.Tool))
	}
	if len(ids) > 0 {
		if _, err := s.st.MarkUsed(ctx, sid, ids); err != nil {
			s.log.Warn("mark used", "err", err)
		}
	}
}

func isEdit(tool string) bool {
	switch tool {
	case "Edit", "Write", "MultiEdit", "NotebookEdit", "apply_patch", "write_file", "replace", "edit", "WriteFile", "StrReplaceFile":
		return true
	}
	return false
}

// Elide keeps the head (60%) and tail (30%) of oversized text.
func Elide(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	head, tail := limit*6/10, limit*3/10
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	start := len(s) - tail
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[:head] + fmt.Sprintf("\n…[%d chars elided]…\n", start-head) + s[start:]
}
