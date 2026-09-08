package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// ── Engine catalogue ──
//
// An engine is the PAIR (model, base URL). The model says which weights run;
// the base URL says which company runs them. Sending a proxied model name to
// Anthropic — or an Anthropic model name to the proxy — fails as "the model
// does not exist", which reads as a model problem and is not one. So the two
// halves are never surfaced separately: the picker offers models, and the
// base URL is derived from the model through this catalogue at dispatch time.
//
// The catalogue has two sources:
//
//   - the LiteLLM proxy's own /v1/models, one entry per catalogue name it
//     serves (e.g. dcc-glm-high). These carry the proxy's PUBLIC URL, because
//     the agent runs on a GitHub Actions runner, not on the host that runs
//     the proxy.
//   - claudeStaticModels(), Anthropic's own API. These carry an EMPTY base
//     URL. Empty is the normal case and means Anthropic; it is deliberately
//     not spelled out as https://api.anthropic.com, which is the established
//     convention on the pipeline side and what Claude Code reaches by
//     exporting nothing at all.

// Engine is one catalogue entry: a model plus the base URL that model must be
// sent to. BaseURL is never serialised to a client — it is derived, not
// chosen, and exposing it as a field would make the pair separable again.
type Engine struct {
	Model   Model
	BaseURL string
}

// Catalog is a resolved engine catalogue.
//
// ProxyOK reports whether the proxied half is trustworthy: true when the
// proxy answered at some point (entries may be served from cache), false when
// it has never answered since this process started. Callers that translate a
// model into a base URL MUST honour it — with ProxyOK false, "this model is
// not in the catalogue" cannot be distinguished from "the proxy is down", and
// treating the second as the first silently redirects a proxied agent to
// Anthropic, where its model name does not exist.
type Catalog struct {
	Engines []Engine
	ProxyOK bool
}

// Lookup resolves a model string to its engine. The zero Engine and false
// mean the model is not in the catalogue.
func (c Catalog) Lookup(model string) (Engine, bool) {
	for _, e := range c.Engines {
		if e.Model.ID == model {
			return e, true
		}
	}
	return Engine{}, false
}

// Models flattens the catalogue for the model picker. Base URLs are dropped
// here on purpose: the client picks a model and never sees, or sets, the
// other half of the pair.
func (c Catalog) Models() []Model {
	out := make([]Model, 0, len(c.Engines))
	for _, e := range c.Engines {
		out = append(out, e.Model)
	}
	return out
}

const (
	// defaultLiteLLMInternalURL is where the proxy answers from inside the
	// compose network on the production VM. Overridden by
	// LITELLM_INTERNAL_URL for any other deployment; a deployment with no
	// proxy never gets an answer and degrades to Anthropic-only.
	defaultLiteLLMInternalURL = "http://litellm:4000"
	// engineCatalogTTL bounds how long a successful proxy fetch is reused.
	// The catalogue changes only when someone edits the proxy config and
	// restarts it, so a minute is generous; it also bounds how long a
	// removed entry survives after the proxy comes back.
	engineCatalogTTL = 60 * time.Second
	// engineCatalogTimeout caps one fetch. This runs on the model-picker
	// request path and on the webhook dispatch path, neither of which may
	// stall on a wedged proxy.
	engineCatalogTimeout = 3 * time.Second
	// engineProviderLiteLLM groups proxied entries under their own heading
	// in the create dialog's picker, so it is visible at a glance that a
	// model is not Anthropic's.
	engineProviderLiteLLM = "litellm"
)

// liteLLMInternalURL is the URL this process reaches the proxy on.
func liteLLMInternalURL() string {
	if v := strings.TrimSpace(os.Getenv("LITELLM_INTERNAL_URL")); v != "" {
		return v
	}
	return defaultLiteLLMInternalURL
}

// liteLLMPublicURL is the URL the AGENT reaches the proxy on. It differs from
// the internal one whenever the agent does not run on the host that runs the
// proxy — which is the whole point of the webhook runtime, where the agent is
// a GitHub-hosted runner. Falls back to the internal URL so a single-host
// deployment needs one variable, not two.
func liteLLMPublicURL() string {
	if v := strings.TrimSpace(os.Getenv("LITELLM_PUBLIC_URL")); v != "" {
		return v
	}
	return liteLLMInternalURL()
}

// engineHTTPClient is the client used to reach the proxy. A package variable
// so tests can point it at an httptest server; immutable in production.
var engineHTTPClient = &http.Client{Timeout: engineCatalogTimeout}

type engineCacheEntry struct {
	engines   []Engine
	fetchedAt time.Time
}

var (
	engineCacheMu sync.Mutex
	engineCache   *engineCacheEntry
)

// LoadEngineCatalog assembles the catalogue. It never returns an error and
// never returns an empty list: a proxy that cannot be reached costs the
// proxied entries and nothing else, and the Anthropic entries are static.
//
// That is a deliberate contract. The failure mode it replaces was an empty
// model picker, which read as "this runtime has no models" and hid the real
// cause for as long as anyone cared to look.
func LoadEngineCatalog(ctx context.Context) Catalog {
	engines := make([]Engine, 0, 16)
	for _, m := range claudeStaticModels() {
		engines = append(engines, Engine{Model: m})
	}

	proxied, ok := cachedProxyEngines(ctx)
	engines = append(engines, proxied...)
	return Catalog{Engines: engines, ProxyOK: ok}
}

// cachedProxyEngines returns the proxied half of the catalogue, refreshing it
// at most once per engineCatalogTTL. On a failed refresh the last successful
// result is served instead: a proxy that is briefly unreachable must not
// change how an already-configured agent is dispatched.
func cachedProxyEngines(ctx context.Context) ([]Engine, bool) {
	engineCacheMu.Lock()
	defer engineCacheMu.Unlock()

	if engineCache != nil && time.Since(engineCache.fetchedAt) < engineCatalogTTL {
		return engineCache.engines, true
	}

	engines, err := fetchProxyEngines(ctx)
	if err != nil {
		if engineCache != nil {
			slog.Warn("engine catalogue: proxy unreachable, serving last known catalogue",
				"url", liteLLMInternalURL(), "age", time.Since(engineCache.fetchedAt).String(), "err", err)
			return engineCache.engines, true
		}
		slog.Warn("engine catalogue: proxy unreachable, offering Anthropic models only",
			"url", liteLLMInternalURL(), "err", err)
		return nil, false
	}

	engineCache = &engineCacheEntry{engines: engines, fetchedAt: time.Now()}
	return engines, true
}

// fetchProxyEngines lists the proxy's catalogue names over its OpenAI-shaped
// /v1/models endpoint.
func fetchProxyEngines(ctx context.Context) ([]Engine, error) {
	url := strings.TrimSuffix(liteLLMInternalURL(), "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	// x-api-key rather than Authorization: both are accepted by the proxy,
	// and this is the header Claude Code itself sends, so a key that works
	// for an agent works here.
	if key := os.Getenv("LITELLM_MASTER_KEY"); key != "" {
		req.Header.Set("x-api-key", key)
	}

	resp, err := engineHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("proxy /v1/models returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode proxy /v1/models: %w", err)
	}

	public := liteLLMPublicURL()
	engines := make([]Engine, 0, len(payload.Data))
	for _, m := range payload.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		engines = append(engines, Engine{
			// Label is the catalogue name itself. It IS the user-facing
			// identity — effort is pinned per entry in the proxy config, so
			// "dcc-glm-high" names an engine rather than just a model, and a
			// prettier label would hide the half that matters.
			Model:   Model{ID: id, Label: id, Provider: engineProviderLiteLLM},
			BaseURL: public,
		})
	}
	return engines, nil
}

// ResetEngineCatalogCacheForTest drops the memoised proxy catalogue. Tests
// only; production has no reason to invalidate ahead of the TTL.
func ResetEngineCatalogCacheForTest() {
	engineCacheMu.Lock()
	defer engineCacheMu.Unlock()
	engineCache = nil
}
