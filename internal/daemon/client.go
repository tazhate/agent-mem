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
	"syscall"
	"time"

	"github.com/tazhate/agent-mem/internal/config"
)

// Call sends one request and waits for the answer within timeout.
func Call(cfg *config.Config, req *Request, timeout time.Duration) (*Response, error) {
	req.V, req.Version = ProtocolVersion, Version
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
		if err := Spawn(cfg); err != nil {
			return err
		}
		// The first start after an upgrade may run migrations for several seconds.
		if !WaitReady(cfg, 20*time.Second) {
			return fmt.Errorf("daemon did not start; see %s", cfg.LogPath())
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
	cmd.Env = append(os.Environ(), "AMEM_DISABLE=1") // children of the daemon (CLI observers) must not re-enter hooks
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
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
