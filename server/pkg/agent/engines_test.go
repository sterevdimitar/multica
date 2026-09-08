package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// withProxy points the engine catalogue at a stub proxy for the duration of
// one test and drops the memoised catalogue on both sides of it, so tests
// can't leak a cached catalogue into each other.
func withProxy(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Setenv("LITELLM_INTERNAL_URL", srv.URL)
	t.Setenv("LITELLM_MASTER_KEY", "test-master-key")
	ResetEngineCatalogCacheForTest()
	t.Cleanup(func() {
		srv.Close()
		ResetEngineCatalogCacheForTest()
	})
	return srv
}

func modelsHandler(ids ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[`))
		for i, id := range ids {
			if i > 0 {
				w.Write([]byte(","))
			}
			w.Write([]byte(`{"id":"` + id + `","object":"model"}`))
		}
		w.Write([]byte(`]}`))
	}
}

// TestLoadEngineCatalog_PairsProxyModelsWithPublicURL is the core contract:
// a model served by the proxy resolves to the PUBLIC proxy URL, and an
// Anthropic model resolves to the empty string. The agent runs on a GitHub
// Actions runner, so the internal (docker-network) URL would be unreachable
// from where it is actually used.
func TestLoadEngineCatalog_PairsProxyModelsWithPublicURL(t *testing.T) {
	withProxy(t, modelsHandler("dcc-glm-low", "dcc-glm-high", "dcc-glm-max"))
	t.Setenv("LITELLM_PUBLIC_URL", "https://example.test/llm")

	catalog := LoadEngineCatalog(context.Background())
	if !catalog.ProxyOK {
		t.Fatal("ProxyOK = false with a reachable proxy")
	}

	for _, id := range []string{"dcc-glm-low", "dcc-glm-high", "dcc-glm-max"} {
		engine, ok := catalog.Lookup(id)
		if !ok {
			t.Fatalf("proxy model %q missing from catalogue", id)
		}
		if engine.BaseURL != "https://example.test/llm" {
			t.Errorf("%s base URL = %q, want the public proxy URL", id, engine.BaseURL)
		}
	}

	anthropic, ok := catalog.Lookup("claude-sonnet-4-6")
	if !ok {
		t.Fatal("Anthropic model missing from catalogue")
	}
	// Empty, deliberately — not https://api.anthropic.com. Empty is what
	// Claude Code reaches by exporting nothing, and it is the value the
	// pipeline's declared config compares against.
	if anthropic.BaseURL != "" {
		t.Errorf("Anthropic base URL = %q, want empty", anthropic.BaseURL)
	}
}

// TestLoadEngineCatalog_PublicURLDefaultsToInternal keeps a single-host
// deployment on one variable instead of two.
func TestLoadEngineCatalog_PublicURLDefaultsToInternal(t *testing.T) {
	srv := withProxy(t, modelsHandler("dcc-glm-low"))
	t.Setenv("LITELLM_PUBLIC_URL", "")

	engine, ok := LoadEngineCatalog(context.Background()).Lookup("dcc-glm-low")
	if !ok {
		t.Fatal("proxy model missing from catalogue")
	}
	if engine.BaseURL != srv.URL {
		t.Errorf("base URL = %q, want the internal URL %q", engine.BaseURL, srv.URL)
	}
}

// TestLoadEngineCatalog_SendsMasterKey pins the credential header. The proxy
// is on the public internet and answers 401 without one; a silently
// unauthenticated request would present as "the proxy has no models".
func TestLoadEngineCatalog_SendsMasterKey(t *testing.T) {
	var got string
	withProxy(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("x-api-key")
		modelsHandler("dcc-glm-low")(w, r)
	})

	LoadEngineCatalog(context.Background())
	if got != "test-master-key" {
		t.Errorf("x-api-key = %q, want the configured master key", got)
	}
}

// TestLoadEngineCatalog_DegradesToAnthropicOnly is the anti-regression for
// the empty model picker. An unreachable proxy must cost the proxied entries
// and nothing else — never the whole list.
func TestLoadEngineCatalog_DegradesToAnthropicOnly(t *testing.T) {
	withProxy(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	catalog := LoadEngineCatalog(context.Background())
	if catalog.ProxyOK {
		t.Error("ProxyOK = true with a failing proxy")
	}
	if len(catalog.Models()) != len(claudeStaticModels()) {
		t.Fatalf("got %d models, want the %d static Anthropic entries",
			len(catalog.Models()), len(claudeStaticModels()))
	}
	if _, ok := catalog.Lookup("claude-sonnet-4-6"); !ok {
		t.Error("Anthropic models must survive a proxy outage")
	}
}

// TestLoadEngineCatalog_ServesLastKnownCatalogueOnOutage guards the dispatch
// path: a proxy that blips must not turn a proxied agent into an Anthropic
// one, which would fail as "the model does not exist".
func TestLoadEngineCatalog_ServesLastKnownCatalogueOnOutage(t *testing.T) {
	fail := false
	withProxy(t, func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		modelsHandler("dcc-glm-high")(w, r)
	})
	t.Setenv("LITELLM_PUBLIC_URL", "https://example.test/llm")

	if _, ok := LoadEngineCatalog(context.Background()).Lookup("dcc-glm-high"); !ok {
		t.Fatal("warm-up fetch did not populate the catalogue")
	}

	fail = true
	catalog := LoadEngineCatalog(context.Background())
	if !catalog.ProxyOK {
		t.Error("ProxyOK = false while a known-good catalogue is still being served")
	}
	engine, ok := catalog.Lookup("dcc-glm-high")
	if !ok {
		t.Fatal("proxy model dropped during an outage")
	}
	if engine.BaseURL != "https://example.test/llm" {
		t.Errorf("base URL = %q, want the public proxy URL", engine.BaseURL)
	}
}

// TestCatalogModelsCarryProviderAndDefault keeps the picker's grouping and
// its "default" badge working: Models() is what the UI renders.
func TestCatalogModelsCarryProviderAndDefault(t *testing.T) {
	withProxy(t, modelsHandler("dcc-glm-low"))

	var sawAnthropicDefault, sawProxyProvider bool
	for _, m := range LoadEngineCatalog(context.Background()).Models() {
		if m.Provider == "anthropic" && m.Default {
			sawAnthropicDefault = true
		}
		if m.ID == "dcc-glm-low" {
			if m.Provider != engineProviderLiteLLM {
				t.Errorf("proxy entry provider = %q, want %q", m.Provider, engineProviderLiteLLM)
			}
			if m.Label != "dcc-glm-low" {
				t.Errorf("proxy entry label = %q, want the catalogue name", m.Label)
			}
			sawProxyProvider = true
		}
	}
	if !sawAnthropicDefault {
		t.Error("no Anthropic entry carried Default; the picker's badge would vanish")
	}
	if !sawProxyProvider {
		t.Error("proxy entry missing from Models()")
	}
}
