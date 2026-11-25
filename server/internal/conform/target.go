package conform

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Target is a running server under test. Both the upstream `t3` binary and
// `yatt-server` expose the same CLI, which is part of the spec:
//
//	<bin> serve --base-dir <dir> --port <port> --host 127.0.0.1 --no-browser <cwd>
//	<bin> auth session issue --base-dir <dir> --token-only [--label <l>]
type Target struct {
	Bin       string
	BaseDir   string
	Workspace string
	Port      int
	Token     string
	Log       *bytes.Buffer
	cmd       *exec.Cmd
}

func (t *Target) Origin() string { return fmt.Sprintf("http://127.0.0.1:%d", t.Port) }
func (t *Target) WSURL() string {
	return fmt.Sprintf("ws://127.0.0.1:%d/ws?orchestrationProtocol=2&clientSurface=desktop", t.Port)
}

// Start launches bin with a fresh data dir and workspace under root.
func Start(ctx context.Context, bin, root string) (*Target, error) {
	base := filepath.Join(root, "base")
	ws := filepath.Join(root, "workspace")
	for _, d := range []string{base, ws} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	// Upstream puts the login shell's PATH ahead of its own, so a CLI
	// installed on the machine would win over the fixture on PATH. Settings
	// name the fixture explicitly; yatt-server ignores this file.
	if fake, err := exec.LookPath("claude"); err == nil && strings.Contains(fake, "conformance") {
		settings := fmt.Sprintf(`{"providerInstances":{"claudeAgent":{"driver":"claudeAgent","config":{"binaryPath":%q}}}}`, fake)
		if err := os.MkdirAll(filepath.Join(base, "userdata"), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(base, "userdata", "settings.json"), []byte(settings), 0o644); err != nil {
			return nil, err
		}
	}
	// Providers and the VCS layer expect a repository.
	git := exec.Command("git", "init", "-q", "-b", "main", ws)
	_ = git.Run()
	port, err := freePort()
	if err != nil {
		return nil, err
	}
	t := &Target{Bin: bin, BaseDir: base, Workspace: ws, Port: port, Log: &bytes.Buffer{}}
	t.cmd = exec.Command(bin, "serve", "--base-dir", base, "--port", fmt.Sprint(port),
		"--host", "127.0.0.1", "--no-browser", ws)
	t.cmd.Dir = ws
	t.cmd.Stdout = t.Log
	t.cmd.Stderr = t.Log
	t.cmd.Env = append(os.Environ(), "NO_COLOR=1", "T3CODE_DISABLE_TELEMETRY=1",
		"FAKE_CODEX_LOG="+filepath.Join(root, "fake-codex.log"), "FAKE_CLAUDE_LOG="+filepath.Join(root, "fake-claude.log"),
		"CODEX_HOME="+filepath.Join(root, "codex-home"))
	t.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := t.cmd.Start(); err != nil {
		return nil, err
	}
	if err := t.waitReady(ctx, 120*time.Second); err != nil {
		t.Stop()
		return nil, fmt.Errorf("%w\n--- server log ---\n%s", err, t.Log.String())
	}
	out, err := exec.CommandContext(ctx, bin, "auth", "session", "issue", "--base-dir", base,
		"--token-only", "--label", "t3conform").Output()
	if err != nil {
		t.Stop()
		return nil, fmt.Errorf("issuing a token: %w", err)
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) == 0 {
		t.Stop()
		return nil, fmt.Errorf("`auth session issue` printed nothing")
	}
	t.Token = lines[len(lines)-1]
	return t, nil
}

func (t *Target) waitReady(ctx context.Context, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		resp, err := http.Get(t.Origin() + "/.well-known/t3/environment")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("server did not answer within %s", limit)
}

// Stop ends the server and everything it started.
func (t *Target) Stop() {
	if t.cmd == nil || t.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = t.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}
