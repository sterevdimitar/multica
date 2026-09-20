package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestDispatchToWebhook_PostsSignedPayload(t *testing.T) {
	var (
		gotBody    []byte
		gotHeaders http.Header
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	payload := map[string]any{
		"task":     map[string]any{"id": "t1"},
		"callback": map[string]any{"url": "https://m/", "token": "tok"},
	}
	err := DispatchToWebhook(context.Background(), DispatchTarget{
		URL:       srv.URL,
		Secret:    "s3cr3t",
		RuntimeID: "rt-1",
		EventType: "multica-task",
	}, payload, http.DefaultClient, time.Now)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}

	if gotHeaders.Get("X-Multica-Signature") == "" {
		t.Errorf("missing X-Multica-Signature")
	}
	if gotHeaders.Get("X-Multica-Timestamp") == "" {
		t.Errorf("missing X-Multica-Timestamp")
	}
	if gotHeaders.Get("X-Multica-Webhook-Id") != "rt-1" {
		t.Errorf("X-Multica-Webhook-Id = %q", gotHeaders.Get("X-Multica-Webhook-Id"))
	}
	if gotHeaders.Get("X-Multica-Event-Type") != "multica-task" {
		t.Errorf("X-Multica-Event-Type = %q", gotHeaders.Get("X-Multica-Event-Type"))
	}

	var got map[string]any
	if err := json.Unmarshal(gotBody, &got); err != nil {
		t.Fatalf("body not json: %v\n%s", err, gotBody)
	}
	if got["task"] == nil || got["callback"] == nil {
		t.Errorf("payload missing fields: %v", got)
	}

	// Signature on the wire actually verifies against the same secret.
	if !VerifyWebhook(gotBody, "s3cr3t",
		gotHeaders.Get("X-Multica-Signature"),
		gotHeaders.Get("X-Multica-Timestamp"),
		time.Now()) {
		t.Errorf("dispatched signature should self-verify")
	}
}

func TestDispatchToWebhook_OmitsEventTypeHeaderWhenEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Multica-Event-Type") != "" {
			t.Errorf("expected no X-Multica-Event-Type header, got %q", r.Header.Get("X-Multica-Event-Type"))
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	if err := DispatchToWebhook(context.Background(), DispatchTarget{
		URL: srv.URL, Secret: "s", RuntimeID: "r", EventType: "",
	}, map[string]any{"x": 1}, http.DefaultClient, time.Now); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
}

func TestDispatchToWebhook_ReturnsErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("translator says no"))
	}))
	defer srv.Close()

	err := DispatchToWebhook(context.Background(), DispatchTarget{
		URL: srv.URL, Secret: "s", RuntimeID: "r", EventType: "e",
	}, map[string]any{"x": 1}, http.DefaultClient, time.Now)
	if err == nil {
		t.Fatalf("expected error on 500")
	}
	if msg := err.Error(); !contains(msg, "500") || !contains(msg, "translator says no") {
		t.Errorf("error should mention status and body, got %q", msg)
	}
}

func TestDispatchWithRetry_SucceedsAfterTransientFailures(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	err := DispatchWithRetry(context.Background(), DispatchTarget{
		URL: srv.URL, Secret: "s", RuntimeID: "r", EventType: "e",
	}, map[string]any{"x": 1}, http.DefaultClient, time.Now,
		RetryPolicy{MaxAttempts: 5, InitialBackoff: 5 * time.Millisecond})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected exactly 3 calls (2 fails + 1 success), got %d", got)
	}
}

func TestDispatchWithRetry_GivesUpAfterMaxAttempts(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	err := DispatchWithRetry(context.Background(), DispatchTarget{
		URL: srv.URL, Secret: "s", RuntimeID: "r", EventType: "e",
	}, map[string]any{"x": 1}, http.DefaultClient, time.Now,
		RetryPolicy{MaxAttempts: 3, InitialBackoff: 1 * time.Millisecond})
	if err == nil {
		t.Fatalf("expected error after exhausting attempts")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected exactly 3 attempts, got %d", got)
	}
}

func TestDispatchWithRetry_AbortsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before first attempt completes

	err := DispatchWithRetry(ctx, DispatchTarget{
		URL: srv.URL, Secret: "s", RuntimeID: "r", EventType: "e",
	}, map[string]any{"x": 1}, http.DefaultClient, time.Now,
		RetryPolicy{MaxAttempts: 5, InitialBackoff: 1 * time.Second})
	if err == nil {
		t.Fatalf("expected error on cancelled context")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// A 4xx is a rejection, not a transient failure (runtime placement P10): the
// receiver understood the request and refused it, so retrying it three times
// over ~15 s only delays the placement that must follow.
func TestDispatchWithRetry_DoesNotRetryA409(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"reason":"runtime_unavailable","detail":"no online runner carries \"local-pc\" in o/r"}`))
	}))
	defer srv.Close()

	err := DispatchWithRetry(context.Background(), DispatchTarget{
		URL: srv.URL, Secret: "s", RuntimeID: "r", EventType: "e",
	}, map[string]any{"x": 1}, http.DefaultClient, time.Now,
		RetryPolicy{MaxAttempts: 3, InitialBackoff: 1 * time.Millisecond})
	if err == nil {
		t.Fatal("expected an error on 409")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected exactly 1 attempt on a 409, got %d", got)
	}
	var rejected *WebhookRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("error is %T (%v), want *WebhookRejectedError", err, err)
	}
	if rejected.Status != http.StatusConflict {
		t.Errorf("Status = %d, want 409", rejected.Status)
	}
	detail, ok := rejected.RuntimeUnavailable()
	if !ok {
		t.Fatal("RuntimeUnavailable() = false, want true for the translator's 409 body")
	}
	if detail != `no online runner carries "local-pc" in o/r` {
		t.Errorf("detail = %q", detail)
	}
}

func TestDispatchWithRetry_A400IsRejectedButNotRuntimeUnavailable(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad runs_on"))
	}))
	defer srv.Close()

	err := DispatchWithRetry(context.Background(), DispatchTarget{
		URL: srv.URL, Secret: "s", RuntimeID: "r", EventType: "e",
	}, map[string]any{"x": 1}, http.DefaultClient, time.Now,
		RetryPolicy{MaxAttempts: 3, InitialBackoff: 1 * time.Millisecond})
	var rejected *WebhookRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("error is %T (%v), want *WebhookRejectedError", err, err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("expected exactly 1 attempt on a 400, got %d", got)
	}
	if _, ok := rejected.RuntimeUnavailable(); ok {
		t.Error("a 400 must not read as runtime_unavailable")
	}
	if !contains(err.Error(), "400") || !contains(err.Error(), "bad runs_on") {
		t.Errorf("error should mention status and body, got %q", err.Error())
	}
}

// A 5xx keeps today's shape: retried, then the wrapped error.
func TestDispatchWithRetry_A503IsStillRetried(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	err := DispatchWithRetry(context.Background(), DispatchTarget{
		URL: srv.URL, Secret: "s", RuntimeID: "r", EventType: "e",
	}, map[string]any{"x": 1}, http.DefaultClient, time.Now,
		RetryPolicy{MaxAttempts: 3, InitialBackoff: 1 * time.Millisecond})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected 3 attempts on a 503, got %d", got)
	}
	var rejected *WebhookRejectedError
	if errors.As(err, &rejected) {
		t.Error("a 5xx must not be a WebhookRejectedError")
	}
}

// The availability probe: a signed POST of {"event":"availability"} whose
// 200 body is the answer; anything else is an error the caller ignores.
func TestProbeWebhookAvailability(t *testing.T) {
	var gotBody []byte
	var gotHeaders http.Header
	status := http.StatusOK
	answer := `{"available":false,"reason":"no online runner carries \"local-pc\" in o/r","checked_at":"2026-09-20T12:00:00Z"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	defer srv.Close()
	target := DispatchTarget{URL: srv.URL, Secret: "s", RuntimeID: "r", EventType: "e"}

	got, err := ProbeWebhookAvailability(context.Background(), target, http.DefaultClient, time.Now)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got.Available || got.Reason != `no online runner carries "local-pc" in o/r` {
		t.Errorf("answer = %+v", got)
	}
	if got.CheckedAt.IsZero() {
		t.Error("checked_at not decoded")
	}
	if string(gotBody) != `{"event":"availability"}` {
		t.Errorf("probe body = %s", gotBody)
	}
	if gotHeaders.Get("X-Multica-Signature") == "" || gotHeaders.Get("X-Multica-Timestamp") == "" {
		t.Error("probe is not signed")
	}

	status = http.StatusBadRequest
	answer = "envelope missing task or callback"
	if _, err := ProbeWebhookAvailability(context.Background(), target, http.DefaultClient, time.Now); err == nil {
		t.Error("a 400 (an older translator) must be an error, not an answer")
	}

	status = http.StatusOK
	answer = "not json"
	if _, err := ProbeWebhookAvailability(context.Background(), target, http.DefaultClient, time.Now); err == nil {
		t.Error("an undecodable 200 must be an error")
	}
}
