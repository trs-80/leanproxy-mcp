package bouncer

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"time"
)

const SecretRedacted = "[SECRET_REDACTED]"

type RedactionMeta struct {
	MessageID string
	Method    string
}

type Redactor struct {
	patterns     []*regexp.Regexp
	alertManager *AlertManager
	bufferSize   int
}

func NewRedactor(patterns []*regexp.Regexp) *Redactor {
	return &Redactor{
		patterns:   patterns,
		bufferSize: 4096,
	}
}

func NewRedactorWithAlerts(patterns []*regexp.Regexp, alertManager *AlertManager) *Redactor {
	return &Redactor{
		patterns:     patterns,
		alertManager: alertManager,
		bufferSize:   4096,
	}
}

func (r *Redactor) RedactStream(reader io.Reader, writer io.Writer, meta ...*RedactionMeta) error {
	readerBuf := bufio.NewReaderSize(reader, r.bufferSize)
	writerBuf := bufio.NewWriterSize(writer, r.bufferSize)
	defer writerBuf.Flush()

	var totalRead, totalWritten int64
	matchCount := 0

	const maxOverlap = 128
	var overlap []byte

	for {
		buf := GetBuffer()

		n, err := readerBuf.Read(buf)
		if n > 0 {
			chunk := append(overlap, buf[:n]...)

			var toRedact []byte
			if err == io.EOF || len(chunk) <= maxOverlap {
				toRedact = chunk
				overlap = nil
			} else {
				splitIdx := len(chunk) - maxOverlap
				toRedact = chunk[:splitIdx]
				overlap = make([]byte, maxOverlap)
				copy(overlap, chunk[splitIdx:])
			}

			redacted, count := r.redactChunkWithCount(toRedact)

			_, writeErr := writerBuf.Write(redacted)
			if writeErr != nil {
				ReturnBuffer(buf)
				return fmt.Errorf("bouncer redact: %w", writeErr)
			}
			totalRead += int64(n)
			totalWritten += int64(len(redacted))
			matchCount += count
			slog.Debug("processing chunk", "size", len(toRedact))
		}

		ReturnBuffer(buf)

		if err == io.EOF {
			if len(overlap) > 0 {
				redacted, count := r.redactChunkWithCount(overlap)
				_, writeErr := writerBuf.Write(redacted)
				if writeErr != nil {
					return fmt.Errorf("bouncer redact: %w", writeErr)
				}
				totalWritten += int64(len(redacted))
				matchCount += count
				overlap = nil
			}
			slog.Info("streaming redaction complete", "bytes_read", totalRead, "bytes_written", totalWritten)
			break
		}
		if err != nil {
			return fmt.Errorf("bouncer redact: %w", err)
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

func (r *Redactor) redactChunkWithCount(chunk []byte) ([]byte, int) {
	result := make([]byte, 0, len(chunk))
	remaining := chunk
	matchCount := 0

	for len(remaining) > 0 {
		matchIndex := -1
		matchEnd := -1

		for _, pattern := range r.patterns {
			loc := pattern.FindIndex(remaining)
			if loc != nil && (matchIndex == -1 || loc[0] < matchIndex) {
				matchIndex = loc[0]
				matchEnd = loc[1]
			}
		}

		if matchIndex == -1 {
			result = append(result, remaining...)
			break
		}

		result = append(result, remaining[:matchIndex]...)
		result = append(result, SecretRedacted...)
		remaining = remaining[matchEnd:]
		matchCount++
	}

	return result, matchCount
}

func (r *Redactor) redactChunk(chunk []byte) []byte {
	result := make([]byte, 0, len(chunk))
	remaining := chunk

	for len(remaining) > 0 {
		matchIndex := -1
		matchEnd := -1

		for _, pattern := range r.patterns {
			loc := pattern.FindIndex(remaining)
			if loc != nil && (matchIndex == -1 || loc[0] < matchIndex) {
				matchIndex = loc[0]
				matchEnd = loc[1]
			}
		}

		if matchIndex == -1 {
			result = append(result, remaining...)
			break
		}

		result = append(result, remaining[:matchIndex]...)
		result = append(result, SecretRedacted...)
		remaining = remaining[matchEnd:]
	}

	return result
}

func (r *Redactor) RedactJSON(data []byte) ([]byte, int, error) {
	slog.Debug("redacting message", "size", len(data))

	var raw interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		slog.Warn("invalid JSON input, passing through unchanged")
		return data, 0, nil
	}

	redactedRaw, count := r.redactInterface(raw)
	redacted, err := json.Marshal(redactedRaw)
	if err != nil {
		return data, 0, err
	}

	if count > 0 {
		slog.Info("redaction complete", "secrets_found", count)
	}

	return redacted, count, nil
}

// sensitiveKeyPattern matches JSON object keys whose string values are
// redacted wholesale, regardless of whether the value looks like a known
// secret format (SLR-09 key-aware redaction).
var sensitiveKeyPattern = regexp.MustCompile(`(?i)(password|passwd|secret|token|api[_-]?key|authorization|private[_-]?key|credential|client_secret)`)

func (r *Redactor) redactInterface(val interface{}) (interface{}, int) {
	switch v := val.(type) {
	case string:
		return r.redactString(v)
	case map[string]interface{}:
		totalCount := 0
		out := make(map[string]interface{}, len(v))
		for k, val := range v {
			// Keys can carry secrets too (NEW-A-4).
			newKey, keyCount := r.redactString(k)
			totalCount += keyCount

			var newVal interface{}
			var count int
			if s, ok := val.(string); ok && s != "" && sensitiveKeyPattern.MatchString(k) {
				newVal, count = SecretRedacted, 1
			} else {
				newVal, count = r.redactInterface(val)
			}
			out[newKey] = newVal
			totalCount += count
		}
		return out, totalCount
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
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0x200B && r <= 0x200D, // ZWSP, ZWNJ, ZWJ
			r == 0x2060,                // word joiner
			r == 0xFEFF,                // BOM / ZWNBSP
			r >= 0x202A && r <= 0x202E, // bidi embedding/override
			r >= 0x2066 && r <= 0x2069: // bidi isolates
			return -1
		}
		return r
	}, s)
}

func (r *Redactor) redactString(data string) (string, int) {
	// Match against the string with invisible characters removed; if nothing
	// matches, return the original so benign content (e.g. ZWJ emoji
	// sequences) is passed through byte-for-byte.
	result := stripInvisible(data)
	totalCount := 0
	for _, pattern := range r.patterns {
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
	return &Redactor{
		patterns:   loaded.All,
		bufferSize: 4096,
	}
}

type SidecarClient interface {
	Redact(ctx context.Context, content string) string
	FallbackCount() int64
	Provider() string
	Model() string
	Healthy(ctx context.Context) bool
}

func RedactJSONWithSidecar(ctx context.Context, data []byte, r *Redactor, sidecar SidecarClient) ([]byte, error) {
	if r != nil {
		redacted, count, err := r.RedactJSON(data)
		if err != nil {
			return nil, err
		}
		if count > 0 {
			return redacted, nil
		}
	}
	if sidecar != nil {
		sidecarResult := sidecar.Redact(ctx, string(data))
		return []byte(sidecarResult), nil
	}
	return data, nil
}
