package service

import (
	"testing"
	"time"
)

func TestSignWebhook_DeterministicAndVerifiable(t *testing.T) {
	body := []byte(`{"task":"hello"}`)
	secret := "s3cr3t"
	ts := time.Unix(1700000000, 0)
	runtimeID := "rid-123"

	sig := SignWebhook(body, secret, runtimeID, ts)

	if sig.Timestamp != "1700000000" {
		t.Errorf("Timestamp = %q, want %q", sig.Timestamp, "1700000000")
	}
	if sig.RuntimeID != runtimeID {
		t.Errorf("RuntimeID = %q, want %q", sig.RuntimeID, runtimeID)
	}
	if len(sig.Header) <= len("sha256=") {
		t.Fatalf("Header empty: %q", sig.Header)
	}

	// Re-signing with the same inputs yields the same header (determinism).
	sig2 := SignWebhook(body, secret, runtimeID, ts)
	if sig.Header != sig2.Header {
		t.Errorf("non-deterministic: %q vs %q", sig.Header, sig2.Header)
	}

	if !VerifyWebhook(body, secret, sig.Header, sig.Timestamp, time.Unix(1700000010, 0)) {
		t.Errorf("verify should pass for valid signature within window")
	}
}

func TestVerifyWebhook_RejectsWrongSecret(t *testing.T) {
	body := []byte(`{"x":1}`)
	ts := time.Unix(1700000000, 0)
	sig := SignWebhook(body, "good", "rid", ts)
	if VerifyWebhook(body, "bad", sig.Header, sig.Timestamp, ts) {
		t.Errorf("verify should reject wrong secret")
	}
}

func TestVerifyWebhook_RejectsTamperedBody(t *testing.T) {
	ts := time.Unix(1700000000, 0)
	sig := SignWebhook([]byte(`{"x":1}`), "s", "rid", ts)
	if VerifyWebhook([]byte(`{"x":2}`), "s", sig.Header, sig.Timestamp, ts) {
		t.Errorf("verify should reject body tampering")
	}
}

func TestVerifyWebhook_RejectsStaleTimestamp(t *testing.T) {
	body := []byte(`{"x":1}`)
	ts := time.Unix(1700000000, 0)
	sig := SignWebhook(body, "s", "rid", ts)
	// 6 minutes later — outside the 5-minute replay window.
	if VerifyWebhook(body, "s", sig.Header, sig.Timestamp, ts.Add(6*time.Minute)) {
		t.Errorf("verify should reject stale timestamp")
	}
	// 4 minutes later — still in window.
	if !VerifyWebhook(body, "s", sig.Header, sig.Timestamp, ts.Add(4*time.Minute)) {
		t.Errorf("verify should accept timestamp within window")
	}
	// 4 minutes before "now" (clock skew on the receiver's side, sender's
	// clock is ahead) — should still verify.
	if !VerifyWebhook(body, "s", sig.Header, sig.Timestamp, ts.Add(-4*time.Minute)) {
		t.Errorf("verify should accept negative skew within window")
	}
}

func TestVerifyWebhook_RejectsMalformedTimestamp(t *testing.T) {
	body := []byte(`{}`)
	if VerifyWebhook(body, "s", "sha256=00", "not-a-number", time.Now()) {
		t.Errorf("verify should reject non-numeric timestamp")
	}
}
