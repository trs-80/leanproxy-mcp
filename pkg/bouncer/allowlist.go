package bouncer

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

type SecretPattern struct {
	Name        string
	Pattern     *regexp.Regexp
	Example     string
	Description string
}

// BuiltInPatterns is the default set of secret detectors. All patterns are RE2
// (Go regexp) with no nested quantifiers. Order matters for the JSON path:
// earlier patterns win when two patterns overlap on the same text.
var BuiltInPatterns = []SecretPattern{
	{
		Name:        "aws-access-key",
		Pattern:     regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA|ANVA)[0-9A-Z]{16}\b`),
		Example:     "AKIAIOSFODNN7EXAMPLE",
		Description: "AWS Access Key ID (20 characters; AKIA, ASIA and other IAM prefixes)",
	},
	{
		Name:        "aws-secret-access-key",
		Pattern:     regexp.MustCompile(`(?i)\baws[_-]?secret[_-]?access[_-]?key\s*[=:]\s*['"]?[A-Za-z0-9/+]{40}\b`),
		Example:     "aws_secret_access_key=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Description: "AWS Secret Access Key assignment (40-character base64 value)",
	},
	{
		Name:        "gcp-api-key",
		Pattern:     regexp.MustCompile(`\bAIza[0-9A-Za-z_\-]{35}\b`),
		Example:     "[Google API key - AIza followed by 35 chars]",
		Description: "Google Cloud API key (39 characters, starts with AIza)",
	},
	{
		Name:        "github-token",
		Pattern:     regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{36,255}\b`),
		Example:     "ghp_abcdefghijklmnopqrstuvwxyz1234567890abcd",
		Description: "GitHub token (ghp_ classic PAT, gho_ OAuth, ghu_ user-to-server, ghs_ server-to-server, ghr_ refresh)",
	},
	{
		Name:        "github-fine-grained-pat",
		Pattern:     regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{22,}\b`),
		Example:     "github_pat_11abcdefghIJ9xsQ_xxxxxxxxxxxxxxxxx",
		Description: "GitHub Fine-grained PAT (starts with github_pat_)",
	},
	{
		Name:        "gitlab-pat",
		Pattern:     regexp.MustCompile(`\bglpat-[A-Za-z0-9_\-]{20,}\b`),
		Example:     "glpat-abcdefghijklmnopqrstuvwxyz",
		Description: "GitLab Personal Access Token (starts with glpat-)",
	},
	{
		Name:        "slack-token",
		Pattern:     regexp.MustCompile(`\bxox[abpers]-[0-9A-Za-z\-]{10,}\b`),
		Example:     "[Slack token - xoxb-<digits>-<digits>-<24 chars>]",
		Description: "Slack token (xoxb bot, xoxp user, xoxa/xoxe/xoxr/xoxs variants)",
	},
	{
		Name:        "slack-webhook",
		Pattern:     regexp.MustCompile(`https://hooks\.slack\.com/services/T[0-9A-Z]{8,}/B[0-9A-Z]{8,}/[0-9A-Za-z]{24}`),
		Example:     "[Slack webhook - https://hooks.slack.com/services/T<id>/B<id>/<24 chars>]",
		Description: "Slack incoming webhook URL",
	},
	{
		Name:        "stripe-secret-key",
		Pattern:     regexp.MustCompile(`\b[sr]k_(?:live|test)_[A-Za-z0-9]{24,}\b`),
		Example:     "[Stripe Secret/Restricted Key - 24+ chars after sk_live_, sk_test_, rk_live_, rk_test_]",
		Description: "Stripe secret (sk_) or restricted (rk_) key, live or test mode",
	},
	{
		Name:        "stripe-publishable-key",
		Pattern:     regexp.MustCompile(`\bpk_(?:live|test)_[A-Za-z0-9]{24,}\b`),
		Example:     "[Stripe Publishable Key - 24+ chars after pk_live_ or pk_test_]",
		Description: "Stripe publishable key, live or test mode",
	},
	{
		Name:        "stripe-webhook-secret",
		Pattern:     regexp.MustCompile(`\bwhsec_[A-Za-z0-9]{32,}\b`),
		Example:     "[Stripe webhook secret - whsec_ followed by 32+ chars]",
		Description: "Stripe webhook signing secret (starts with whsec_)",
	},
	{
		Name:        "openai-anthropic-key",
		Pattern:     regexp.MustCompile(`\bsk-(?:proj-|ant-)?[A-Za-z0-9_\-]{20,}\b`),
		Example:     "sk-ant-api03-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		Description: "OpenAI (sk-, sk-proj-) or Anthropic (sk-ant-) API key",
	},
	{
		Name:        "generic-api-key",
		Pattern:     regexp.MustCompile(`(?i)\bapi[_-]?key\b\s*[=:]\s*['"]?[A-Za-z0-9_\-]{16,}`),
		Example:     "api_key=abcdefghijklmnopqrstuvwx",
		Description: "Generic API key assignment (api_key=..., apiKey: ...; case-insensitive)",
	},
	{
		Name:        "bearer-token",
		Pattern:     regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9\-_]+\.[A-Za-z0-9\-_]+\.[A-Za-z0-9\-_]+`),
		Example:     "Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
		Description: "JWT Bearer token (three base64url segments)",
	},
	{
		Name:        "jwt",
		Pattern:     regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{10,}\b`),
		Example:     "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
		Description: "Raw JSON Web Token (no Bearer prefix required)",
	},
	{
		Name:        "bearer-opaque",
		Pattern:     regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9_\-.=+/]{20,}`),
		Example:     "Bearer abcdefghijklmnopqrstuvwxyz012345",
		Description: "Opaque Bearer token (20+ token characters after Bearer)",
	},
	{
		Name:        "basic-auth",
		Pattern:     regexp.MustCompile(`(?i)\bbasic\s+[A-Za-z0-9+/=]{16,}`),
		Example:     "Basic dXNlcjpwYXNzd29yZDEyMw==",
		Description: "HTTP Basic authorization credential (base64 user:password)",
	},
	{
		Name:        "pem-private-key",
		Pattern:     regexp.MustCompile(`(?s)-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY-----.*?-----END (?:RSA |EC |DSA |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY-----`),
		Example:     "-----BEGIN RSA PRIVATE KEY-----\n...\n-----END RSA PRIVATE KEY-----",
		Description: "PEM private key block (BEGIN ... END, multi-line; lazy body bounded by the END marker)",
	},
	{
		Name:        "pem-private-key-unterminated",
		Pattern:     regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY-----[A-Za-z0-9+/=\s]+`),
		Example:     "-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA...",
		Description: "PEM private key header plus base64 body without an END marker (truncated or split)",
	},
	{
		Name:        "connection-string",
		Pattern:     regexp.MustCompile(`(?i)\b(?:postgres|postgresql|mysql|mongodb|mongodb\+srv|redis|rediss|amqp|amqps|mssql|jdbc:[a-z]+)://[^:\s/@]*:[^@\s]+@`),
		Example:     "postgres://user:password@host:5432/db",
		Description: "Database / broker connection string with embedded credentials (scheme://user:pass@ is redacted)",
	},
	{
		Name:        "kv-secret",
		Pattern:     regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|secret|api[_-]?key|access[_-]?token|auth[_-]?token|client[_-]?secret|private[_-]?key)\b\s*[=:]\s*['"]?[^\s'",;}\])]{8,}`),
		Example:     "password=hunter2hunter2",
		Description: "key=value / key: value secret assignment (password, secret, api_key, access_token, client_secret, ...)",
	},
	{
		Name:        "azure-storage-key",
		Pattern:     regexp.MustCompile(`(?i)AccountKey=[A-Za-z0-9+/=]{86,}`),
		Example:     "AccountKey=" + "[86+ base64 chars]",
		Description: "Azure Storage account key in a connection string",
	},
	{
		Name:        "env-assignment",
		Pattern:     regexp.MustCompile(`(?:\bexport\s+)?\b[A-Z][A-Z0-9_]{1,40}_(?:KEY|TOKEN|SECRET|PASSWORD|PASS)=['"]?[^\s'"]+`),
		Example:     "export API_KEY=abc123def456",
		Description: "Shell/env assignment of a *_KEY, *_TOKEN, *_SECRET, *_PASSWORD or *_PASS variable",
	},
	{
		Name:        "env-var-value",
		Pattern:     regexp.MustCompile(`\$[A-Z_][A-Z0-9_]{0,30}=([^\s,}]+)`),
		Example:     "$API_KEY=secret123",
		Description: "Environment variable assignment",
	},
}

func ValidatePatterns() error {
	for _, p := range BuiltInPatterns {
		if p.Pattern == nil {
			return fmt.Errorf("allowlist: pattern %q has nil regexp", p.Name)
		}
		if p.Name == "" {
			return fmt.Errorf("allowlist: pattern has empty name")
		}
	}
	return nil
}

func GetPatternNames() []string {
	names := make([]string, len(BuiltInPatterns))
	for i, p := range BuiltInPatterns {
		names[i] = p.Name
	}
	return names
}

func GetPatternByName(name string) *SecretPattern {
	for i := range BuiltInPatterns {
		if BuiltInPatterns[i].Name == name {
			return &BuiltInPatterns[i]
		}
	}
	return nil
}

func GetBuiltInPatterns() []SecretPattern {
	return BuiltInPatterns
}

func CompileCustomPatterns(configs []PatternConfig) ([]*regexp.Regexp, error) {
	patterns := make([]*regexp.Regexp, 0, len(configs))
	for _, c := range configs {
		if c.Name == "" || c.Pattern == "" {
			continue
		}
		re, err := regexp.Compile(c.Pattern)
		if err != nil {
			return nil, fmt.Errorf("allowlist: invalid pattern %q: %w", c.Name, err)
		}
		patterns = append(patterns, re)
	}
	return patterns, nil
}

func CompileCustomPatternsWithTimeout(configs []PatternConfig, timeout time.Duration) ([]*regexp.Regexp, error) {
	patterns := make([]*regexp.Regexp, 0, len(configs))
	for _, c := range configs {
		if c.Name == "" || c.Pattern == "" {
			continue
		}
		if err := ValidateReDoS(c.Pattern, timeout); err != nil {
			slog.Warn("pattern skipped due to ReDoS risk", "name", c.Name, "error", err)
			continue
		}
		re, err := regexp.Compile(c.Pattern)
		if err != nil {
			return nil, fmt.Errorf("allowlist: invalid pattern %q: %w", c.Name, err)
		}
		patterns = append(patterns, re)
	}
	return patterns, nil
}

func ValidateReDoS(pattern string, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 100 * time.Millisecond
	}

	re, err := regexp.Compile(pattern)
	if err != nil {
		return err
	}

	toxic := make([]byte, 1000)
	for i := range toxic {
		toxic[i] = '!'
	}

	done := make(chan bool, 1)
	go func() {
		re.Match(toxic)
		done <- true
	}()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("pattern appears vulnerable to ReDoS (timeout)")
	}
}

type PatternConfig struct {
	Name    string `yaml:"name"`
	Pattern string `yaml:"pattern"`
}

func (pc PatternConfig) Validate() error {
	if pc.Name == "" {
		return fmt.Errorf("allowlist: pattern config has empty name")
	}
	if pc.Pattern == "" {
		return fmt.Errorf("allowlist: pattern config %q has empty pattern", pc.Name)
	}
	if len(pc.Pattern) > 500 {
		return fmt.Errorf("allowlist: pattern config %q exceeds maximum length of 500", pc.Name)
	}
	if _, err := regexp.Compile(pc.Pattern); err != nil {
		return fmt.Errorf("allowlist: pattern config %q has invalid regexp: %w", pc.Name, err)
	}
	return nil
}

func PatternsToRegexps(patterns []SecretPattern) []*regexp.Regexp {
	result := make([]*regexp.Regexp, 0, len(patterns))
	for i := range patterns {
		result = append(result, patterns[i].Pattern)
	}
	return result
}

func LoadPatternsWithLogging(customConfigs []PatternConfig) ([]*regexp.Regexp, []string) {
	patternCount := len(BuiltInPatterns)
	slog.Info("loading allow-list patterns", "count", patternCount)

	var skipped []string
	for i, p := range BuiltInPatterns {
		slog.Debug("pattern validated", "name", p.Name, "index", i)
	}

	if len(customConfigs) > 0 {
		for _, c := range customConfigs {
			if err := c.Validate(); err != nil {
				slog.Warn("invalid custom pattern skipped", "name", c.Name, "error", err.Error())
				skipped = append(skipped, c.Name)
			}
		}
	}

	allPatterns := PatternsToRegexps(BuiltInPatterns)
	return allPatterns, skipped
}

func MatchSecret(input string) []string {
	var matched []string
	for _, pattern := range BuiltInPatterns {
		if pattern.Pattern.MatchString(input) {
			matched = append(matched, pattern.Name)
		}
	}
	return matched
}

func RedactSecrets(input string) string {
	result := input
	for _, pattern := range BuiltInPatterns {
		result = pattern.Pattern.ReplaceAllString(result, SecretRedacted)
	}
	return result
}

func RedactWithPatterns(input string, patterns []*regexp.Regexp) string {
	result := input
	for _, pattern := range patterns {
		result = pattern.ReplaceAllString(result, SecretRedacted)
	}
	return result
}

func FormatPatternList() string {
	lines := make([]string, 0, len(BuiltInPatterns))
	for _, p := range BuiltInPatterns {
		lines = append(lines, fmt.Sprintf("- %s: %s (%s)", p.Name, p.Description, p.Example))
	}
	return strings.Join(lines, "\n")
}
