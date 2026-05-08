package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

// WebhookSignature is the set of headers a webhook receiver needs to verify
// a payload. Modelled after Stripe's signature scheme: timestamp + body are
// HMAC'd together so a stolen header is bound to a specific request body
// and a specific moment in time.
type WebhookSignature struct {
	Header    string // X-Multica-Signature value, e.g. "sha256=abc..."
	Timestamp string // X-Multica-Timestamp value (unix seconds, decimal)
	RuntimeID string // X-Multica-Webhook-Id value
}

// SignWebhook computes the headers the dispatcher should attach to an
// outgoing webhook POST. The HMAC covers timestamp + "." + body so that
// any of (secret, timestamp, body) being wrong invalidates the signature.
//
// Callers should attach all three header values to the outgoing request:
//
//	X-Multica-Signature: <sig.Header>
//	X-Multica-Timestamp: <sig.Timestamp>
//	X-Multica-Webhook-Id: <sig.RuntimeID>
func SignWebhook(body []byte, secret, runtimeID string, now time.Time) WebhookSignature {
	ts := strconv.FormatInt(now.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return WebhookSignature{
		Header:    "sha256=" + hex.EncodeToString(mac.Sum(nil)),
		Timestamp: ts,
		RuntimeID: runtimeID,
	}
}

// VerifyWebhook returns true iff the signature is valid AND the timestamp
// is within ±5 minutes of `now`. Used by the receiver (translator service)
// to reject replays and forgeries. Constant-time comparison via hmac.Equal
// avoids timing oracles on the signature check.
func VerifyWebhook(body []byte, secret, header, timestamp string, now time.Time) bool {
	sentTs, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil {
		return false
	}
	if absInt64(now.Unix()-sentTs) > 300 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(header))
}

func absInt64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
