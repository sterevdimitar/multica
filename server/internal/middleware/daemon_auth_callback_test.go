package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/multica-ai/multica/server/internal/auth"
)

// recordingHandler is a tiny next-handler stand-in that captures the
// final request's context so the test can inspect what DaemonAuth set.
type recordingHandler struct {
	got *http.Request
}

func (h *recordingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.got = r
	w.WriteHeader(http.StatusOK)
}

func runDaemonAuth(t *testing.T, tokenValue string) (status int, gotReq *http.Request) {
	t.Helper()
	rec := &recordingHandler{}
	// patCache=nil and daemonCache=nil are tolerated by the mdt_/mul_
	// branches (they fail closed with "invalid daemon token" / "invalid
	// token"). queries=nil same — but since the callback-JWT branch
	// doesn't touch them, the test exercises ONLY the callback path.
	// cloudPAT=nil too (added upstream): the mcn_ branch fails closed and
	// the callback-JWT branch under test never touches it.
	handler := DaemonAuth(nil, nil, nil, nil)(rec)

	req := httptest.NewRequest(http.MethodPost, "/api/daemon/tasks/abc/messages", nil)
	if tokenValue != "" {
		req.Header.Set("Authorization", "Bearer "+tokenValue)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w.Code, rec.got
}

func TestDaemonAuth_CallbackJWT_SetsContextOnSuccess(t *testing.T) {
	secret := auth.JWTSecret()
	tok, err := auth.IssueCallbackToken(secret, "task-abc", "rt-xyz", 10*time.Minute)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	status, gotReq := runDaemonAuth(t, tok)
	if status != http.StatusOK {
		t.Fatalf("expected 200 (passthrough), got %d", status)
	}
	if gotReq == nil {
		t.Fatalf("next handler never ran")
	}
	if DaemonAuthPathFromContext(gotReq.Context()) != DaemonAuthPathCallbackJWT {
		t.Errorf("auth path = %q, want %q",
			DaemonAuthPathFromContext(gotReq.Context()), DaemonAuthPathCallbackJWT)
	}
	if got := CallbackTaskIDFromContext(gotReq.Context()); got != "task-abc" {
		t.Errorf("CallbackTaskID = %q, want task-abc", got)
	}
	if got := CallbackRuntimeIDFromContext(gotReq.Context()); got != "rt-xyz" {
		t.Errorf("CallbackRuntimeID = %q, want rt-xyz", got)
	}
}

func TestDaemonAuth_CallbackJWT_RejectsExpired(t *testing.T) {
	tok, _ := auth.IssueCallbackTokenAt(auth.JWTSecret(), "t", "r",
		1*time.Minute, time.Now().Add(-2*time.Hour))

	status, gotReq := runDaemonAuth(t, tok)
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 on expired callback JWT, got %d", status)
	}
	if gotReq != nil {
		t.Errorf("next handler should not have run")
	}
}

func TestDaemonAuth_CallbackJWT_RejectsWrongSecret(t *testing.T) {
	tok, _ := auth.IssueCallbackToken([]byte("not-the-server-secret"), "t", "r", time.Minute)

	status, gotReq := runDaemonAuth(t, tok)
	// Wrong secret means ParseCallbackToken fails → falls through to the
	// generic-user-JWT path → which also fails signature check → 401.
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", status)
	}
	if gotReq != nil {
		t.Errorf("next handler should not have run")
	}
}

func TestDaemonAuth_UserJWTNotMisclassifiedAsCallback(t *testing.T) {
	// Build a token signed by the right secret but with `sub="user-id"`,
	// not "multica-webhook-callback". ParseCallbackToken must reject it
	// (subject mismatch), and the generic JWT fallback should pick it up
	// instead — so the request authenticates as DaemonAuthPathJWT, NOT
	// DaemonAuthPathCallbackJWT. Critical for security: a stolen user
	// JWT must never grant callback-token privileges.
	claims := jwt.MapClaims{
		"sub": "user-xyz",
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(auth.JWTSecret())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	status, gotReq := runDaemonAuth(t, tok)
	if status != http.StatusOK {
		t.Fatalf("expected 200 (passthrough on generic JWT), got %d", status)
	}
	if got := DaemonAuthPathFromContext(gotReq.Context()); got != DaemonAuthPathJWT {
		t.Errorf("user JWT auth path = %q, want %q (callback path is a security regression)",
			got, DaemonAuthPathJWT)
	}
	if got := CallbackTaskIDFromContext(gotReq.Context()); got != "" {
		t.Errorf("user JWT must not populate CallbackTaskID; got %q", got)
	}
}

func TestDaemonAuth_CallbackJWT_RejectsMissingHeaders(t *testing.T) {
	status, _ := runDaemonAuth(t, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("expected 401 on missing header, got %d", status)
	}
}

func TestWithCallbackContext_RoundTrip(t *testing.T) {
	ctx := WithCallbackContext(context.Background(), "task-1", "rt-1")
	if got := CallbackTaskIDFromContext(ctx); got != "task-1" {
		t.Errorf("CallbackTaskID = %q, want task-1", got)
	}
	if got := CallbackRuntimeIDFromContext(ctx); got != "rt-1" {
		t.Errorf("CallbackRuntimeID = %q, want rt-1", got)
	}
	if got := DaemonAuthPathFromContext(ctx); got != DaemonAuthPathCallbackJWT {
		t.Errorf("auth path = %q, want %q", got, DaemonAuthPathCallbackJWT)
	}
}
