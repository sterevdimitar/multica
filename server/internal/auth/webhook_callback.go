package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// CallbackSubject identifies tokens issued for webhook-runtime task callbacks.
// Verified on parse so a leaked PAT/JWT can't be used in place of a callback
// token (and vice versa).
const CallbackSubject = "multica-webhook-callback"

// CallbackClaims is the structured shape of a per-task callback token. The
// token is HMAC-signed (HS256) with the project's existing JWT secret so the
// existing middleware infrastructure can validate it without managing a new
// key.
type CallbackClaims struct {
	TaskID    string `json:"task_id"`
	RuntimeID string `json:"runtime_id"`
	jwt.RegisteredClaims
}

// IssueCallbackToken creates a token bound to a specific (task, runtime)
// pair. The webhook receiver attaches this token in Authorization on every
// callback POST to /messages, /usage, /complete, /fail.
func IssueCallbackToken(secret []byte, taskID, runtimeID string, ttl time.Duration) (string, error) {
	return IssueCallbackTokenAt(secret, taskID, runtimeID, ttl, time.Now())
}

// IssueCallbackTokenAt is the test-friendly form that takes an explicit
// "now" so unit tests can produce already-expired tokens.
func IssueCallbackTokenAt(secret []byte, taskID, runtimeID string, ttl time.Duration, now time.Time) (string, error) {
	claims := CallbackClaims{
		TaskID:    taskID,
		RuntimeID: runtimeID,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   CallbackSubject,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(secret)
}

// ParseCallbackToken validates the signature, expiry, and subject of a
// callback token and returns its claims. Returns a non-nil error for any of:
// bad signature, expired token, wrong signing method, wrong subject.
func ParseCallbackToken(secret []byte, raw string) (*CallbackClaims, error) {
	out := &CallbackClaims{}
	tok, err := jwt.ParseWithClaims(raw, out, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return secret, nil
	})
	if err != nil {
		return nil, err
	}
	if !tok.Valid {
		return nil, errors.New("invalid token")
	}
	if out.Subject != CallbackSubject {
		// A user JWT or PAT can't be repurposed as a callback token.
		return nil, errors.New("not a callback token")
	}
	if out.TaskID == "" || out.RuntimeID == "" {
		return nil, errors.New("missing task_id or runtime_id claim")
	}
	return out, nil
}
