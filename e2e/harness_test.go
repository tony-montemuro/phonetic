//go:build e2e

// Package e2e runs the real phonetic binary as a subprocess and exercises it
// over HTTP. Run with `make test-e2e`. When invoking go test directly, pass
// -count=1: the test cache can't see the binary TestMain builds, so changes to
// the service won't invalidate cached results.
//
// Known gap: if the test binary itself is killed (e.g. a go test -timeout
// panic), cleanups don't run and phonetic processes can be orphaned.
package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// binPath is the phonetic binary built once in TestMain (with -race when the
// tests themselves run with -race).
var binPath string

func TestMain(m *testing.M) {
	os.Exit(runMain(m))
}

func runMain(m *testing.M) int {
	dir, err := os.MkdirTemp("", "phonetic-e2e-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		return 1
	}
	defer os.RemoveAll(dir)

	binPath = filepath.Join(dir, "phonetic")
	args := []string{"build", "-o", binPath}
	if raceEnabled {
		args = append(args, "-race")
	}
	args = append(args, "github.com/tony-montemuro/phonetic/cmd/phonetic")
	build := exec.Command("go", args...)
	build.Stdout, build.Stderr = os.Stdout, os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "e2e: building phonetic:", err)
		return 1
	}
	return m.Run()
}

// Timeouts used across the harness.
const (
	readyTimeout = 10 * time.Second
	exitTimeout  = 10 * time.Second
	// waitDelay bounds how long Wait waits for output pipes after the process
	// exits (child processes, e.g. ffmpeg, may still hold them).
	waitDelay = 5 * time.Second
)

// options customize an instance. Zero values pick sensible defaults.
type options struct {
	DataDir string            // default: fresh t.TempDir()
	Addr    string            // default: a free localhost port
	Env     map[string]string // extra/overriding variables
}

// instance is one running phonetic process.
type instance struct {
	BaseURL string
	Addr    string
	DataDir string
	// Client talks to this instance. Its own transport keeps connections from
	// leaking between instances and lets signal() close idle ones first. Its
	// timeout suits API calls; use a separate client for large downloads.
	Client *http.Client

	opts    options // resolved options, reused by restart
	cmd     *exec.Cmd
	logs    *syncBuffer
	exited  chan struct{}
	waitErr error
}

// exitStatus describes how the process ended.
type exitStatus struct {
	Code   int            // exit code; -1 if killed by a signal
	Signal syscall.Signal // terminating signal, if any
}

func (s exitStatus) String() string {
	if s.Signal != 0 {
		return "killed by " + s.Signal.String()
	}
	return fmt.Sprintf("exit code %d", s.Code)
}

// start launches phonetic and waits until it is serving.
func start(t testing.TB, opts options) *instance {
	t.Helper()
	const attempts = 3
	for i := 1; ; i++ {
		inst := launch(t, opts)
		err := inst.waitReady(t, readyTimeout)
		if err == nil {
			return inst
		}
		// Another process may have grabbed the probed port before we bound it.
		if i < attempts && opts.Addr == "" && strings.Contains(inst.Logs(), "address already in use") {
			continue
		}
		t.Fatalf("phonetic (%s) not ready: %v", inst.Addr, err)
	}
}

// launch starts the process without waiting for it to become ready.
func launch(t testing.TB, opts options) *instance {
	t.Helper()
	if opts.DataDir == "" {
		opts.DataDir = t.TempDir()
	}
	if opts.Addr == "" {
		opts.Addr = freeAddr(t)
	}

	env := map[string]string{
		"PHONETIC_ADDR":      opts.Addr,
		"PHONETIC_DATA_DIR":  opts.DataDir,
		"PHONETIC_LOG_LEVEL": "debug",
	}
	for k, v := range opts.Env {
		env[k] = v
	}

	inst := &instance{
		BaseURL: "http://" + env["PHONETIC_ADDR"],
		Addr:    env["PHONETIC_ADDR"],
		DataDir: opts.DataDir,
		Client:  &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}},
		opts:    opts,
		logs:    &syncBuffer{},
		exited:  make(chan struct{}),
	}
	inst.cmd = exec.Command(binPath)
	inst.cmd.Env = buildEnv(env)
	inst.cmd.Stdout = inst.logs
	inst.cmd.Stderr = inst.logs
	inst.cmd.WaitDelay = waitDelay
	// Own process group, so kill() also takes down any children it spawns.
	inst.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := inst.cmd.Start(); err != nil {
		t.Fatalf("starting phonetic: %v", err)
	}
	go func() {
		inst.waitErr = inst.cmd.Wait()
		close(inst.exited)
	}()

	t.Cleanup(func() {
		select {
		case <-inst.exited:
		default:
			inst.killGroup()
			select {
			case <-inst.exited:
			case <-time.After(waitDelay + exitTimeout):
				t.Errorf("phonetic (%s) did not exit after SIGKILL", inst.Addr)
			}
		}
		inst.Client.CloseIdleConnections()
		logs := inst.Logs()
		if strings.Contains(logs, "WARNING: DATA RACE") {
			t.Errorf("phonetic (%s) reported a data race", inst.Addr)
		}
		if t.Failed() {
			t.Logf("phonetic (%s, pid %d) logs:\n%s", inst.Addr, inst.cmd.Process.Pid, logs)
		}
	})
	return inst
}

// passthroughEnv lists the only parent environment variables the child
// inherits. An allowlist keeps a developer's shell (API keys, base URLs) from
// leaking into tests.
var passthroughEnv = []string{"PATH", "HOME", "TMPDIR", "GORACE", "GOCOVERDIR"}

func buildEnv(env map[string]string) []string {
	var out []string
	for _, k := range passthroughEnv {
		if v, ok := os.LookupEnv(k); ok {
			if _, overridden := env[k]; !overridden {
				out = append(out, k+"="+v)
			}
		}
	}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// waitReady waits for this process's own "listening" log line (proving the
// port is ours, not another instance's), then for /healthz to return 200.
func (i *instance) waitReady(t testing.TB, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	if err := i.waitForLog(t, "msg=listening addr="+i.Addr, timeout); err != nil {
		return err
	}
	for {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, i.BaseURL+"/healthz", nil)
		resp, err := i.Client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		cancel()
		if err == nil && resp.StatusCode == http.StatusOK {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("/healthz not OK after %s (last err: %v)", timeout, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitForLog polls the process output until it contains substr. It fails fast
// if the process exits first.
func (i *instance) waitForLog(t testing.TB, substr string, timeout time.Duration) error {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if strings.Contains(i.Logs(), substr) {
			return nil
		}
		select {
		case <-i.exited:
			if strings.Contains(i.Logs(), substr) {
				return nil
			}
			return fmt.Errorf("process exited (%s) before logging %q", i.status(), substr)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no log line containing %q after %s", substr, timeout)
		}
	}
}

// signal sends sig to the process and waits for it to exit.
func (i *instance) signal(t testing.TB, sig syscall.Signal, timeout time.Duration) exitStatus {
	t.Helper()
	// Otherwise kept-alive or speculatively dialed connections make Shutdown
	// wait out its grace period for idle/new connections.
	i.Client.CloseIdleConnections()
	if err := i.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal %v: %v", sig, err)
	}
	return i.wait(t, timeout)
}

// kill SIGKILLs the process group (simulating a crash) and waits for exit.
func (i *instance) kill(t testing.TB) exitStatus {
	t.Helper()
	i.killGroup()
	return i.wait(t, waitDelay+exitTimeout)
}

func (i *instance) killGroup() {
	// Negative pid targets the process group created via Setpgid.
	_ = syscall.Kill(-i.cmd.Process.Pid, syscall.SIGKILL)
}

// wait blocks until the process exits.
func (i *instance) wait(t testing.TB, timeout time.Duration) exitStatus {
	t.Helper()
	select {
	case <-i.exited:
	case <-time.After(timeout):
		t.Fatalf("phonetic (%s) did not exit within %s", i.Addr, timeout)
	}
	var exitErr *exec.ExitError
	if i.waitErr != nil && !errors.As(i.waitErr, &exitErr) {
		t.Fatalf("wait: %v", i.waitErr)
	}
	return i.status()
}

// status reports how the process ended; only valid after exited is closed.
func (i *instance) status() exitStatus {
	ps := i.cmd.ProcessState
	if ps == nil {
		return exitStatus{Code: -1}
	}
	s := exitStatus{Code: ps.ExitCode()}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		s.Signal = ws.Signal()
	}
	return s
}

// restart starts a new process with the same data dir, address and env. The
// current process must have exited.
func (i *instance) restart(t testing.TB) *instance {
	t.Helper()
	select {
	case <-i.exited:
	default:
		t.Fatal("restart: previous process still running")
	}
	return start(t, i.opts)
}

// get performs a GET against the instance; the body is closed at cleanup.
func (i *instance) get(t testing.TB, path string) *http.Response {
	t.Helper()
	resp, err := i.Client.Get(i.BaseURL + path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// Logs returns everything the process has written so far.
func (i *instance) Logs() string { return i.logs.String() }

// freeAddr returns a localhost address with a currently-free port. Another
// process can take it before phonetic binds; start() detects and retries.
func freeAddr(t testing.TB) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// syncBuffer is a goroutine-safe bytes.Buffer for capturing process output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
