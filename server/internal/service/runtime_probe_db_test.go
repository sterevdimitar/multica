package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// The probe against a real row. Needs DATABASE_URL (port 5439 on this
// machine); skips without it.

func pointRuntimeAt(t *testing.T, ctx context.Context, pool *pgxpool.Pool, runtimeID, url string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `UPDATE agent_runtime SET webhook_url = $2 WHERE id = $1`, runtimeID, url); err != nil {
		t.Fatalf("point runtime: %v", err)
	}
}

func TestProbeRuntimeAvailabilityWritesAndFailsOpen(t *testing.T) {
	ctx := context.Background()
	pool := newTaskClaimRacePool(t)
	withWebhookRuntimeFlag(t)
	svc := NewTaskService(db.New(pool), pool, nil, events.New())
	_, _, _, runtimeID := newLifecycleFixture(t, ctx, pool, lifecycleFixtureOpts{prefix: "PROBE", number: 720010, webhook: true}, StatusTodo)

	var status atomic.Int32
	var body atomic.Value
	status.Store(http.StatusOK)
	body.Store(`{"available":false,"reason":"no online runner carries \"probe\" in o/r","checked_at":"2026-09-20T12:00:00Z"}`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Multica-Signature") == "" {
			t.Error("probe not signed")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(body.Load().(string)))
	}))
	defer srv.Close()
	pointRuntimeAt(t, ctx, pool, runtimeID, srv.URL)
	load := func() db.AgentRuntime {
		r, err := svc.Queries.GetAgentRuntime(ctx, util.MustParseUUID(runtimeID))
		if err != nil {
			t.Fatalf("load runtime: %v", err)
		}
		return r
	}

	// available:false → down for ~2 min with the reason, checked_at stamped.
	svc.ProbeRuntimeAvailability(ctx)
	r := load()
	if !r.DownUntil.Valid || r.DownReason.String != `no online runner carries "probe" in o/r` {
		t.Fatalf("after a no: down_until valid=%v reason=%q", r.DownUntil.Valid, r.DownReason.String)
	}
	if until := time.Until(r.DownUntil.Time); until < 90*time.Second || until > 150*time.Second {
		t.Errorf("down window %s, want ~2 min", until)
	}
	if !r.AvailabilityCheckedAt.Valid {
		t.Error("availability_checked_at not stamped")
	}

	// A 400 (an older translator) leaves the row untouched (P16).
	status.Store(http.StatusBadRequest)
	body.Store("envelope missing task or callback")
	before := load()
	svc.ProbeRuntimeAvailability(ctx)
	after := load()
	if !after.DownUntil.Valid || !after.DownUntil.Time.Equal(before.DownUntil.Time) || after.DownReason.String != before.DownReason.String {
		t.Error("a 400 must not touch the row")
	}

	// available:true → both cleared.
	status.Store(http.StatusOK)
	body.Store(`{"available":true,"reason":"online in o/r","checked_at":"2026-09-20T12:01:00Z"}`)
	svc.ProbeRuntimeAvailability(ctx)
	r = load()
	if r.DownUntil.Valid || r.DownReason.Valid {
		t.Errorf("after a yes: down_until valid=%v reason valid=%v, want both NULL", r.DownUntil.Valid, r.DownReason.Valid)
	}

	// Transport error → untouched.
	setDown(t, ctx, pool, runtimeID, "still down")
	srv.Close()
	svc.ProbeRuntimeAvailability(ctx)
	if r = load(); !r.DownUntil.Valid || r.DownReason.String != "still down" {
		t.Error("a transport error must not touch the row")
	}
}
