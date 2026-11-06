package daemon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/tazhate/agent-mem/internal/config"
	"github.com/tazhate/agent-mem/internal/event"
	"github.com/tazhate/agent-mem/internal/redact"
)

// Call sends one request and waits for the answer within timeout.
func Call(cfg *config.Config, req *Request, timeout time.Duration) (*Response, error) {
	req.V, req.Version, req.Built = ProtocolVersion, Version, BuildTime
	if req.Exe == "" {
		req.Exe = selfExe()
	}
	deadline := time.Now().Add(timeout)
	conn, err := net.DialTimeout("unix", cfg.SocketPath(), min(timeout, 200*time.Millisecond))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep <private> literal so the daemon can strip it
	if err := enc.Encode(req); err != nil {
		return nil, err
	}
	if _, err := conn.Write(buf.Bytes()); err != nil {
		return nil, err
	}
	line, err := bufio.NewReaderSize(conn, 64<<10).ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return &resp, errors.New(cmpOr(resp.Error, "daemon returned an error without details"))
	}
	return &resp, nil
}

// CallData is Call for API ops, decoding resp.Data into out. It starts the daemon if needed.
func CallData(cfg *config.Config, op string, params, out any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req := &Request{Op: op, Params: raw}
	resp, err := Call(cfg, req, 10*time.Second)
	if err != nil && IsUnavailable(err) {
		if err := StartAndWait(cfg, 20*time.Second); err != nil {
			return err
		}
		resp, err = Call(cfg, req, 10*time.Second)
	}
	if err != nil {
		return err
	}
	if out != nil && len(resp.Data) > 0 {
		return json.Unmarshal(resp.Data, out)
	}
	return nil
}

// IsUnavailable reports a missing or dead daemon (as opposed to a slow or failing one).
func IsUnavailable(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

// Spawn starts the daemon detached from the caller's session. Concurrent spawns are harmless:
// only one process wins the lock, the rest exit.
func Spawn(cfg *config.Config) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(cfg.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "daemon")
	cmd.Dir = "/" // do not pin whatever directory the first hook ran in
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = DaemonEnv(cfg) // children of the daemon (CLI observers) must not re-enter hooks
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// StartAndWait starts the daemon and waits until it answers. It returns early when the started
// process exits without answering (bad config, broken database), pointing at the log.
func StartAndWait(cfg *config.Config, timeout time.Duration) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	logf, err := os.OpenFile(cfg.LogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	cmd := exec.Command(exe, "daemon")
	cmd.Dir = "/"
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = DaemonEnv(cfg)
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		if _, err := Call(cfg, &Request{Op: OpPing}, 200*time.Millisecond); err == nil {
			return nil
		}
		select {
		case err := <-exited:
			// Exit 0 means another daemon holds the lock (it may still be migrating): keep
			// polling. A failure exit (bad config, broken database) will not get better.
			if err != nil {
				return fmt.Errorf("daemon exited during start (%v); see %s", err, cfg.LogPath())
			}
			exited = nil
		case <-time.After(50 * time.Millisecond):
		}
	}
	return fmt.Errorf("daemon did not answer within %s; see %s", timeout, cfg.LogPath())
}

// WaitReady polls until the daemon answers a ping or the timeout passes.
func WaitReady(cfg *config.Config, timeout time.Duration) bool {
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		if _, err := Call(cfg, &Request{Op: OpPing}, 200*time.Millisecond); err == nil {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// selfExe is the resolved path of the running binary.
func selfExe() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		return p
	}
	return exe
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// sessionVars redirect one agent session to another backend or model (a wrapper that points
// Claude Code at another Anthropic-compatible API sets ANTHROPIC_BASE_URL and maps model
// names). The daemon outlives the session
// that happened to start it, so it must not inherit them.
var sessionVars = []string{
	"ANTHROPIC_BASE_URL", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_MODEL", "ANTHROPIC_SMALL_FAST_MODEL",
	"ANTHROPIC_DEFAULT_HAIKU_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL",
	"API_TIMEOUT_MS", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_SSE_PORT",
}

// DaemonEnv is the environment for a spawned daemon: the caller's, minus session redirects
// (except variables the config names as env: secret references), plus AMEM_DISABLE so CLI
// observers it runs do not trigger amem hooks.
func DaemonEnv(cfg *config.Config) []string {
	drop := slices.DeleteFunc(slices.Clone(sessionVars), func(v string) bool { return slices.Contains(envRefs(cfg), v) })
	return append(WithoutVars(os.Environ(), drop), "AMEM_DISABLE=1")
}

// envRefs lists the variables named by env: references in the config.
func envRefs(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	add := func(ref string) {
		if name, ok := strings.CutPrefix(ref, "env:"); ok {
			out = append(out, name)
		}
	}
	for _, p := range cfg.Observer.Providers {
		add(p.Key)
	}
	add(cfg.Embed.Key)
	return out
}

// WithoutVars filters NAME=value entries.
func WithoutVars(env []string, names []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(names, name) {
			out = append(out, kv)
		}
	}
	return out
}

// PrivateMarker replaces a fully private prompt before it is spooled; the daemon recognizes
// it as a private turn when the event is replayed.
const PrivateMarker = "<private></private>"

// Sanitize applies capture cleaning before an event touches the disk spool, so secrets and
// <private> blocks never sit on disk in clear text.
func Sanitize(cfg *config.Config, ev *event.Event) {
	r := redact.New(cfg.Capture.RedactGeneric)
	fix := func(v string) string { return CleanText(r, cfg.Capture.Redact, v, cfg.Capture.MaxFieldChars) }
	if redact.IsFullyPrivate(ev.Prompt) {
		ev.Prompt = PrivateMarker
	} else {
		ev.Prompt = fix(ev.Prompt)
	}
	ev.LastAssistant = fix(ev.LastAssistant)
	if t := ev.Tool; t != nil {
		t.Output, t.Error = fix(t.Output), fix(t.Error)
		t.Input = CleanJSON(t.Input, fix)
	}
}
