//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// gracefulWait is shorter than the service's 10s shutdown budget, so a
// shutdown that falls back to force-closing can't pass as graceful.
const gracefulWait = 5 * time.Second

// assertHealthy checks /healthz, which pings the database, returns 200.
func assertHealthy(t testing.TB, inst *instance) {
	t.Helper()
	resp := inst.get(t, "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestServesHealthzAndShutsDownGracefully(t *testing.T) {
	t.Parallel()
	inst := start(t, options{})

	resp := inst.get(t, "/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d", resp.StatusCode)
	}
	var body struct{ Status string }
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Status != "ok" {
		t.Fatalf("healthz body = %+v, err = %v", body, err)
	}
	resp.Body.Close() // return the connection to the idle pool

	if st := inst.signal(t, syscall.SIGTERM, gracefulWait); st != (exitStatus{}) {
		t.Fatalf("after SIGTERM: %s, want exit code 0", st)
	}
	logs := inst.Logs()
	if !strings.Contains(logs, "msg=stopped") {
		t.Error("missing final 'stopped' log line")
	}
	if strings.Contains(logs, "graceful shutdown timed out") {
		t.Error("shutdown was forced, not graceful")
	}
}

func TestSIGINTAlsoShutsDown(t *testing.T) {
	t.Parallel()
	inst := start(t, options{})
	if st := inst.signal(t, syscall.SIGINT, gracefulWait); st != (exitStatus{}) {
		t.Fatalf("after SIGINT: %s, want exit code 0", st)
	}
}

// Restarting on the same data dir and address must reuse the database
// (migrations are idempotent) and come back healthy, including after a crash.
func TestRestartAfterCleanStopAndCrash(t *testing.T) {
	t.Parallel()
	inst := start(t, options{})

	if st := inst.signal(t, syscall.SIGTERM, gracefulWait); st != (exitStatus{}) {
		t.Fatalf("clean stop: %s", st)
	}
	inst = inst.restart(t)
	assertHealthy(t, inst)

	if st := inst.kill(t); st.Signal != syscall.SIGKILL {
		t.Fatalf("kill: %s, want killed by SIGKILL", st)
	}
	inst = inst.restart(t)
	assertHealthy(t, inst) // database usable after an unclean stop

	if _, err := os.Stat(filepath.Join(inst.DataDir, "phonetic.db")); err != nil {
		t.Fatalf("database file: %v", err)
	}
}

// The data dir (including missing parents) is created on startup.
func TestCreatesNestedDataDir(t *testing.T) {
	t.Parallel()
	dataDir := filepath.Join(t.TempDir(), "a", "b", "c")
	start(t, options{DataDir: dataDir})
	if _, err := os.Stat(filepath.Join(dataDir, "phonetic.db")); err != nil {
		t.Fatalf("database file: %v", err)
	}
}

func TestInvalidConfigExitsWithError(t *testing.T) {
	t.Parallel()
	inst := launch(t, options{Env: map[string]string{
		"PHONETIC_ADDR":      "not-an-addr",
		"PHONETIC_LOG_LEVEL": "loud",
	}})
	if st := inst.wait(t, exitTimeout); st != (exitStatus{Code: 1}) {
		t.Fatalf("%s, want exit code 1", st)
	}
	for _, want := range []string{"PHONETIC_ADDR", "PHONETIC_LOG_LEVEL"} {
		if !strings.Contains(inst.Logs(), want) {
			t.Errorf("output does not mention %s", want)
		}
	}
}

func TestPortInUseExitsWithError(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	inst := launch(t, options{Addr: ln.Addr().String()})
	if st := inst.wait(t, exitTimeout); st != (exitStatus{Code: 1}) {
		t.Fatalf("%s, want exit code 1", st)
	}
	if !strings.Contains(inst.Logs(), "address already in use") {
		t.Error("output does not mention the listen failure")
	}
}

// -h documents every setting and exits 0; stray arguments are an error.
func TestHelpFlag(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"-h", "-help"} {
		out, err := exec.Command(binPath, arg).Output() // stdout only: help is pageable
		if err != nil {
			t.Fatalf("%s: %v\n%s", arg, err, out)
		}
		for _, want := range []string{"Usage: phonetic", "PHONETIC_API_TOKEN", "PHONETIC_FETCH_BLOCKED", "DEV/TEST ONLY"} {
			if !strings.Contains(string(out), want) {
				t.Errorf("%s output missing %q:\n%s", arg, want, out)
			}
		}
	}
	for _, args := range [][]string{{"serve"}, {"-port", "80"}} {
		cmd := exec.Command(binPath, args...)
		cmd.Env = []string{} // if parsing regressed, fail fast on config instead of serving
		err := cmd.Run()
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
			t.Errorf("%v: err = %v, want exit code 2", args, err)
		}
	}
}
