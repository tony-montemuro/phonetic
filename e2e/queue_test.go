//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// article mirrors the API's JSON fields the tests use.
type article struct {
	ID          int64  `json:"id"`
	URL         string `json:"url"`
	Status      string `json:"status"`
	FailedStage string `json:"failed_stage"`
	Attempts    int    `json:"attempts"`
	Error       string `json:"error"`
	Title       string `json:"title"`
	Author      string `json:"author"`
	WordCount   int    `json:"word_count"`
	ImageCount  int    `json:"image_count"`
	ReadyAt     string `json:"ready_at"`
	DurationMS  int64  `json:"duration_ms"`
	AudioBytes  int64  `json:"audio_bytes"`
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
	web := newSite(t)
	inst := start(t, options{})

	a := submit(t, inst, web.URL("/article/hello?utm_source=share#top"))
	if a.URL != web.URL("/article/hello") || a.Status != "queued" {
		t.Errorf("submitted article = %+v", a)
	}
	got := waitStatus(t, inst, a.ID, "ready", 10*time.Second)
	if got.ReadyAt == "" || got.Attempts != 0 {
		t.Errorf("ready article = %+v", got)
	}
	// Real fetch and extract ran: metadata in the API, artifacts on disk.
	if got.Title != articleTitle("hello") || got.Author != "Test Author" || got.WordCount < 100 || got.ImageCount != 1 {
		t.Errorf("extracted metadata = %+v", got)
	}
	dir := filepath.Join(inst.DataDir, "articles", strconv.FormatInt(a.ID, 10))
	for _, name := range []string{"raw.html", "fetch.json", "extracted.json", "script.txt", "audio.mp3"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("artifact %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "chunks")); !os.IsNotExist(err) {
		t.Errorf("chunks dir left behind (%v)", err)
	}
	// The real script and synthesize stages ran against the fake engine.
	script, _ := os.ReadFile(filepath.Join(dir, "script.txt"))
	if !strings.HasPrefix(string(script), articleTitle("hello")+". By Test Author.") {
		t.Errorf("script starts %q", script[:min(len(script), 60)])
	}
	if info, err := os.Stat(filepath.Join(dir, "audio.mp3")); err != nil || got.AudioBytes != info.Size() {
		t.Errorf("audio_bytes = %d, file: %v %v", got.AudioBytes, info, err)
	}
	if want := len(inst.opts.TTS.Requests()) * int(chunkSpeech.Milliseconds()); got.DurationMS < int64(want) {
		t.Errorf("duration_ms = %d, want at least %d (one chunk of speech per request)", got.DurationMS, want)
	}

	// Resubmitting (any spelling) returns the existing article.
	var dup article
	respelled := strings.Replace(web.URL("/article/hello"), "http://", "HTTP://", 1) + "#comments"
	if code := api(t, inst, "POST", "/api/articles", fmt.Sprintf(`{"url": %q}`, respelled), &dup); code != http.StatusOK || dup.ID != a.ID {
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

// Pages that can never become an episode fail at once, without retries.
func TestUnfetchablePagesFailPermanently(t *testing.T) {
	t.Parallel()
	web := newSite(t)
	inst := start(t, options{})
	for _, tt := range []struct{ path, stage, wantErr string }{
		{"/missing", "fetching", "404"},
		{"/file.pdf", "fetching", "not HTML"},
		{"/empty", "extracting", "no readable article"},
	} {
		a := submit(t, inst, web.URL(tt.path))
		got := waitStatus(t, inst, a.ID, "failed", 10*time.Second)
		if got.FailedStage != tt.stage || got.Attempts != 1 || !strings.Contains(got.Error, tt.wantErr) {
			t.Errorf("%s: failed article = %+v, want a first-attempt %s failure mentioning %q", tt.path, got, tt.stage, tt.wantErr)
		}
	}
}

// Synthesis runs one article at a time (the engine has one GPU), even with
// several workers.
func TestSynthesisRunsOneAtATime(t *testing.T) {
	t.Parallel()
	web := newSite(t)
	engine := newSpeechEngine(t)
	engine.Delay.Store(int64(300 * time.Millisecond))
	inst := start(t, options{TTS: engine, Env: map[string]string{"PHONETIC_WORKERS": "4"}})
	var ids []int64
	for i := range 4 {
		ids = append(ids, submit(t, inst, web.URL(fmt.Sprintf("/article/concurrent-%d", i))).ID)
	}
	for _, id := range ids {
		waitStatus(t, inst, id, "ready", 30*time.Second)
	}
	if n := len(engine.Requests()); n < len(ids) {
		t.Errorf("engine got %d requests, want at least %d", n, len(ids))
	}
	if p := engine.Peak(); p != 1 {
		t.Errorf("peak concurrent synthesis requests = %d, want 1", p)
	}
}

// An engine that rejects the request fails the article at once; one that
// is down leaves it to be retried.
func TestSpeechEngineFailures(t *testing.T) {
	t.Parallel()
	web := newSite(t)

	rejecting := newSpeechEngine(t)
	rejecting.Fail.Store(http.StatusBadRequest)
	inst := start(t, options{TTS: rejecting})
	a := submit(t, inst, web.URL("/article/rejected"))
	got := waitStatus(t, inst, a.ID, "failed", 10*time.Second)
	if got.FailedStage != "synthesizing" || got.Attempts != 1 || !strings.Contains(got.Error, "400") {
		t.Errorf("failed article = %+v, want a first-attempt synthesizing failure with the 400", got)
	}

	down := newSpeechEngine(t)
	down.Fail.Store(http.StatusServiceUnavailable)
	inst = start(t, options{TTS: down})
	a = submit(t, inst, web.URL("/article/engine-down"))
	if err := inst.waitForLog(t, "stage failed; will retry", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	api(t, inst, "GET", fmt.Sprintf("/api/articles/%d", a.ID), "", &got)
	if got.Status != "synthesizing" || !strings.Contains(got.Error, "503") {
		t.Errorf("article = %+v, want still synthesizing with a 503 error", got)
	}
}

// A transient failure (503) is retried later, not failed.
func TestTransientFetchFailureIsRetried(t *testing.T) {
	t.Parallel()
	web := newSite(t)
	inst := start(t, options{})
	a := submit(t, inst, web.URL("/flaky"))
	if err := inst.waitForLog(t, "stage failed; will retry", 10*time.Second); err != nil {
		t.Fatal(err)
	}
	var got article
	api(t, inst, "GET", fmt.Sprintf("/api/articles/%d", a.ID), "", &got)
	if got.Status != "fetching" || got.Attempts != 1 || !strings.Contains(got.Error, "503") {
		t.Errorf("article = %+v, want still fetching after 1 attempt with a 503 error", got)
	}
}

// By default (setting left blank) the fetcher refuses local addresses
// without connecting: a local URL fails permanently and the site never sees
// a request.
func TestFetchingLocalAddressesIsRefusedByDefault(t *testing.T) {
	t.Parallel()
	web := newSite(t)
	inst := start(t, options{Env: map[string]string{"PHONETIC_FETCH_ALLOW_PRIVATE": ""}})
	a := submit(t, inst, web.URL("/slow/local"))
	got := waitStatus(t, inst, a.ID, "failed", 10*time.Second)
	if got.FailedStage != "fetching" || !strings.Contains(got.Error, "not a public internet address") {
		t.Errorf("failed article = %+v, want a blocked-address fetch failure", got)
	}
	if n := web.hits.Load(); n != 0 {
		t.Errorf("site received %d requests; the blocked fetch must not connect", n)
	}
	if strings.Contains(inst.Logs(), "SSRF protection is off") {
		t.Error("SSRF-off warning logged with the default setting")
	}
}

// PHONETIC_FETCH_BLOCKED applies even with private fetching allowed.
func TestConfiguredBlockIsEnforced(t *testing.T) {
	t.Parallel()
	web := newSite(t)
	inst := start(t, options{Env: map[string]string{"PHONETIC_FETCH_BLOCKED": "127.0.0.1"}})
	a := submit(t, inst, web.URL("/slow/blocked"))
	got := waitStatus(t, inst, a.ID, "failed", 10*time.Second)
	if !strings.Contains(got.Error, "configured block") || web.hits.Load() != 0 {
		t.Errorf("failed article = %+v (site hits %d), want a configured-block failure without connecting", got, web.hits.Load())
	}
	if !strings.Contains(inst.Logs(), "SSRF protection is off") {
		t.Error("expected the SSRF-off warning when private fetching is allowed")
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

// Killing the process mid-fetch (no cleanup at all) must not lose the
// article: the next start releases the dead process's lease and finishes it.
func TestCrashMidStageRecoversOnRestart(t *testing.T) {
	t.Parallel()
	web := newSite(t)
	web.Slow.Store(true)
	inst := start(t, options{})
	a := submit(t, inst, web.URL("/slow/crash"))
	web.waitHits(t, 1) // the fetch is in flight
	if st := inst.kill(t); st.Signal != syscall.SIGKILL {
		t.Fatalf("kill: %s", st)
	}

	web.Slow.Store(false)
	inst = inst.restart(t)
	if err := inst.waitForLog(t, "released stale leases", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	got := waitStatus(t, inst, a.ID, "ready", 10*time.Second)
	if got.Error != "" || got.Title != articleTitle("crash") {
		t.Errorf("recovered article = %+v", got)
	}
}

// SIGTERM mid-fetch interrupts the stage, releases its lease without
// counting the attempt, and exits 0; the article then finishes after restart.
func TestGracefulShutdownMidStageReleasesWork(t *testing.T) {
	t.Parallel()
	web := newSite(t)
	web.Slow.Store(true)
	inst := start(t, options{})
	a := submit(t, inst, web.URL("/slow/graceful"))
	web.waitHits(t, 1) // the fetch is in flight
	if st := inst.signal(t, syscall.SIGTERM, gracefulWait); st != (exitStatus{}) {
		t.Fatalf("after SIGTERM: %s, want exit code 0", st)
	}
	if !strings.Contains(inst.Logs(), releasedMsg) {
		t.Error("expected the interrupted stage's lease to be released")
	}

	// The next run of the stage must be attempt 1: the interrupted one
	// wasn't counted (it would be 2 otherwise).
	inst = inst.restart(t)
	if strings.Contains(inst.Logs(), "released stale leases") {
		t.Error("clean shutdown should leave no stale leases")
	}
	web.waitHits(t, 2)
	if !strings.Contains(inst.Logs(), "stage=fetching attempt=1") {
		t.Errorf("rerun is not attempt 1; logs:\n%s", inst.Logs())
	}
	inst.kill(t)

	web.Slow.Store(false)
	inst = inst.restart(t)
	waitStatus(t, inst, a.ID, "ready", 10*time.Second)
}

// A stage that keeps crashing the whole process must not loop forever: each
// crash counts as an attempt, and once they're used up the next start fails
// the article instead of running it again.
func TestCrashLoopFailsArticle(t *testing.T) {
	t.Parallel()
	web := newSite(t)
	web.Slow.Store(true)
	inst := start(t, options{})
	a := submit(t, inst, web.URL("/slow/crashloop"))
	const maxAttempts = 3 // pipeline default
	for i := 1; i <= maxAttempts; i++ {
		web.waitHits(t, int64(i)) // attempt i's fetch is in flight
		if !strings.Contains(inst.Logs(), fmt.Sprintf("stage=fetching attempt=%d", i)) {
			t.Fatalf("crash %d: expected attempt %d in the logs", i, i)
		}
		inst.kill(t)
		inst = inst.restart(t)
	}
	got := waitStatus(t, inst, a.ID, "failed", 10*time.Second)
	if !strings.Contains(got.Error, "gave up after 3 attempts") {
		t.Errorf("error = %q", got.Error)
	}
	if strings.Contains(inst.Logs(), `msg="stage started"`) || web.hits.Load() != maxAttempts {
		t.Errorf("stage ran again after attempts were exhausted (site hits %d)", web.hits.Load())
	}
}

// A second process on the same data dir is refused before it touches the
// database, so it can't release the running instance's live leases.
func TestSecondInstanceOnDataDirIsRefused(t *testing.T) {
	t.Parallel()
	web := newSite(t)
	web.Slow.Store(true)
	first := start(t, options{})
	a := submit(t, first, web.URL("/slow/locked"))
	web.waitHits(t, 1)

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
