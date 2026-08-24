package bouncer

import (
	"bytes"
	"strings"
	"testing"
)

// (?i) in Go regexp matches via unicode.SimpleFold orbits, so U+017F 'ſ'
// matches 's' — but plain ToLower leaves 'ſ' alone. The prefilter must use
// the same equivalence as the regexes or a fold-only spelling of a trigger
// word slips secrets past the whole redaction layer.
func TestPrefilter_UnicodeFoldBypass(t *testing.T) {
	payload := []byte(`{"note": "pa` + "ſſ" + `word=hunter2hunter2"}`)

	r := NewRedactor(PatternsToRegexps(BuiltInPatterns))
	out, count, err := r.RedactJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 || bytes.Contains(out, []byte("hunter2")) {
		t.Errorf("fold-only spelling bypassed redaction: count=%d out=%s", count, out)
	}

	if got := RedactSecrets(`pa` + "ſſ" + `word=hunter2hunter2`); strings.Contains(got, "hunter2") {
		t.Errorf("RedactSecrets bypassed: %s", got)
	}
}

func TestFoldNorm_EquivalenceClasses(t *testing.T) {
	cases := map[string]string{
		"pa\u017F\u017Fword": "PASSWORD", // U+017F folds into the s orbit
		"Kelvin \u212A":      "KELVIN K", // U+212A Kelvin sign folds into k orbit
		"plain":              "PLAIN",
		"MIXED":              "MIXED",
	}
	for in, want := range cases {
		if got := foldNormString(in); got != want {
			t.Errorf("foldNormString(%q) = %q, want %q", in, got, want)
		}
	}
}
