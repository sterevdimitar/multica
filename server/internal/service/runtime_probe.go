package service

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/multica-ai/multica/server/internal/util"
)

// The availability probe: the first writer of down_until (dev-command-center
// design 2026-09-20-runtime-placement §2). A webhook runtime registers as
// online and is never re-checked by the heartbeat sweeper
// (SelectStaleOnlineRuntimes excludes runtime_mode = 'webhook' on purpose),
// so the page showed a dead PC as Online for as long as it liked. Every
// other sweeper tick this asks each webhook runtime's receiver whether the
// label is served — for the translator: an online runner in a connected
// repository, a wake URL, or nothing to serve (hosted).
//
// It FAILS OPEN (P16): a transport error, a non-200 (an older translator
// answers 400 to the envelope) or an undecodable body writes nothing. A
// row is written only from a parsed {"available": …} answer. The window it
// writes is twice its interval, so a runtime stays down continuously while
// the probe keeps saying so and comes back within a minute of a runner
// appearing — or of the probe stopping.

// runtimeProbeTimeout bounds one probe POST; the sweeper tick must not hang
// on a slow receiver.
const runtimeProbeTimeout = 5 * time.Second

// ProbeRuntimeAvailability probes every webhook runtime in every workspace
// once. Returns how many answered and how many of those said no, for the
// sweeper's log line. Flag-gated like every other webhook path.
func (s *TaskService) ProbeRuntimeAvailability(ctx context.Context) (probed, down int) {
	if os.Getenv("MULTICA_WEBHOOK_RUNTIME") != "1" {
		return 0, 0
	}
	runtimes, err := s.Queries.ListAllWebhookRuntimes(ctx)
	if err != nil {
		slog.Warn("runtime probe: list webhook runtimes", "err", err)
		return 0, 0
	}
	for _, r := range runtimes {
		if !r.WebhookUrl.Valid || r.WebhookUrl.String == "" {
			continue
		}
		target := DispatchTarget{
			URL:       r.WebhookUrl.String,
			Secret:    r.WebhookSecret.String,
			RuntimeID: util.UUIDToString(r.ID),
			EventType: r.WebhookEventType.String,
		}
		pctx, cancel := context.WithTimeout(ctx, runtimeProbeTimeout)
		answer, err := ProbeWebhookAvailability(pctx, target, webhookHTTPClient, time.Now)
		cancel()
		if err != nil {
			// Fail open: nothing is written. Debug, not warn — an older
			// translator answers 400 every minute until it is deployed.
			slog.Debug("runtime probe: no answer; row untouched", "err", err, "runtime", runtimeDisplayName(r))
			continue
		}
		probed++
		if answer.Available {
			s.markRuntimeUp(ctx, r.ID)
			continue
		}
		down++
		s.markRuntimeDown(ctx, r.ID, time.Now().Add(runtimeProbeDownWindow), answer.Reason)
	}
	return probed, down
}
