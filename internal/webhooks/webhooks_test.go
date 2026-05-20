package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// captureStderr replaces stderrFn for the duration of fn and returns
// whatever was written. Webhook failures log to stderr by contract;
// the only way to assert on the diagnostic without scraping the real
// stream is to swap the writer.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := stderrFn
	stderrFn = func() io.Writer { return &buf }
	t.Cleanup(func() { stderrFn = prev })
	fn()
	return buf.String()
}

// TestValidate_TableDriven covers every refusal path in one place so
// adding a new validation rule means adding a row, not a whole test.
func TestValidate_TableDriven(t *testing.T) {
	cases := []struct {
		name    string
		def     WebhookDef
		wantErr string
	}{
		{name: "valid http", def: WebhookDef{URL: "http://example.com/x"}},
		{name: "valid https", def: WebhookDef{URL: "https://hooks.slack.com/x"}},
		{name: "valid with slack format", def: WebhookDef{URL: "https://x.test", Format: FormatSlack}},
		{name: "empty url", def: WebhookDef{}, wantErr: "url must not be empty"},
		{name: "ftp scheme", def: WebhookDef{URL: "ftp://x/y"}, wantErr: "scheme must be http or https"},
		{name: "no host", def: WebhookDef{URL: "https:///"}, wantErr: "no host component"},
		{name: "unknown format", def: WebhookDef{URL: "https://x", Format: "json5"}, wantErr: "format"},
		{name: "negative timeout", def: WebhookDef{URL: "https://x", TimeoutSeconds: -1}, wantErr: "timeout_seconds must be ≥ 0"},
		{name: "huge timeout", def: WebhookDef{URL: "https://x", TimeoutSeconds: MaxTimeoutSeconds + 1}, wantErr: "exceeds the"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.def.Validate(0)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate returned %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate returned nil, want substring %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Validate err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

// TestFire_PlainFormat_PostsEnvelope exercises the happy path with
// FormatPlain. The handler asserts the body matches the documented
// envelope shape so consumers of the JSON contract (custom log
// collectors) catch any silent rewrite.
func TestFire_PlainFormat_PostsEnvelope(t *testing.T) {
	var got struct {
		Event   string            `json:"event"`
		Session string            `json:"session"`
		TS      string            `json:"timestamp"`
		Env     map[string]string `json:"env"`
	}
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	defs := []WebhookDef{{URL: srv.URL, Format: FormatPlain}}
	env := map[string]string{
		"BOSUN_SESSION_LABEL": "session-1",
		"BOSUN_DONE_STATUS":   "DONE",
		"BOSUN_DONE_MESSAGE":  "shipped it",
	}
	wg := Fire(context.Background(), defs, "post-done", env)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if got.Event != "post-done" {
		t.Errorf("event = %q, want post-done", got.Event)
	}
	if got.Session != "session-1" {
		t.Errorf("session = %q, want session-1", got.Session)
	}
	if got.Env["BOSUN_DONE_STATUS"] != "DONE" {
		t.Errorf("env.BOSUN_DONE_STATUS = %q, want DONE", got.Env["BOSUN_DONE_STATUS"])
	}
	if _, err := time.Parse(time.RFC3339, got.TS); err != nil {
		t.Errorf("timestamp = %q, not RFC3339: %v", got.TS, err)
	}
}

// TestFire_SlackFormat_PostsTextKey confirms the Slack-shaped body
// uses the "text" key (Slack's incoming-webhook contract).
func TestFire_SlackFormat_PostsTextKey(t *testing.T) {
	var bodyText string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var got map[string]string
		_ = json.Unmarshal(body, &got)
		bodyText = got["text"]
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	defs := []WebhookDef{{URL: srv.URL, Format: FormatSlack}}
	env := map[string]string{"BOSUN_SESSION_LABEL": "session-2", "BOSUN_DONE_STATUS": "DONE", "BOSUN_DONE_MESSAGE": "ok"}
	Fire(context.Background(), defs, "post-done", env).Wait()

	if !strings.Contains(bodyText, "session-2") {
		t.Errorf("body text %q missing session label", bodyText)
	}
	if !strings.Contains(bodyText, "post-done") {
		t.Errorf("body text %q missing event name", bodyText)
	}
}

// TestFire_DiscordFormat_PostsContentKey: Discord's incoming
// webhooks use "content" — confirm we pick the right key.
func TestFire_DiscordFormat_PostsContentKey(t *testing.T) {
	var body map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	defs := []WebhookDef{{URL: srv.URL, Format: FormatDiscord}}
	Fire(context.Background(), defs, "post-done", map[string]string{"BOSUN_SESSION_LABEL": "session-3"}).Wait()

	if body["text"] != "" {
		t.Errorf("Discord body should not have 'text' key, got %v", body)
	}
	if !strings.Contains(body["content"], "session-3") {
		t.Errorf("content = %q, want to mention session-3", body["content"])
	}
}

// TestFire_EventFilter only fires for the configured event list.
func TestFire_EventFilter(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	defs := []WebhookDef{{URL: srv.URL, Events: []string{"post-merge"}}}
	// post-done shouldn't fire.
	Fire(context.Background(), defs, "post-done", nil).Wait()
	// post-merge should.
	Fire(context.Background(), defs, "post-merge", nil).Wait()

	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("hits = %d, want 1 (only post-merge fires)", hits)
	}
}

// TestFire_EmptyEventsFiresEverything: zero-Events means catch-all.
func TestFire_EmptyEventsFiresEverything(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	defs := []WebhookDef{{URL: srv.URL}}
	Fire(context.Background(), defs, "post-done", nil).Wait()
	Fire(context.Background(), defs, "post-merge", nil).Wait()

	mu.Lock()
	defer mu.Unlock()
	if hits != 2 {
		t.Errorf("hits = %d, want 2 (catch-all)", hits)
	}
}

// TestFire_CustomHeaders confirms operator-supplied headers reach
// the endpoint. The common operator need: auth tokens for endpoints
// behind a gateway.
func TestFire_CustomHeaders(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("X-Auth-Token")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	defs := []WebhookDef{{
		URL:     srv.URL,
		Headers: map[string]string{"X-Auth-Token": "secret-123"},
	}}
	Fire(context.Background(), defs, "post-done", nil).Wait()

	if gotAuth != "secret-123" {
		t.Errorf("X-Auth-Token = %q, want secret-123", gotAuth)
	}
}

// TestFire_Non2xx_LogsError: 5xx and 4xx surface as stderr lines.
// We deliberately don't retry — bosun isn't a queue — so the
// diagnostic is the only operator-visible signal.
func TestFire_Non2xx_LogsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate-limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	stderr := captureStderr(t, func() {
		Fire(context.Background(), []WebhookDef{{URL: srv.URL}}, "post-done", nil).Wait()
	})
	if !strings.Contains(stderr, "429") {
		t.Errorf("stderr should mention status code, got %q", stderr)
	}
	if !strings.Contains(stderr, "rate-limited") {
		t.Errorf("stderr should include response preview, got %q", stderr)
	}
}

// TestFire_DoesNotBlockBosunPath: Fire returns immediately even if
// the endpoint is slow. The WaitGroup is for tests; production callers
// discard it and the bosun command proceeds.
func TestFire_DoesNotBlockBosunPath(t *testing.T) {
	// Closing the gate releases ALL pending handlers — one channel
	// op for both "fire one request" and "let any later test traffic
	// drain." Sending then closing was a footgun (the test goroutine
	// raced the close).
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	start := time.Now()
	wg := Fire(context.Background(), []WebhookDef{{URL: srv.URL, TimeoutSeconds: 5}}, "post-done", nil)
	elapsed := time.Since(start)
	if elapsed > 200*time.Millisecond {
		t.Errorf("Fire took %v, expected near-instant return", elapsed)
	}
	// Release the handler then drain. Close-after-release is safe
	// because the handler only reads from gate once.
	close(gate)
	wg.Wait()
}

// TestFire_TimeoutCancels: a hung endpoint surfaces a timeout in
// stderr after the configured TimeoutSeconds, not later.
func TestFire_TimeoutCancels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(3 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	start := time.Now()
	stderr := captureStderr(t, func() {
		Fire(context.Background(), []WebhookDef{{URL: srv.URL, TimeoutSeconds: 1}}, "post-done", nil).Wait()
	})
	elapsed := time.Since(start)

	if !strings.Contains(stderr, "timed out") {
		t.Errorf("stderr should mention timeout, got %q", stderr)
	}
	if elapsed > 2500*time.Millisecond {
		t.Errorf("Fire+Wait took %v, expected ~1s (timeout)", elapsed)
	}
}
