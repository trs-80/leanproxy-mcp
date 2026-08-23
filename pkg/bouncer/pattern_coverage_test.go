package bouncer

import (
	"encoding/json"
	"strings"
	"testing"
)

// patternCase is a positive example and a near-miss negative for one built-in pattern.
type patternCase struct {
	pattern  string
	match    []string
	nonMatch []string
}

var builtInCoverageCases = []patternCase{
	{
		pattern: "aws-access-key",
		match: []string{
			"AKIAIOSFODNN7EXAMPLE",
			"ASIA" + "IOSFODNN7EXAMPLE", // split: real-format temp key trips GitHub secret scanning
			"AROAIOSFODNN7EXAMPLE",
			`key="AGPAIOSFODNN7EXAMPLE"`,
		},
		nonMatch: []string{
			"AKIAIOSFODNN7EXAMPL",   // 19 chars
			"XAKIAIOSFODNN7EXAMPLE", // no word boundary
			"akiaiosfodnn7example",
		},
	},
	{
		pattern: "aws-secret-access-key",
		match: []string{
			"aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			`AWS_SECRET_ACCESS_KEY="wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"`,
		},
		nonMatch: []string{
			"aws_secret_access_key = tooshort",
			"aws_secret_access_key is documented here",
		},
	},
	{
		pattern:  "gcp-api-key",
		match:    []string{"AIza" + "SyA1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6r"}, // split for secret scanners
		nonMatch: []string{"AIzaSyA1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p", "AIza"},
	},
	{
		pattern: "github-token",
		match: []string{
			"ghp_" + strings.Repeat("a", 36),
			"gho_" + strings.Repeat("b", 36),
			"ghu_" + strings.Repeat("c", 36),
			"ghs_" + strings.Repeat("d", 36),
			"ghr_" + strings.Repeat("e", 80),
		},
		nonMatch: []string{
			"ghp_" + strings.Repeat("a", 35),
			"ghx_" + strings.Repeat("a", 36),
		},
	},
	{
		pattern:  "github-fine-grained-pat",
		match:    []string{"github_pat_11abcdefghIJ9xsQ_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"},
		nonMatch: []string{"github_pat_11"},
	},
	{
		pattern:  "gitlab-pat",
		match:    []string{"glpat-abcdefghijklmnopqrstuvwxyz"},
		nonMatch: []string{"glpat-short", "glpatabcdefghijklmnopqrstuvwxyz"},
	},
	{
		pattern: "slack-token",
		match: []string{
			"xox" + "b-123456789012-1234567890123-abcdefghijklmnopqrstuvwx",
			"xoxp-1234567890-ABCDEFGHIJ",
			"xoxe-1234567890-ABCDEFGHIJ",
		},
		nonMatch: []string{"xoxb-short", "xoxz-123456789012-1234567890123"},
	},
	{
		pattern:  "slack-webhook",
		match:    []string{"https://hooks.slack.com/" + "services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX"},
		nonMatch: []string{"https://hooks.slack.com/services/", "https://hooks.slack.com/services/T000/B000/short"},
	},
	{
		pattern: "stripe-secret-key",
		match: []string{
			"sk_live_" + strings.Repeat("x", 24),
			"sk_live_" + strings.Repeat("x", 99),
			"sk_test_" + strings.Repeat("x", 24),
			"rk_live_" + strings.Repeat("x", 24),
		},
		nonMatch: []string{"sk_live_" + strings.Repeat("x", 23), "sk_prod_" + strings.Repeat("x", 24)},
	},
	{
		pattern:  "stripe-publishable-key",
		match:    []string{"pk_live_" + strings.Repeat("x", 24), "pk_test_" + strings.Repeat("x", 40)},
		nonMatch: []string{"pk_live_" + strings.Repeat("x", 23)},
	},
	{
		pattern:  "stripe-webhook-secret",
		match:    []string{"whsec_" + strings.Repeat("a", 32)},
		nonMatch: []string{"whsec_" + strings.Repeat("a", 31)},
	},
	{
		pattern: "openai-anthropic-key",
		match: []string{
			"sk-" + strings.Repeat("a", 48),
			"sk-proj-" + strings.Repeat("b", 40),
			"sk-ant-api03-" + strings.Repeat("c", 40) + "-AA",
		},
		nonMatch: []string{"sk-short", "sk_" + strings.Repeat("a", 48)},
	},
	{
		pattern: "jwt",
		match: []string{
			"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c",
		},
		nonMatch: []string{"eyJhbGciOiJIUzI1NiJ9", "eyJhbGciOiJIUzI1NiJ9.short.x"},
	},
	{
		pattern:  "bearer-opaque",
		match:    []string{"Bearer " + strings.Repeat("Z", 32), "authorization: bearer abcdefghij0123456789abcd"},
		nonMatch: []string{"Bearer", "Bearer short", "bearer of bad news"},
	},
	{
		pattern:  "basic-auth",
		match:    []string{"Basic dXNlcjpwYXNzd29yZDEyMw==", "Authorization: Basic YWxhZGRpbjpvcGVuc2VzYW1l"},
		nonMatch: []string{"Basic authentication", "basic"},
	},
	{
		pattern: "pem-private-key",
		match: []string{
			"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\nabc+/=\n-----END RSA PRIVATE KEY-----",
			"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBg\n-----END PRIVATE KEY-----",
			"-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEA\n-----END OPENSSH PRIVATE KEY-----",
			"-----BEGIN EC PRIVATE KEY-----\r\nMHQCAQEEIA\r\n-----END EC PRIVATE KEY-----",
		},
		nonMatch: []string{
			"-----BEGIN PUBLIC KEY-----\nMIIBIjANBg\n-----END PUBLIC KEY-----",
			"-----BEGIN CERTIFICATE-----\nMIIC\n-----END CERTIFICATE-----",
		},
	},
	{
		pattern: "pem-private-key-unterminated",
		match: []string{
			"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA",
		},
		nonMatch: []string{"-----BEGIN PUBLIC KEY-----\nMIIBIjANBg"},
	},
	{
		pattern: "connection-string",
		match: []string{
			"postgres://admin:hunter2@db.example.com:5432/app",
			"postgresql://admin:hunter2@db/app",
			"mysql://root:toor@localhost/db",
			"mongodb+srv://" + "user:p%40ss@cluster0.mongodb.net/db", // split for secret scanners
			"redis://:mypassword@cache:6379/0",
			"amqp://guest:guest@rabbit:5672/",
		},
		nonMatch: []string{
			"postgres://db.example.com:5432/app",
			"https://user:pass@example.com",
		},
	},
	{
		pattern: "kv-secret",
		match: []string{
			"password=hunter2hunter2",
			"passwd: s3cretvalue",
			"client_secret: abcdef1234567890",
			"access_token=ya29.a0AfH6SMBx",
			"SECRET = 'topsecretvalue'",
		},
		nonMatch: []string{
			"password=short",
			"password123",
			"my_secret_key",
			"the secret is that there is no secret",
			`{"password": "hunter2hunter2"}`, // quoted JSON keys are handled by key-aware JSON redaction
		},
	},
	{
		pattern: "azure-storage-key",
		match: []string{
			"DefaultEndpointsProtocol=https;AccountName=x;AccountKey=" + strings.Repeat("A", 86) + "==;EndpointSuffix=core.windows.net",
		},
		nonMatch: []string{"AccountKey=short"},
	},
	{
		pattern: "env-assignment",
		match: []string{
			"export API_KEY=abc123def456",
			"export AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI",
			"DATABASE_PASSWORD=hunter2",
			"MY_TOKEN=abc",
		},
		nonMatch: []string{
			"API_KEY",
			"export PATH=/usr/bin",
			"api_key=abc123def456",
		},
	},
	{
		pattern: "generic-api-key",
		match: []string{
			"api_key=abcdefghijklmnopqrstuvwx",
			"apiKey: abcdefghijklmnopqrstuvwx",
			"API-KEY: abcdefghijklmnopqrstuvwx",
		},
		nonMatch: []string{
			"ApiKeyAuthenticationHandler",
			"apiKeyAuthenticationHandlerFactory",
			"api_key=short",
		},
	},
	{
		pattern:  "bearer-token",
		match:    []string{"Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c", "bearer abc.def.ghi"},
		nonMatch: []string{"Bearer abc.def", "Bearer123"},
	},
	{
		pattern:  "env-var-value",
		match:    []string{"$API_KEY=secret123", "$MY_VAR=value"},
		nonMatch: []string{"API_KEY=secret", "$api_key=secret"},
	},
}

func TestBuiltInPatternCoverage(t *testing.T) {
	for _, tc := range builtInCoverageCases {
		t.Run(tc.pattern, func(t *testing.T) {
			p := GetPatternByName(tc.pattern)
			if p == nil {
				t.Fatalf("built-in pattern %q not found", tc.pattern)
			}
			for _, in := range tc.match {
				if !p.Pattern.MatchString(in) {
					t.Errorf("%s should match %q", tc.pattern, in)
				}
			}
			for _, in := range tc.nonMatch {
				if p.Pattern.MatchString(in) {
					t.Errorf("%s should NOT match %q", tc.pattern, in)
				}
			}
		})
	}
}

// Every built-in pattern must have a coverage case above.
func TestBuiltInPatternCoverageIsExhaustive(t *testing.T) {
	covered := map[string]bool{}
	for _, tc := range builtInCoverageCases {
		covered[tc.pattern] = true
	}
	for _, p := range BuiltInPatterns {
		if !covered[p.Name] {
			t.Errorf("built-in pattern %q has no coverage case", p.Name)
		}
	}
}

// SLR-09: a long Stripe key must be redacted entirely, not just its first 24 chars.
func TestStripeKeyRedactedEntirely(t *testing.T) {
	key := "sk_live_" + strings.Repeat("x", 99)
	got := RedactSecrets("key: " + key)
	if strings.Contains(got, "x") {
		t.Errorf("tail of stripe key leaked: %q", got)
	}
}

// SLR-09: generic-api-key must not fire on identifiers that merely contain "ApiKey".
func TestGenericAPIKeyNoProseFalsePositive(t *testing.T) {
	in := "Registered ApiKeyAuthenticationHandler for the route"
	if got := RedactSecrets(in); got != in {
		t.Errorf("prose was redacted: %q", got)
	}
}

// SLR-09: key-aware JSON redaction — sensitive keys get their string value redacted regardless of shape.
func TestRedactJSONKeyAware(t *testing.T) {
	input := `{"password":"hunter2","config":{"client_secret":"abc","api-key":"x","Authorization":"Token opaque"},"name":"hello","count":1,"token_count":5,"empty_token":""}`
	redactor := NewRedactor(PatternsToRegexps(BuiltInPatterns))
	out, count, err := redactor.RedactJSON([]byte(input))
	if err != nil {
		t.Fatalf("RedactJSON: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if got["password"] != SecretRedacted {
		t.Errorf("password not redacted: %v", got["password"])
	}
	cfg := got["config"].(map[string]interface{})
	for _, k := range []string{"client_secret", "api-key", "Authorization"} {
		if cfg[k] != SecretRedacted {
			t.Errorf("%s not redacted: %v", k, cfg[k])
		}
	}
	if got["name"] != "hello" {
		t.Errorf("benign value changed: %v", got["name"])
	}
	if got["count"] != float64(1) || got["token_count"] != float64(5) {
		t.Errorf("non-string values changed: %v %v", got["count"], got["token_count"])
	}
	if got["empty_token"] != "" {
		t.Errorf("empty string should stay empty: %v", got["empty_token"])
	}
	if count != 4 {
		t.Errorf("expected 4 redactions, got %d", count)
	}
}

// NEW-A-4: JSON object keys are scanned too.
func TestRedactJSONScansKeys(t *testing.T) {
	input := `{"AKIAIOSFODNN7EXAMPLE":"v","nested":{"ghp_123456789012345678901234567890123456":1}}`
	redactor := NewRedactor(PatternsToRegexps(BuiltInPatterns))
	out, count, err := redactor.RedactJSON([]byte(input))
	if err != nil {
		t.Fatalf("RedactJSON: %v", err)
	}
	if strings.Contains(string(out), "AKIAIOSFODNN7EXAMPLE") || strings.Contains(string(out), "ghp_") {
		t.Errorf("secret key leaked: %s", out)
	}
	if count != 2 {
		t.Errorf("expected 2 redactions, got %d", count)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if got[SecretRedacted] != "v" {
		t.Errorf("value under redacted key lost: %v", got)
	}
}

// Zero-width / bidi control characters must not split a secret so that it evades matching.
func TestRedactStripsInvisibleCharacters(t *testing.T) {
	redactor := NewRedactor(PatternsToRegexps(BuiltInPatterns))
	for _, zw := range []string{"\u200b", "\u200c", "\u200d", "\u2060", "\ufeff", "\u202a", "\u202e", "\u2066", "\u2069"} {
		secret := "AKIA" + zw + "IOSFODNN7EXAMPLE"
		input := `{"k":"` + secret + `"}`
		out, count, err := redactor.RedactJSON([]byte(input))
		if err != nil {
			t.Fatalf("RedactJSON: %v", err)
		}
		if count != 1 || strings.Contains(string(out), "IOSFODNN7EXAMPLE") {
			t.Errorf("secret with %U leaked: %s (count=%d)", []rune(zw)[0], out, count)
		}
	}

	// Benign text containing a ZWJ emoji sequence must pass through untouched.
	benign := "{\"msg\":\"family \U0001F468\u200d\U0001F469\u200d\U0001F467\"}"
	out, count, err := redactor.RedactJSON([]byte(benign))
	if err != nil {
		t.Fatalf("RedactJSON: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output not JSON: %v", err)
	}
	if count != 0 || got["msg"] != "family \U0001F468\u200d\U0001F469\u200d\U0001F467" {
		t.Errorf("benign ZWJ text altered: %v (count=%d)", got["msg"], count)
	}
}

// SLR-09 probe: raw secrets inside JSON string values must be caught.
func TestRedactJSONProbeSecrets(t *testing.T) {
	// leak is the fragment of the secret that must not survive redaction.
	secrets := map[string]struct{ value, leak string }{
		"aws_temp":    {"ASIA" + "IOSFODNN7EXAMPLE", "7EXAMPLE"},
		"gh_server":   {"ghs_" + strings.Repeat("a", 36), "aaaaaaaa"},
		"slack":       {"xox" + "b-123456789012-1234567890123-abcdefghijklmnopqrstuvwx", "qrstuvwx"},
		"stripe_long": {"sk_live_" + strings.Repeat("x", 99), "xxxxxxxx"},
		"openai":      {"sk-proj-" + strings.Repeat("b", 40), "bbbbbbbb"},
		"raw_jwt":     {"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c", "adQssw5c"},
		"pem":         {"-----BEGIN RSA PRIVATE KEY-----\nMIIEowIBAAKCAQEA\n-----END RSA PRIVATE KEY-----", "MIIEowIBAAKCAQEA"},
		"dsn":         {"postgres://admin:hunter2@db.example.com:5432/app", "hunter2"},
		"gcp":         {"AIza" + "SyA1b2C3d4E5f6G7h8I9j0K1l2M3n4O5p6r", "n4O5p6r"},
		"export":      {"export API_KEY=abc123def456", "abc123def456"},
	}
	redactor := NewRedactor(PatternsToRegexps(BuiltInPatterns))
	for name, secret := range secrets {
		payload, _ := json.Marshal(map[string]string{"field": secret.value})
		out, count, err := redactor.RedactJSON(payload)
		if err != nil {
			t.Fatalf("%s: RedactJSON: %v", name, err)
		}
		if count == 0 || strings.Contains(string(out), secret.leak) {
			t.Errorf("%s leaked: %s", name, out)
		}
	}
}
