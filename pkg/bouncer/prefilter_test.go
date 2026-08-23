package bouncer

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// The union prefilter must never reject input that any individual pattern
// would match; otherwise a secret could be skipped before the real scan.
func TestPrefilterIsSupersetOfPatterns(t *testing.T) {
	r := NewRedactor(PatternsToRegexps(BuiltInPatterns))
	if r.prefilter == nil {
		t.Fatal("prefilter not built for built-in patterns")
	}
	for _, tc := range builtInCoverageCases {
		// Find the pattern index so the per-pattern trigger check is exercised.
		idx := -1
		p := GetPatternByName(tc.pattern)
		if p == nil {
			t.Fatalf("pattern %q not found", tc.pattern)
		}
		for i := range BuiltInPatterns {
			if BuiltInPatterns[i].Name == tc.pattern {
				idx = i
			}
		}
		for _, in := range tc.match {
			lowered := strings.ToLower(in)
			if !r.prefilter.possibleString(idx, lowered) {
				t.Errorf("prefilter rejected %q, which pattern %s matches", in, tc.pattern)
			}
			if !r.prefilter.anyPossible([]byte(lowered)) {
				t.Errorf("anyPossible rejected %q (pattern %s)", in, tc.pattern)
			}
		}
	}
}

// A pattern with no extractable literal must always be scanned.
func TestPrefilterNoLiteralAlwaysScans(t *testing.T) {
	pf := buildPrefilter([]*regexp.Regexp{regexp.MustCompile(`[0-9a-f]{32}`)})
	if !pf.possibleString(0, "zzz") {
		t.Error("pattern without literals must never be prefiltered out")
	}
}

// A clean payload must come back unchanged, with zero secrets reported, and
// the result must equal what the full walk produces.
func TestRedactJSONCleanPayloadFastPath(t *testing.T) {
	r := NewRedactor(PatternsToRegexps(BuiltInPatterns))
	clean := []byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"/tmp/x.txt","lines":[1,2,3],"note":"hello world"}}}`)
	out, n, err := r.RedactJSON(clean)
	if err != nil || n != 0 {
		t.Fatalf("RedactJSON(clean) = n=%d err=%v", n, err)
	}
	if !bytes.Equal(out, clean) {
		t.Errorf("clean payload altered:\n got %s\nwant %s", out, clean)
	}
}

// Escaped or invisible-char-split secrets must NOT take the fast path.
func TestRedactJSONFastPathDoesNotSkipEscapedSecrets(t *testing.T) {
	r := NewRedactor(PatternsToRegexps(BuiltInPatterns))
	cases := []string{
		`{"k":"\u0041KIAIOSFODNN7EXAMPLE"}`,       // JSON unicode escape inside the secret
		`{"k":"AKIA` + "​" + `IOSFODNN7EXAMPLE"}`, // raw zero-width space splits the secret
		`{"password":"hunter2hunter2"}`,           // sensitive key, value not a known format
		`{"k":"AKIAIOSFODNN7EXAMPLE"}`,            // plain positive control
	}
	for _, in := range cases {
		out, n, err := r.RedactJSON([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if n == 0 || !strings.Contains(string(out), SecretRedacted) {
			t.Errorf("%s: not redacted (n=%d): %s", in, n, out)
		}
	}
}

func TestStripInvisibleNoAllocWhenClean(t *testing.T) {
	s := "plain ascii and émoji 👍"
	if got := stripInvisible(s); got != s {
		t.Fatalf("stripInvisible altered clean string: %q", got)
	}
	if n := testing.AllocsPerRun(100, func() { _ = stripInvisible(s) }); n != 0 {
		t.Errorf("stripInvisible allocated %v times on clean input", n)
	}
}

func BenchmarkRedactJSONCleanLarge(b *testing.B) {
	var args []map[string]interface{}
	for i := 0; i < 200; i++ {
		args = append(args, map[string]interface{}{"path": "/srv/app/file.go", "content": strings.Repeat("the quick brown fox ", 20), "n": i})
	}
	payload, _ := json.Marshal(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": args})
	setDiscardSlog(b)
	r := NewRedactor(PatternsToRegexps(BuiltInPatterns))
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = r.RedactJSON(payload)
	}
}

func BenchmarkRedactSecretsClean(b *testing.B) {
	setDiscardSlog(b)
	in := strings.Repeat("ordinary log line with no secrets in it at all\n", 50)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = RedactSecrets(in)
	}
}
