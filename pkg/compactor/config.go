package compactor

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/mmornati/leanproxy-mcp/internal/cachefile"
	"github.com/mmornati/leanproxy-mcp/internal/netguard"
	"github.com/mmornati/leanproxy-mcp/pkg/utils"
)

type Config struct {
	Enabled     bool   `yaml:"enabled"`
	LLMProvider string `yaml:"llm_provider"`
	LLMEndpoint string `yaml:"llm_endpoint"`
	LLMAPIKey   string `yaml:"llm_api_key"`
	LLMModel    string `yaml:"llm_model"`
	CacheDir    string `yaml:"cache_dir"`
}

func LoadConfig(path string) (*Config, error) {
	baseDir := filepath.Dir(filepath.Clean(path))
	if err := utils.ValidatePath(path, baseDir); err != nil {
		return nil, fmt.Errorf("compactor: path validation: %w", err)
	}

	data, err := os.ReadFile(path) // #nosec G304 -- path validated via ValidatePath above
	if err != nil {
		return nil, fmt.Errorf("compactor: read config: %w", err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("compactor: parse config: %w", err)
	}

	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Validate rejects a configuration that would send manifests off this machine.
//
// Only checked when the compactor is enabled, so a disabled stanza with a
// leftover hosted endpoint still loads — it just cannot run. Distillation reads
// every tool name, description and parameter schema and hands them to a model,
// so the endpoint has to be local. A model served from 127.0.0.1 (Ollama,
// llama.cpp, LM Studio) satisfies this; a hosted API does not.
func (c *Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if err := netguard.CheckInferenceEndpoint(c.LLMEndpoint); err != nil {
		return fmt.Errorf("compactor: llm_endpoint: %w", err)
	}
	if c.LLMModel == "" {
		return fmt.Errorf("compactor: llm_model must be set explicitly")
	}
	return nil
}

// applyDefaults fills in only what is safe to assume.
//
// Provider, model and endpoint are deliberately NOT defaulted. They used to
// resolve to openai / gpt-4o-mini / api.openai.com, so enabling distillation
// without naming a provider shipped the whole tool manifest — every tool name,
// description and parameter schema — to a third party. Leaving them empty makes
// an unconfigured compactor fail closed at Validate instead.
func (c *Config) applyDefaults() {
	if c.CacheDir == "" {
		// cachefile.HomeDir, not os.UserHomeDir: the distilled cache is
		// LeanProxy's own state and must follow $LEANPROXY_HOME with the rest
		// of it.
		usr, err := cachefile.HomeDir()
		if err == nil {
			c.CacheDir = filepath.Join(usr, ".config", "leanproxy", "distilled")
		}
	}
	if c.Enabled {
		c.Enabled = true
	}
}

func (c *Config) GetAPIKey() string {
	if c.LLMAPIKey != "" {
		return c.LLMAPIKey
	}
	return os.Getenv("LEANPROXY_LLM_API_KEY")
}
