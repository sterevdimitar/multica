package service

import (
	"testing"

	agentpkg "github.com/multica-ai/multica/server/pkg/agent"
)

// catalog builds a fixed catalogue so these tests exercise the pairing rule
// rather than proxy discovery (that has its own tests in pkg/agent).
//
// A degraded catalogue is modelled the way LoadEngineCatalog actually
// degrades: the Anthropic half survives and the proxied entries are simply
// absent. That absence is exactly what makes a degraded lookup miss look
// identical to a genuinely unknown model.
func catalog(proxyOK bool) agentpkg.Catalog {
	engines := []agentpkg.Engine{{Model: agentpkg.Model{ID: "claude-sonnet-4-6"}}}
	if proxyOK {
		engines = append(engines, agentpkg.Engine{
			Model:   agentpkg.Model{ID: "dcc-glm-high"},
			BaseURL: "https://example.test/llm",
		})
	}
	return agentpkg.Catalog{ProxyOK: proxyOK, Engines: engines}
}

// TestApplyEngineBaseURL_ProxiedModelGetsProxyURL is the pairing itself: the
// user picked only a model, and the base URL followed. Without this the
// dispatch reaches Anthropic with a GLM model name and fails as "the model
// does not exist".
func TestApplyEngineBaseURL_ProxiedModelGetsProxyURL(t *testing.T) {
	env := applyEngineBaseURL(map[string]string{"NEXT_AGENT": "fixer"}, "dcc-glm-high", "agent-1", catalog(true))

	if got := env[engineBaseURLKey]; got != "https://example.test/llm" {
		t.Errorf("%s = %q, want the proxy URL", engineBaseURLKey, got)
	}
	if env["NEXT_AGENT"] != "fixer" {
		t.Error("unrelated custom_env keys must survive: the env map is the only channel to the runner")
	}
}

// TestApplyEngineBaseURL_AnthropicModelClearsURL covers the switch back. An
// agent moved from GLM to Sonnet in the UI must stop being proxied, or it
// keeps hitting a proxy that has never heard of the Anthropic model name.
func TestApplyEngineBaseURL_AnthropicModelClearsURL(t *testing.T) {
	env := applyEngineBaseURL(
		map[string]string{engineBaseURLKey: "https://example.test/llm"},
		"claude-sonnet-4-6", "agent-1", catalog(true),
	)

	got, ok := env[engineBaseURLKey]
	if !ok {
		t.Fatalf("%s must be written explicitly, not omitted", engineBaseURLKey)
	}
	if got != "" {
		t.Errorf("%s = %q, want empty (Anthropic)", engineBaseURLKey, got)
	}
}

// TestApplyEngineBaseURL_UnknownModelIsAnthropic pins the custom-model case:
// the picker lets a user type a model that is not in the catalogue, and a
// model we do not proxy is by definition Anthropic's.
func TestApplyEngineBaseURL_UnknownModelIsAnthropic(t *testing.T) {
	env := applyEngineBaseURL(
		map[string]string{engineBaseURLKey: "https://example.test/llm"},
		"claude-some-future-model", "agent-1", catalog(true),
	)

	if got := env[engineBaseURLKey]; got != "" {
		t.Errorf("%s = %q, want empty for a model outside the catalogue", engineBaseURLKey, got)
	}
}

// TestApplyEngineBaseURL_DegradedCatalogueLeavesConfiguredURL is the
// important one. With the proxy unreachable, "not in the catalogue" and "the
// catalogue lost its proxied half" are the same observation — and clearing
// the URL on the second sends a proxied agent to Anthropic with a model name
// Anthropic does not have, which reads as a model bug and is a proxy outage.
func TestApplyEngineBaseURL_DegradedCatalogueLeavesConfiguredURL(t *testing.T) {
	env := applyEngineBaseURL(
		map[string]string{engineBaseURLKey: "https://example.test/llm"},
		"dcc-glm-high", "agent-1", catalog(false),
	)

	if got := env[engineBaseURLKey]; got != "https://example.test/llm" {
		t.Errorf("%s = %q, want the configured URL preserved while the catalogue is degraded", engineBaseURLKey, got)
	}
}

// TestApplyEngineBaseURL_NilEnv guards the common case: most agents carry no
// custom_env at all, and the map must be created rather than written into nil.
func TestApplyEngineBaseURL_NilEnv(t *testing.T) {
	env := applyEngineBaseURL(nil, "dcc-glm-high", "agent-1", catalog(true))
	if got := env[engineBaseURLKey]; got != "https://example.test/llm" {
		t.Errorf("%s = %q, want the proxy URL", engineBaseURLKey, got)
	}

	// A degraded catalogue with no env stays nil — there is nothing to
	// preserve and nothing to invent.
	if env := applyEngineBaseURL(nil, "dcc-glm-high", "agent-1", catalog(false)); env != nil {
		t.Errorf("degraded catalogue with no env produced %v, want nil", env)
	}
}
