package embedder

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type Provider string

// Only local providers exist. A hosted embedding provider would send tool
// arguments — file paths, symbol names, search strings — to a third party to
// compute a cache key, which no deployment under a data policy can allow. See
// internal/netguard.
const (
	ProviderOllama Provider = "ollama"
)

type EmbedRequest struct {
	ToolName string
	Args     json.RawMessage
}

func (r EmbedRequest) Input() string {
	var b strings.Builder
	b.WriteString(r.ToolName)
	b.WriteByte(':')
	if len(r.Args) > 0 {
		keys := make([]string, 0, 8)
		var parsed map[string]interface{}
		if err := json.Unmarshal(r.Args, &parsed); err == nil {
			for k := range parsed {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				b.WriteByte(' ')
				b.WriteString(k)
				b.WriteByte('=')
				val, _ := json.Marshal(parsed[k])
				b.Write(val)
			}
		} else {
			b.Write(r.Args)
		}
	}
	return b.String()
}

type Embedding struct {
	Vector []float32
	Model  string
}

type Embedder interface {
	Embed(ctx context.Context, req EmbedRequest) (Embedding, error)
	Provider() Provider
	Close() error
}

type Config struct {
	Provider Provider      `yaml:"provider"`
	Ollama   *OllamaConfig `yaml:"ollama,omitempty"`
}

func (c Config) Validate() error {
	switch c.Provider {
	case ProviderOllama:
		if c.Ollama == nil {
			return fmt.Errorf("embedder: ollama config required when provider=%q", c.Provider)
		}
		c.Ollama.withDefaults()
		if err := validateOllamaURL(c.Ollama.URL); err != nil {
			return fmt.Errorf("embedder ollama: %w", err)
		}
		if strings.TrimSpace(c.Ollama.Model) == "" {
			return fmt.Errorf("embedder ollama: model must not be empty")
		}
		return nil
	default:
		return fmt.Errorf("embedder: unknown provider %q", c.Provider)
	}
}

var (
	ErrEmbedderUnavailable = errors.New("embedder unavailable")
	ErrPayloadTooLarge     = errors.New("embedder payload too large")
)

const MaxPayloadBytes = 64 * 1024
