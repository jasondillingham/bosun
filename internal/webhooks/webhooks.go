// Package webhooks fires async HTTP notifications at bosun lifecycle
// events. Sibling to internal/hooks (which runs operator-defined shell
// commands): webhooks are the "push to Slack / Discord / something
// HTTP" path that hooks can technically do via curl but that operators
// want a first-class config knob for.
//
// Phase 5 #64. Async by design — Fire spawns a goroutine per webhook
// and returns immediately. Slack's median response is 200ms but the
// p99 is multi-second, and a blocking hook would visibly stall every
// `bosun done`. Failures log to stderr; bosun's success path is never
// gated on a webhook delivery.
package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Format selects the body shape we send. Slack and Discord both
// accept simple JSON payloads with a single text field on their
// "incoming webhook" surface — we fill the canonical key for each so
// the operator doesn't have to write a template.
type Format string

const (
	FormatPlain   Format = "plain"
	FormatSlack   Format = "slack"
	FormatDiscord Format = "discord"
)

// KnownFormats is the public enum surface — used by Validate to
// refuse typos at config-load time.
var KnownFormats = []Format{FormatPlain, FormatSlack, FormatDiscord}

// DefaultTimeoutSeconds caps each HTTP POST. 10s is generous for
// Slack / Discord / GitHub Actions — long enough to absorb a
// occasional slow path, short enough that a stalled endpoint can't
// hold a goroutine for the rest of the bosun process's lifetime.
const DefaultTimeoutSeconds = 10

// MaxTimeoutSeconds caps the operator-visible knob so a misconfigured
// 600 doesn't pin goroutines indefinitely. Anything longer should be
// handled by a real queue, not the inline webhook.
const MaxTimeoutSeconds = 60

// WebhookDef is one operator-configured HTTP endpoint that bosun POSTs
// to on lifecycle events.
type WebhookDef struct {
	// URL is the destination. Must be http or https. Slack and
	// Discord both publish "incoming webhook" URLs that fit here.
	URL string `json:"url"`
	// Events filters which lifecycle events trigger this webhook.
	// Empty means "all events" — convenient for catch-all
	// notification endpoints. Entries must be valid lifecycle event
	// names (same set hooks.KnownEvents accepts).
	Events []string `json:"events,omitempty"`
	// Format shapes the JSON body. See the Format constants.
	// Empty defaults to FormatPlain.
	Format Format `json:"format,omitempty"`
	// Headers are extra HTTP headers added to the POST. Useful for
	// X-Auth tokens, GitHub-style signature secrets, etc.
	Headers map[string]string `json:"headers,omitempty"`
	// TimeoutSeconds bounds the HTTP request. Zero defaults to
	// DefaultTimeoutSeconds.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
}

// Validate reports a non-nil error when the def is structurally
// invalid. The operator-facing config.Validate calls into this so
// bosun refuses to start with a malformed webhooks list.
func (w WebhookDef) Validate(index int) error {
	if strings.TrimSpace(w.URL) == "" {
		return fmt.Errorf("webhooks[%d]: url must not be empty", index)
	}
	u, err := url.Parse(w.URL)
	if err != nil {
		return fmt.Errorf("webhooks[%d]: url %q is not parseable: %w", index, w.URL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("webhooks[%d]: url scheme must be http or https, got %q", index, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("webhooks[%d]: url %q has no host component", index, w.URL)
	}
	if w.Format != "" && !isKnownFormat(w.Format) {
		return fmt.Errorf("webhooks[%d]: format %q must be one of %s",
			index, w.Format, formatList())
	}
	if w.TimeoutSeconds < 0 {
		return fmt.Errorf("webhooks[%d]: timeout_seconds must be ≥ 0, got %d", index, w.TimeoutSeconds)
	}
	if w.TimeoutSeconds > MaxTimeoutSeconds {
		return fmt.Errorf("webhooks[%d]: timeout_seconds %d exceeds the %ds ceiling",
			index, w.TimeoutSeconds, MaxTimeoutSeconds)
	}
	return nil
}

func isKnownFormat(f Format) bool {
	for _, k := range KnownFormats {
		if k == f {
			return true
		}
	}
	return false
}

func formatList() string {
	out := make([]string, 0, len(KnownFormats))
	for _, k := range KnownFormats {
		out = append(out, string(k))
	}
	return strings.Join(out, "|")
}

// httpClientFn returns the http.Client used for one POST. Production
// builds a fresh client per call so the timeout matches the def.
// Tests substitute a closure that returns httptest.Server-bound
// clients without changing the caller surface.
var httpClientFn = func(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

// stderrFn is the destination for "webhook failed" diagnostics.
// Production uses os.Stderr; tests can replace it to capture
// without scraping the real stream.
var stderrFn func() io.Writer = func() io.Writer { return os.Stderr }

// Fire posts one webhook per matching def, in parallel goroutines.
// Returns a sync.WaitGroup the caller can Wait() on for test
// determinism — production callsites discard the WaitGroup because
// the whole point is fire-and-forget. The returned WG is already
// `Add`-incremented; goroutines call Done.
//
// `env` is the same map already populated by every hooks.Run
// callsite, so wiring webhooks alongside hooks is one line per
// site.
func Fire(ctx context.Context, defs []WebhookDef, event string, env map[string]string) *sync.WaitGroup {
	wg := &sync.WaitGroup{}
	for _, d := range defs {
		if !matchesEvent(d, event) {
			continue
		}
		d := d
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := deliverOne(ctx, d, event, env); err != nil {
				_, _ = fmt.Fprintf(stderrFn(), "bosun: webhook %s -> %s failed: %v\n", event, d.URL, err)
			}
		}()
	}
	return wg
}

// matchesEvent returns true when d should fire for event. Empty
// Events list means "all events."
func matchesEvent(d WebhookDef, event string) bool {
	if len(d.Events) == 0 {
		return true
	}
	for _, e := range d.Events {
		if e == event {
			return true
		}
	}
	return false
}

// deliverOne builds the body, POSTs it, and returns any error. Split
// out from the goroutine so tests can call it synchronously without
// dancing around the WaitGroup.
func deliverOne(ctx context.Context, d WebhookDef, event string, env map[string]string) error {
	timeout := time.Duration(d.TimeoutSeconds) * time.Second
	if timeout == 0 {
		timeout = DefaultTimeoutSeconds * time.Second
	}
	body, err := buildBody(d.Format, event, env)
	if err != nil {
		return fmt.Errorf("build body: %w", err)
	}

	postCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(postCtx, http.MethodPost, d.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "bosun-webhook/1")
	for k, v := range d.Headers {
		req.Header.Set(k, v)
	}

	client := httpClientFn(timeout)
	resp, err := client.Do(req)
	if err != nil {
		// Distinguish a context-deadline timeout so the log line
		// tells the operator why their endpoint failed (slow vs.
		// unreachable vs. TLS error).
		if errors.Is(postCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		return err
	}
	defer resp.Body.Close()

	// 2xx is success. 429 / 5xx is the endpoint's problem; we surface
	// the status so an operator can see "Slack is rate-limiting me"
	// instead of guessing. We deliberately don't retry — bosun is not
	// a delivery queue; the operator who needs guaranteed delivery
	// should put a real queue between bosun and Slack.
	if resp.StatusCode/100 != 2 {
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(preview)))
	}
	return nil
}

// safeEnvelopeKeys is the allowlist of env keys forwarded to
// FormatPlain webhook bodies. Everything outside this set is
// deliberately dropped — agent-controlled free-text fields
// (BOSUN_DONE_MESSAGE, BOSUN_STUCK_MESSAGE) used to ride through
// the envelope's `env` map, which meant an agent that put a stray
// API key in `bosun done --message "..."` would ship it to the
// configured webhook URL. Security audit C1+H1 (2026-05) closed
// that path by switching to a curated set.
//
// New keys added here must be either (a) operator-configured at
// hook-fire time or (b) structured non-free-text from bosun's own
// state derivation. Anything an agent can write arbitrary strings
// into stays off this list — use FormatSlack / FormatDiscord
// (which only forwards a buildText-shaped one-liner) if you need
// to surface a message to humans.
var safeEnvelopeKeys = []string{
	// Session identity + structure
	"BOSUN_REPO_ROOT",
	"BOSUN_SESSION_LABEL",
	"BOSUN_SESSION_COUNT",
	"BOSUN_BASE_BRANCH",
	"BOSUN_BRANCH",
	// Numeric / enum state
	"BOSUN_AHEAD",
	"BOSUN_AHEAD_COUNT",
	"BOSUN_DIRTY",
	"BOSUN_DONE_STATUS",   // enum: "DONE" | "STUCK"
	"BOSUN_MERGE_COMMIT",  // sha
	"BOSUN_CLEANUP_COUNT", // int
	"BOSUN_CLEANUP_REASON",
}

// buildBody produces the JSON payload for the chosen format. Slack
// and Discord both accept a single text field; FormatPlain emits a
// curated envelope (see safeEnvelopeKeys) for operators piping into
// a custom collector.
func buildBody(format Format, event string, env map[string]string) ([]byte, error) {
	if format == "" {
		format = FormatPlain
	}
	session := env["BOSUN_SESSION_LABEL"]
	if session == "" {
		// Some events (pre/post init) use BOSUN_ROUND_LABEL or
		// similar instead. Fall back to "the round" so the message
		// reads naturally either way.
		session = "(round)"
	}

	switch format {
	case FormatSlack:
		text := buildText(event, session, env)
		return json.Marshal(map[string]string{"text": text})
	case FormatDiscord:
		text := buildText(event, session, env)
		return json.Marshal(map[string]string{"content": text})
	default:
		// Plain JSON envelope: curated event data so operators with
		// custom collectors can decode without parsing prose. Note
		// `env` is allowlist-filtered — see safeEnvelopeKeys for the
		// security-audit rationale.
		envelope := map[string]any{
			"event":     event,
			"session":   env["BOSUN_SESSION_LABEL"],
			"timestamp": time.Now().UTC().Format(time.RFC3339),
			"env":       filterEnvelopeEnv(env),
		}
		return json.Marshal(envelope)
	}
}

// filterEnvelopeEnv returns a new map containing only keys in
// safeEnvelopeKeys that are present in src. Missing keys are
// skipped (not emitted as ""), so downstream JSON stays compact and
// consumers can use key-presence as a "did the event supply this?"
// signal.
func filterEnvelopeEnv(src map[string]string) map[string]string {
	out := make(map[string]string, len(safeEnvelopeKeys))
	for _, k := range safeEnvelopeKeys {
		if v, ok := src[k]; ok {
			out[k] = v
		}
	}
	return out
}

// buildText is the human-readable single-line message Slack and
// Discord render. Keeps the format consistent so operators can
// recognise events at a glance.
func buildText(event, session string, env map[string]string) string {
	prefix := fmt.Sprintf("[bosun:%s] %s", event, session)
	// Lean on the env keys hooks already populate. Different events
	// supply different keys; we surface the most useful suffixes
	// without making the message a JSON dump.
	switch event {
	case "post-done":
		status := env["BOSUN_DONE_STATUS"]
		msg := env["BOSUN_DONE_MESSAGE"]
		if status != "" {
			prefix += " (" + status + ")"
		}
		if msg != "" {
			prefix += ": " + msg
		}
	case "post-merge", "pre-merge":
		if v := env["BOSUN_MERGE_RESULT"]; v != "" {
			prefix += " — " + v
		}
	case "post-cleanup", "pre-cleanup":
		if v := env["BOSUN_CLEANUP_PLAN"]; v != "" {
			prefix += " — " + v
		}
	}
	return prefix
}
