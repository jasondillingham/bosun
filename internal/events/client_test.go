package events

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestParse_HandlesSpecShapes exercises the three line types the bosun
// server emits plus a couple of cases the SSE spec calls out but the
// server doesn't (multiple data: lines, missing leading space). The
// parser has to handle all of them because the brief says the client
// is "per the SSE spec" — not just "what the current server happens
// to emit."
func TestParse_HandlesSpecShapes(t *testing.T) {
	const stream = "" +
		": keep-alive\n" +
		"\n" +
		"data: hello\n" +
		"\n" +
		"event: announce\n" +
		"data: {\"session\":\"session-1\",\"kind\":\"progress\"}\n" +
		"\n" +
		"data:line-without-space\n" +
		"\n" +
		"data: line1\n" +
		"data: line2\n" +
		"\n"

	var got []Event
	if err := Parse(strings.NewReader(stream), func(e Event) { got = append(got, e) }); err != nil {
		t.Fatalf("Parse: %v", err)
	}

	want := []Event{
		{Name: "", Data: "hello"},
		{Name: "announce", Data: `{"session":"session-1","kind":"progress"}`},
		{Name: "", Data: "line-without-space"},
		{Name: "", Data: "line1\nline2"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d: got %#v, want %#v", i, got[i], want[i])
		}
	}
}

// TestParse_DispatchesOnEOF covers the spec-compliant lenient case:
// a stream ending without a trailing blank line should still
// dispatch the buffered event. We don't expect the bosun server to
// ever skip the blank line, but tolerating it keeps Parse usable
// against fixtures and other SSE producers.
func TestParse_DispatchesOnEOF(t *testing.T) {
	const stream = "data: tail-without-blank-line"

	var got []Event
	if err := Parse(strings.NewReader(stream), func(e Event) { got = append(got, e) }); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(got) != 1 || got[0].Data != "tail-without-blank-line" {
		t.Fatalf("expected 1 event with the partial body, got %#v", got)
	}
}

// TestClient_Stream_AgainstFakeServer runs the full Client.Stream path
// against an httptest server that emits a couple of SSE events then
// closes the connection. With Reconnect=false the client returns on
// EOF — proves the GET + parse + onEvent pipeline is wired up.
func TestClient_Stream_AgainstFakeServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: one\n\n")
		fmt.Fprintf(w, "data: two\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL, Reconnect: false}
	var mu sync.Mutex
	var got []string
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if err := c.Stream(ctx, func(e Event) {
		mu.Lock()
		got = append(got, e.Data)
		mu.Unlock()
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(got) != 2 || got[0] != "one" || got[1] != "two" {
		t.Fatalf("got %#v, want [one two]", got)
	}
}

// TestClient_Stream_4xxNotRetried proves a 404 returns immediately
// even when Reconnect=true. Reconnecting against a permanent wrong-URL
// would burn CPU forever.
func TestClient_Stream_4xxNotRetried(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL, Reconnect: true, ReconnectInitial: 10 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := c.Stream(ctx, func(Event) {})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var nre *nonRetryableError
	if !errors.As(err, &nre) {
		t.Fatalf("expected nonRetryableError, got %T: %v", err, err)
	}
}

// TestClient_Stream_ReconnectsOnEOF starts a server that drops the
// connection after one event, then re-accepts and emits a second
// event. With Reconnect=true and a tiny initial backoff the client
// should consume both events. This is the contract-coverage test for
// the brief's "reconnect cleanly on server restart" requirement.
func TestClient_Stream_ReconnectsOnEOF(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, "data: attempt-%d\n\n", n)
		if flusher != nil {
			flusher.Flush()
		}
		// First two connections close immediately (simulating server
		// restart); the test cancels the context after two events.
	}))
	defer srv.Close()

	c := &Client{
		URL:              srv.URL,
		Reconnect:        true,
		ReconnectInitial: 1 * time.Millisecond,
		ReconnectMax:     10 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var got []string
	done := make(chan struct{})

	go func() {
		_ = c.Stream(ctx, func(e Event) {
			mu.Lock()
			got = append(got, e.Data)
			should := len(got) >= 2
			mu.Unlock()
			if should {
				cancel()
			}
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stream did not return within 3s")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) < 2 {
		t.Fatalf("expected at least 2 events across reconnects, got %#v", got)
	}
	if got[0] != "attempt-1" || got[1] != "attempt-2" {
		t.Fatalf("reconnect ordering broken: %#v", got)
	}
}

// TestClient_Stream_RespectsContext makes sure a cancelled context
// stops Stream promptly even if the server keeps the connection open.
func TestClient_Stream_RespectsContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		fmt.Fprintf(w, ": keep-alive\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	c := &Client{URL: srv.URL, Reconnect: false}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- c.Stream(ctx, func(Event) {}) }()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stream did not return after cancel within 2s")
	}
}

// TestClient_Stream_EmptyURL guards against a misconfigured client
// silently spinning on http.DefaultClient with no host.
func TestClient_Stream_EmptyURL(t *testing.T) {
	c := &Client{}
	err := c.Stream(context.Background(), func(Event) {})
	if err == nil || !strings.Contains(err.Error(), "URL is empty") {
		t.Fatalf("expected empty-URL error, got %v", err)
	}
}
