package bouncer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"
)

const SecretRedacted = "[SECRET_REDACTED]"

// defaultMaxOverlap is the number of trailing bytes held back between
// streaming scans so that a secret straddling a read boundary is still seen
// as a whole. It must be at least as long as the longest secret any built-in
// pattern can match; JWTs are routinely several hundred bytes.
const defaultMaxOverlap = 1024

// maxPendingMatch bounds how long a match touching the end of the buffer is
// held back waiting for more input before it is emitted as-is.
const maxPendingMatch = 64 * 1024

type RedactionMeta struct {
	MessageID string
	Method    string
}

type Redactor struct {
	patterns     []*regexp.Regexp
	prefilter    *prefilter // nil means "always scan every pattern"
	alertManager *AlertManager
	bufferSize   int
	maxOverlap   int
}

func NewRedactor(patterns []*regexp.Regexp) *Redactor {
	return &Redactor{
		patterns:   patterns,
		prefilter:  buildPrefilter(patterns),
		bufferSize: 4096,
		maxOverlap: defaultMaxOverlap,
	}
}

func NewRedactorWithAlerts(patterns []*regexp.Regexp, alertManager *AlertManager) *Redactor {
	return &Redactor{
		patterns:     patterns,
		prefilter:    buildPrefilter(patterns),
		alertManager: alertManager,
		bufferSize:   4096,
		maxOverlap:   defaultMaxOverlap,
	}
}

// span is a half-open [start, end) byte range of a pattern match.
type span struct{ start, end int }

// findSpans returns the merged, ordered set of byte ranges matched by any
// pattern. Overlapping or adjacent matches from different patterns are
// coalesced so each byte of input is redacted at most once.
func (r *Redactor) findSpans(data []byte) []span {
	lowered := foldNorm(data)
	var spans []span
	for i, pattern := range r.patterns {
		if !r.prefilter.possible(i, lowered) {
			continue
		}
		for _, loc := range pattern.FindAllIndex(data, -1) {
			if loc[1] > loc[0] {
				spans = append(spans, span{loc[0], loc[1]})
			}
		}
	}
	if len(spans) == 0 {
		return nil
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end > spans[j].end
	})
	merged := spans[:1]
	for _, s := range spans[1:] {
		last := &merged[len(merged)-1]
		if s.start < last.end {
			if s.end > last.end {
				last.end = s.end
			}
			continue
		}
		merged = append(merged, s)
	}
	return merged
}

// applySpans writes data[:limit] to out with every span replaced by the
// redaction marker. All spans must end at or before limit.
func applySpans(out []byte, data []byte, spans []span, limit int) []byte {
	pos := 0
	for _, s := range spans {
		out = append(out, data[pos:s.start]...)
		out = append(out, SecretRedacted...)
		pos = s.end
	}
	return append(out, data[pos:limit]...)
}

// RedactStream copies reader to writer, replacing secrets as they pass
// through. Input is accumulated before scanning and a tail of up to
// maxOverlap bytes after the last match is held back until more input (or
// EOF) arrives, so a secret split across arbitrary read boundaries — including
// very small reads from a pipe — is still redacted as a whole.
func (r *Redactor) RedactStream(reader io.Reader, writer io.Writer, meta ...*RedactionMeta) error {
	readerBuf := bufio.NewReaderSize(reader, r.bufferSize)
	writerBuf := bufio.NewWriterSize(writer, r.bufferSize)
	defer writerBuf.Flush()

	maxOverlap := r.maxOverlap
	if maxOverlap <= 0 {
		maxOverlap = defaultMaxOverlap
	}
	// Scan once we hold a full buffer beyond the hold-back window, so steady
	// state emits bufferSize bytes per scan rather than stalling.
	scanThreshold := r.bufferSize + maxOverlap

	var totalRead, totalWritten int64
	matchCount := 0
	carry := make([]byte, 0, scanThreshold+r.bufferSize)
	out := make([]byte, 0, scanThreshold)

	buf := GetBuffer()
	defer ReturnBuffer(buf)

	for {
		n, err := readerBuf.Read(buf)
		if n > 0 {
			carry = append(carry, buf[:n]...)
			totalRead += int64(n)
		}

		atEOF := err == io.EOF
		if err != nil && !atEOF {
			return fmt.Errorf("bouncer redact: %w", err)
		}

		if !atEOF && len(carry) < scanThreshold {
			continue
		}

		spans := r.findSpans(carry)
		hold := 0
		if !atEOF {
			// A match that runs right up to the end of what we have read may be
			// a truncated prefix of a longer secret (open-ended patterns such as
			// github_pat_…{22,} match partial tokens). Hold it back from its
			// start and rescan once more input arrives, unless it has grown
			// beyond any plausible secret length.
			if n := len(spans); n > 0 && spans[n-1].end == len(carry) && len(carry)-spans[n-1].start <= maxPendingMatch {
				hold = len(carry) - spans[n-1].start
				spans = spans[:n-1]
			}
			lastEnd := 0
			if len(spans) > 0 {
				lastEnd = spans[len(spans)-1].end
			}
			if h := min(maxOverlap, len(carry)-lastEnd); h > hold {
				hold = h
			}
		}
		emitEnd := len(carry) - hold

		out = applySpans(out[:0], carry, spans, emitEnd)
		if _, writeErr := writerBuf.Write(out); writeErr != nil {
			return fmt.Errorf("bouncer redact: %w", writeErr)
		}
		totalWritten += int64(len(out))
		matchCount += len(spans)
		slog.Debug("processing chunk", "size", emitEnd, "held", hold)

		carry = append(carry[:0], carry[emitEnd:]...)

		if atEOF {
			slog.Debug("streaming redaction complete", "bytes_read", totalRead, "bytes_written", totalWritten, "secrets_found", matchCount)
			break
		}
	}

	if r.alertManager != nil && matchCount > 0 && len(meta) > 0 && meta[0] != nil {
		r.alertManager.RecordRedaction(RedactionEvent{
			PatternName: "streaming_redaction",
			Count:       matchCount,
			Timestamp:   time.Now(),
			MessageID:   meta[0].MessageID,
			Method:      meta[0].Method,
		})
		r.alertManager.EmitSummary(meta[0].MessageID, meta[0].Method)
	}

	return nil
}

// redactChunkWithCount redacts a self-contained byte slice and reports how
// many distinct secret spans were replaced.
func (r *Redactor) redactChunkWithCount(chunk []byte) ([]byte, int) {
	spans := r.findSpans(chunk)
	if len(spans) == 0 {
		out := make([]byte, len(chunk))
		copy(out, chunk)
		return out, 0
	}
	return applySpans(make([]byte, 0, len(chunk)), chunk, spans, len(chunk)), len(spans)
}

func (r *Redactor) redactChunk(chunk []byte) []byte {
	out, _ := r.redactChunkWithCount(chunk)
	return out
}

// RedactJSON redacts every string value in a JSON document. If the input is
// not valid JSON it falls back to a byte-level scan of the raw input rather
// than passing it through unchanged, so a malformed or truncated payload can
// never be used to smuggle a secret past the redactor.
func (r *Redactor) RedactJSON(data []byte) ([]byte, int, error) {
	slog.Debug("redacting message", "size", len(data))

	if r.canSkipJSON(data) {
		return data, 0, nil
	}

	var raw interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		slog.Warn("invalid JSON input, falling back to byte-level redaction", "error", err)
		redacted, count := r.redactChunkWithCount(data)
		if count > 0 {
			slog.Debug("redaction complete", "secrets_found", count, "mode", "bytes")
		}
		return redacted, count, nil
	}

	redactedRaw, count := r.redactInterface(raw)
	redacted, err := json.Marshal(redactedRaw)
	if err != nil {
		return nil, 0, fmt.Errorf("bouncer redact: marshal: %w", err)
	}

	if count > 0 {
		slog.Debug("redaction complete", "secrets_found", count)
	}

	return redacted, count, nil
}

// canSkipJSON reports whether data provably contains nothing to redact, so
// the decode/walk/encode round trip can be skipped and the input returned
// verbatim. The prefilter runs over the raw bytes, which is only exact when
// every decoded string is byte-identical to a substring of the raw document:
// that rules out any backslash escape (\uXXXX, \/, ...) and any invisible
// character that stripInvisible would remove. Key-aware redaction is covered
// by running sensitiveKeyPattern over the raw bytes as well.
func (r *Redactor) canSkipJSON(data []byte) bool {
	if r.prefilter == nil || bytes.IndexByte(data, '\\') >= 0 {
		return false
	}
	if hasInvisible(string(data)) {
		return false
	}
	lowered := foldNorm(data)
	return !r.prefilter.anyPossible(lowered) && !sensitiveKeyTriggers.anyPossible(lowered)
}

// sensitiveKeyTriggers prefilters sensitiveKeyPattern the same way pattern
// scans are prefiltered.
var sensitiveKeyTriggers = &prefilter{pats: []triggerSets{extractTriggers(sensitiveKeyPattern.String())}}

// sensitiveKeyPattern matches JSON object keys whose string values are
// redacted wholesale, regardless of whether the value looks like a known
// secret format (SLR-09 key-aware redaction).
var sensitiveKeyPattern = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|authorization|private[_-]?key|credential|client_secret)`)

func (r *Redactor) redactInterface(val interface{}) (interface{}, int) {
	switch v := val.(type) {
	case string:
		return r.redactString(v)
	case map[string]interface{}:
		// Mutate in place; only keys that are themselves redacted need to be
		// re-inserted, and those are deferred until the range is complete.
		totalCount := 0
		var renamed map[string]interface{}
		for k, val := range v {
			var newVal interface{}
			var count int
			if s, ok := val.(string); ok && s != "" && sensitiveKeyPattern.MatchString(k) {
				newVal, count = SecretRedacted, 1
			} else {
				newVal, count = r.redactInterface(val)
			}
			totalCount += count

			// Keys can carry secrets too (NEW-A-4).
			newKey, keyCount := r.redactString(k)
			totalCount += keyCount
			if keyCount == 0 {
				v[k] = newVal
				continue
			}
			if renamed == nil {
				renamed = make(map[string]interface{})
			}
			renamed[newKey] = newVal
			delete(v, k)
		}
		for k, val := range renamed {
			v[k] = val
		}
		return v, totalCount
	case []interface{}:
		totalCount := 0
		for i, val := range v {
			newVal, count := r.redactInterface(val)
			v[i] = newVal
			totalCount += count
		}
		return v, totalCount
	default:
		return v, 0
	}
}

// stripInvisible removes zero-width and bidirectional control characters that
// can be inserted inside a secret to split it so that no pattern matches.
func stripInvisible(s string) string {
	if !hasInvisible(s) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isInvisible(r) {
			return -1
		}
		return r
	}, s)
}

func isInvisible(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200D, // ZWSP, ZWNJ, ZWJ
		r == 0x2060,                // word joiner
		r == 0xFEFF,                // BOM / ZWNBSP
		r >= 0x202A && r <= 0x202E, // bidi embedding/override
		r >= 0x2066 && r <= 0x2069: // bidi isolates
		return true
	}
	return false
}

// hasInvisible is a cheap pre-check: every rune stripInvisible removes is
// >= U+200B, so a pure-ASCII string (the common case) is rejected by a byte
// scan without decoding runes.
func hasInvisible(s string) bool {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return false
	}
	return strings.ContainsFunc(s, isInvisible)
}

func (r *Redactor) redactString(data string) (string, int) {
	// Match against the string with invisible characters removed; if nothing
	// matches, return the original so benign content (e.g. ZWJ emoji
	// sequences) is passed through byte-for-byte.
	result := stripInvisible(data)
	lowered := foldNormString(result)
	totalCount := 0
	for i, pattern := range r.patterns {
		if !r.prefilter.possibleString(i, lowered) {
			continue
		}
		matches := pattern.FindAllString(result, -1)
		if len(matches) > 0 {
			totalCount += len(matches)
			result = pattern.ReplaceAllString(result, SecretRedacted)
		}
	}
	if totalCount == 0 {
		return data, 0
	}
	return result, totalCount
}

func NewRedactorFromLoaded(loaded *LoadedPatterns) *Redactor {
	return NewRedactor(loaded.All)
}

type SidecarClient interface {
	Redact(ctx context.Context, content string) string
	FallbackCount() int64
	Provider() string
	Model() string
	Healthy(ctx context.Context) bool
}

// RedactJSONWithSidecar applies the regex redactor first and then, if a
// sidecar is configured, hands the already-redacted content to the sidecar
// for a second pass. The sidecar therefore never sees a secret the regex
// layer could catch, and its output is only accepted if it is valid JSON.
func RedactJSONWithSidecar(ctx context.Context, data []byte, r *Redactor, sidecar SidecarClient) ([]byte, error) {
	if r != nil {
		redacted, _, err := r.RedactJSON(data)
		if err != nil {
			return nil, err
		}
		data = redacted
	}
	if sidecar != nil {
		sidecarResult := []byte(sidecar.Redact(ctx, string(data)))
		if !json.Valid(sidecarResult) {
			return nil, fmt.Errorf("bouncer redact: sidecar returned invalid JSON")
		}
		return sidecarResult, nil
	}
	return data, nil
}

// Patterns returns the compiled patterns this redactor applies.
func (r *Redactor) Patterns() []*regexp.Regexp {
	return r.patterns
}
