package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// DispatchTarget bundles the per-runtime fields needed to fire a single
// webhook POST. Built fresh per dispatch from the runtime's stored config so
// there's no cached secret to invalidate when a runtime rotates its
// webhook_secret via re-registration.
type DispatchTarget struct {
	URL       string
	Secret    string
	RuntimeID string
	EventType string
}

// RetryPolicy controls how DispatchWithRetry handles transient failures.
// MaxAttempts includes the first attempt, so MaxAttempts=3 means
// "first try, then 2 retries". InitialBackoff doubles between retries.
type RetryPolicy struct {
	MaxAttempts    int
	InitialBackoff time.Duration
}

// DispatchToWebhook POSTs `payload` (JSON-marshaled) to target.URL with the
// Multica HMAC headers. Returns nil on 2xx, error otherwise. `clock` is the
// time source used for the signature timestamp — production uses time.Now,
// tests can pass a fixed-clock function.
//
// The full set of headers attached:
//
//	Content-Type: application/json
//	X-Multica-Signature: sha256=<hex>
//	X-Multica-Timestamp: <unix>
//	X-Multica-Webhook-Id: <runtime_id>
//	X-Multica-Event-Type: <event_type, when set on the runtime>
func DispatchToWebhook(ctx context.Context, target DispatchTarget, payload any, client *http.Client, clock func() time.Time) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
	}
	sig := SignWebhook(body, target.Secret, target.RuntimeID, clock())

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Multica-Signature", sig.Header)
	req.Header.Set("X-Multica-Timestamp", sig.Timestamp)
	req.Header.Set("X-Multica-Webhook-Id", sig.RuntimeID)
	if target.EventType != "" {
		req.Header.Set("X-Multica-Event-Type", target.EventType)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Read up to 1 KiB so the error message includes the receiver's
		// rejection reason without unbounded memory use on adversarial bodies.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("webhook returned %d: %s", resp.StatusCode, snippet)
	}
	return nil
}

// DispatchWithRetry wraps DispatchToWebhook with exponential backoff. Returns
// nil on the first successful attempt, or the last error after exhausting
// attempts. Aborts immediately if ctx is cancelled mid-backoff.
func DispatchWithRetry(ctx context.Context, target DispatchTarget, payload any, client *http.Client, clock func() time.Time, policy RetryPolicy) error {
	if policy.MaxAttempts < 1 {
		policy.MaxAttempts = 3
	}
	if policy.InitialBackoff <= 0 {
		policy.InitialBackoff = 500 * time.Millisecond
	}

	var lastErr error
	backoff := policy.InitialBackoff
	for i := 0; i < policy.MaxAttempts; i++ {
		if err := DispatchToWebhook(ctx, target, payload, client, clock); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if i == policy.MaxAttempts-1 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
			backoff *= 2
		}
	}
	return fmt.Errorf("after %d attempts: %w", policy.MaxAttempts, lastErr)
}
