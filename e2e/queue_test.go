//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
)

// article mirrors the API's JSON fields the tests use.
type article struct {
	ID       int64  `json:"id"`
	URL      string `json:"url"`
	Status   string `json:"status"`
	Attempts int    `json:"attempts"`
	Error    string `json:"error"`
	ReadyAt  string `json:"ready_at"`
}

// api sends an authenticated JSON request and decodes the response into out
// (if non-nil), returning the status code.
func api(t testing.TB, inst *instance, method, path, body string, out any) int {
	t.Helper()
	req, err := http.NewRequest(method, inst.BaseURL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+apiToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := inst.Client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("%s %s: decoding: %v", method, path, err)
		}
	}
	return resp.StatusCode
}

// releasedMsg is the log line of a successful shutdown release (anchored on
// the closing quote so the "(not recorded...)" variants don't match).
const releasedMsg = `msg="stage interrupted by shutdown; lease released"`

// submit queues url and returns the new article.
func submit(t testing.TB, inst *instance, url string) article {
	t.Helper()
	var a article
	if code := api(t, inst, "POST", "/api/articles", fmt.Sprintf(`{"url": %q}`, url), &a); code != http.StatusAccepted {
		t.Fatalf("submit %s: status %d, want 202", url, code)
	}
	return a
}

// waitStatus polls the article until it reaches status (bounded).
func waitStatus(t testing.TB, inst *instance, id int64, status string, timeout time.Duration) article {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var a article
	for time.Now().Before(deadline) {
		if code := api(t, inst, "GET", fmt.Sprintf("/api/articles/%d", id), "", &a); code != http.StatusOK {
			t.Fatalf("get article %d: status %d", id, code)
		}
		if a.Status == status {
			return a
		}
		if a.Status == "failed" {
			t.Fatalf("article %d failed (want %q): %s", id, status, a.Error)
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("article %d stuck at %q (want %q) after %s", id, a.Status, status, timeout)
	return a
}

func TestSubmittedArticleBecomesReady(t *testing.T) {
	t.Parallel()
	inst := start(t, options{})

	a := submit(t, inst, "https://example.com/post?utm_source=share#top")
	if a.URL != "https://example.com/post" || a.Status != "queued" {
		t.Errorf("submitted article = %+v", a)
	}
	got := waitStatus(t, inst, a.ID, "ready", 10*time.Second)
	if got.ReadyAt == "" || got.Attempts != 0 {
		t.Errorf("ready article = %+v", got)
	}

	// Resubmitting (any spelling) returns the existing article.
	var dup article
	if code := api(t, inst, "POST", "/api/articles", `{"url": "HTTPS://EXAMPLE.com/post"}`, &dup); code != http.StatusOK || dup.ID != a.ID {
		t.Errorf("resubmit: status %d id %d, want 200 and %d", code, dup.ID, a.ID)
	}

	var list struct{ Articles []article }
	if code := api(t, inst, "GET", "/api/articles?status=ready", "", &list); code != http.StatusOK {
		t.Fatalf("list: status %d", code)
	}
	if len(list.Articles) != 1 || list.Articles[0].ID != a.ID {
		t.Errorf("ready list = %+v", list.Articles)
	}
}

func TestAPIRejectsMissingToken(t *testing.T) {
	t.Parallel()
	inst := start(t, options{})
	resp, err := inst.Client.Post(inst.BaseURL+"/api/articles", "application/json",
		strings.NewReader(`{"url": "https://example.com/a"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", resp.StatusCode)
	}
}

// Killing the process mid-stage (no cleanup at all) must not lose the
// article: the next start releases the dead process's lease and finishes it.
func TestCrashMidStageRecoversOnRestart(t *testing.T) {
	t.Parallel()
	inst := start(t, options{Env: map[string]string{"PHONETIC_DEV_STUB_DELAY": "1m"}})
	a := submit(t, inst, "https://example.com/crash")
	if err := inst.waitForLog(t, `msg="stage started"`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if st := inst.kill(t); st.Signal != syscall.SIGKILL {
		t.Fatalf("kill: %s", st)
	}

	inst = inst.restartWith(t, map[string]string{"PHONETIC_DEV_STUB_DELAY": "0s"})
	if err := inst.waitForLog(t, "released stale leases", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	got := waitStatus(t, inst, a.ID, "ready", 10*time.Second)
	if got.Error != "" {
		t.Errorf("recovered article has error %q", got.Error)
	}
}

// SIGTERM mid-stage interrupts the stage, releases its lease without
// counting the attempt, and exits 0; the article then finishes after restart.
func TestGracefulShutdownMidStageReleasesWork(t *testing.T) {
	t.Parallel()
	slow := map[string]string{"PHONETIC_DEV_STUB_DELAY": "1m"}
	inst := start(t, options{Env: slow})
	a := submit(t, inst, "https://example.com/graceful")
	if err := inst.waitForLog(t, `msg="stage started"`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if st := inst.signal(t, syscall.SIGTERM, gracefulWait); st != (exitStatus{}) {
		t.Fatalf("after SIGTERM: %s, want exit code 0", st)
	}
	if !strings.Contains(inst.Logs(), releasedMsg) {
		t.Error("expected the interrupted stage's lease to be released")
	}

	// The next run of the stage must be attempt 1: the interrupted one
	// wasn't counted (it would be 2 otherwise).
	inst = inst.restartWith(t, slow)
	if strings.Contains(inst.Logs(), "released stale leases") {
		t.Error("clean shutdown should leave no stale leases")
	}
	if err := inst.waitForLog(t, `msg="stage started"`, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inst.Logs(), "stage=fetching attempt=1") {
		t.Errorf("rerun is not attempt 1; logs:\n%s", inst.Logs())
	}
	inst.kill(t)

	inst = inst.restartWith(t, map[string]string{"PHONETIC_DEV_STUB_DELAY": "0s"})
	waitStatus(t, inst, a.ID, "ready", 10*time.Second)
}

// A stage that keeps crashing the whole process must not loop forever: each
// crash counts as an attempt, and once they're used up the next start fails
// the article instead of running it again.
func TestCrashLoopFailsArticle(t *testing.T) {
	t.Parallel()
	slow := map[string]string{"PHONETIC_DEV_STUB_DELAY": "1m"}
	inst := start(t, options{Env: slow})
	a := submit(t, inst, "https://example.com/crashloop")
	const maxAttempts = 3 // pipeline default
	for i := 1; i <= maxAttempts; i++ {
		if err := inst.waitForLog(t, fmt.Sprintf("stage=fetching attempt=%d", i), 10*time.Second); err != nil {
			t.Fatalf("crash %d: %v", i, err)
		}
		inst.kill(t)
		inst = inst.restartWith(t, slow)
	}
	got := waitStatus(t, inst, a.ID, "failed", 10*time.Second)
	if !strings.Contains(got.Error, "gave up after 3 attempts") {
		t.Errorf("error = %q", got.Error)
	}
	if strings.Contains(inst.Logs(), `msg="stage started"`) {
		t.Error("stage ran again after attempts were exhausted")
	}
}

// A second process on the same data dir is refused before it touches the
// database, so it can't release the running instance's live leases.
func TestSecondInstanceOnDataDirIsRefused(t *testing.T) {
	t.Parallel()
	first := start(t, options{Env: map[string]string{"PHONETIC_DEV_STUB_DELAY": "1m"}})
	a := submit(t, first, "https://example.com/locked")
	if err := first.waitForLog(t, `msg="stage started"`, 10*time.Second); err != nil {
		t.Fatal(err)
	}

	second := launch(t, options{DataDir: first.DataDir})
	if st := second.wait(t, exitTimeout); st != (exitStatus{Code: 1}) {
		t.Fatalf("second instance: %s, want exit code 1", st)
	}
	if !strings.Contains(second.Logs(), "in use by another phonetic process") {
		t.Errorf("second instance output does not explain the refusal:\n%s", second.Logs())
	}
	// The key assertion: releasing leases is what would have broken the
	// first instance (status and attempts would look unchanged).
	if strings.Contains(second.Logs(), "released stale leases") {
		t.Error("second instance released leases before being refused")
	}

	// The second instance didn't claim or complete anything either.
	var got article
	api(t, first, "GET", fmt.Sprintf("/api/articles/%d", a.ID), "", &got)
	if got.Status != "fetching" || got.Attempts != 1 {
		t.Errorf("first instance's article disturbed: %+v", got)
	}
	// And the first instance still owns a live lease: its shutdown release
	// succeeds.
	if st := first.signal(t, syscall.SIGTERM, gracefulWait); st != (exitStatus{}) {
		t.Fatalf("first instance shutdown: %s", st)
	}
	if !strings.Contains(first.Logs(), releasedMsg) {
		t.Error("first instance lost its lease")
	}
}
