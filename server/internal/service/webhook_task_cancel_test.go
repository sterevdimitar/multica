package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestShouldDispatchCancel(t *testing.T) {
	cases := []struct {
		name        string
		flagOn      bool
		runtimeMode string
		webhookURL  string
		dispatched  bool
		want        bool
	}{
		{"webhook runtime, dispatched", true, "webhook", "https://x.test/hook", true, true},
		{"flag off", false, "webhook", "https://x.test/hook", true, false},
		{"local runtime", true, "local", "https://x.test/hook", true, false},
		{"never dispatched", true, "webhook", "https://x.test/hook", false, false},
		{"webhook mode but no url", true, "webhook", "", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldDispatchCancel(tc.flagOn, tc.runtimeMode, tc.webhookURL, tc.dispatched)
			if got != tc.want {
				t.Errorf("shouldDispatchCancel = %v, want %v", got, tc.want)
			}
		})
	}
}

type capturedCancel struct {
	mu        sync.Mutex
	bodies    [][]byte
	signature []string
	timestamp []string
}

func (c *capturedCancel) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func TestDispatchCancelEventWireFormat(t *testing.T) {
	cap := &capturedCancel{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cap.mu.Lock()
		cap.bodies = append(cap.bodies, b)
		cap.signature = append(cap.signature, r.Header.Get("X-Multica-Signature"))
		cap.timestamp = append(cap.timestamp, r.Header.Get("X-Multica-Timestamp"))
		cap.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	target := DispatchTarget{
		URL:       srv.URL,
		Secret:    "shhh",
		RuntimeID: "11111111-2222-3333-4444-555555555555",
	}
	dispatchCancelEvent(target, "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", srv.Client())
	waitFor(t, func() bool { return cap.count() >= 1 })

	if got := cap.count(); got != 1 {
		t.Fatalf("received %d POSTs, want exactly 1", got)
	}

	cap.mu.Lock()
	body, sig, ts := cap.bodies[0], cap.signature[0], cap.timestamp[0]
	cap.mu.Unlock()

	var env struct {
		Event string `json:"event"`
		Task  struct {
			ID        string `json:"id"`
			RuntimeID string `json:"runtime_id"`
		} `json:"task"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.Event != CancelEventType {
		t.Errorf("event = %q, want %q", env.Event, CancelEventType)
	}
	if env.Task.ID != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Errorf("task.id = %q", env.Task.ID)
	}
	if env.Task.RuntimeID != target.RuntimeID {
		t.Errorf("task.runtime_id = %q", env.Task.RuntimeID)
	}

	// The same HMAC discipline as dispatch. An unsigned cancel path would
	// be a DoS primitive: anyone who could reach the receiver could kill
	// any run.
	if !VerifyWebhook(body, target.Secret, sig, ts, time.Now()) {
		t.Error("signature does not verify against the runtime secret")
	}
	if VerifyWebhook(body, "wrong-secret", sig, ts, time.Now()) {
		t.Error("signature verified under the wrong secret")
	}
}

func TestDispatchCancelEventStopsAfterThreeAttempts(t *testing.T) {
	cap := &capturedCancel{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		cap.mu.Lock()
		cap.bodies = append(cap.bodies, b)
		cap.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	dispatchCancelEvent(DispatchTarget{URL: srv.URL, Secret: "s", RuntimeID: "r"}, "t", srv.Client())
	waitFor(t, func() bool { return cap.count() >= 3 })
	// Give a fourth attempt a chance to arrive before asserting there is none.
	time.Sleep(200 * time.Millisecond)
	if got := cap.count(); got != 3 {
		t.Errorf("attempts = %d, want exactly 3 — a cancel storm is worse than a wasted run", got)
	}
}

// waitFor polls cond for up to 10s. The dispatch is fire-and-forget in a
// goroutine with a 1s initial backoff doubling per retry, so three attempts
// take ~3s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}
