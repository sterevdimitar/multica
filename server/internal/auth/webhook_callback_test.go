package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestCallbackToken_RoundTrip(t *testing.T) {
	secret := []byte("server-jwt-secret")
	taskID := "task-abc"
	runtimeID := "rt-xyz"

	tok, err := IssueCallbackToken(secret, taskID, runtimeID, 60*time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	claims, err := ParseCallbackToken(secret, tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if claims.TaskID != taskID || claims.RuntimeID != runtimeID {
		t.Errorf("claims = %+v, want task_id=%q runtime_id=%q", claims, taskID, runtimeID)
	}
	if claims.Subject != CallbackSubject {
		t.Errorf("Subject = %q, want %q", claims.Subject, CallbackSubject)
	}
}

func TestCallbackToken_RejectsWrongSecret(t *testing.T) {
	tok, _ := IssueCallbackToken([]byte("a"), "t", "r", time.Minute)
	if _, err := ParseCallbackToken([]byte("b"), tok); err == nil {
		t.Errorf("expected error on wrong secret")
	}
}

func TestCallbackToken_RejectsExpired(t *testing.T) {
	tok, _ := IssueCallbackTokenAt([]byte("a"), "t", "r", time.Minute, time.Now().Add(-2*time.Hour))
	if _, err := ParseCallbackToken([]byte("a"), tok); err == nil {
		t.Errorf("expected error on expired token")
	}
}

func TestCallbackToken_RejectsWrongSubject(t *testing.T) {
	// A token with the right secret but a different subject (e.g. a user
	// JWT) must not authenticate webhook callbacks.
	secret := []byte("a")
	wrongSubject := jwt.NewWithClaims(jwt.SigningMethodHS256, CallbackClaims{
		TaskID:    "t",
		RuntimeID: "r",
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "user-session",
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	signed, _ := wrongSubject.SignedString(secret)
	if _, err := ParseCallbackToken(secret, signed); err == nil {
		t.Errorf("expected error on wrong subject")
	}
}

func TestCallbackToken_RejectsMissingClaims(t *testing.T) {
	secret := []byte("a")
	missing := jwt.NewWithClaims(jwt.SigningMethodHS256, CallbackClaims{
		// TaskID and RuntimeID intentionally empty.
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   CallbackSubject,
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	})
	signed, _ := missing.SignedString(secret)
	if _, err := ParseCallbackToken(secret, signed); err == nil {
		t.Errorf("expected error on missing claims")
	}
}
