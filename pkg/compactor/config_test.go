package compactor

import (
	"strings"
	"testing"
)

// A compactor configured for a hosted model must fail to load, not quietly
// send every tool name, description and parameter schema to a third party.
func TestConfigRejectsHostedLLMEndpoint(t *testing.T) {
	cfg := &Config{
		Enabled:     true,
		LLMEndpoint: "https://api.openai.com/v1/chat/completions",
		LLMModel:    "gpt-4o-mini",
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted a hosted LLM endpoint")
	}
	if !strings.Contains(err.Error(), "non-loopback") {
		t.Errorf("wrong rejection reason: %v", err)
	}
}

// An unset endpoint used to resolve to OpenAI. It must now be an error.
func TestConfigRejectsUnsetEndpointWhenEnabled(t *testing.T) {
	cfg := &Config{Enabled: true, LLMModel: "local"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted an enabled compactor with no endpoint")
	}
}

func TestConfigAllowsLocalLLMEndpoint(t *testing.T) {
	cfg := &Config{
		Enabled:     true,
		LLMEndpoint: "http://127.0.0.1:11434/v1/chat/completions",
		LLMModel:    "qwen2.5-coder",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected a local endpoint: %v", err)
	}
}

// A disabled compactor with a leftover hosted endpoint still loads — it just
// cannot run — so an old config file is not a hard startup failure.
func TestConfigIgnoresEndpointWhenDisabled(t *testing.T) {
	cfg := &Config{Enabled: false, LLMEndpoint: "https://api.openai.com/v1/chat/completions"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected a disabled compactor: %v", err)
	}
}
