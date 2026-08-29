package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/trs-80/leanproxy-mcp-bob/pkg/bouncer"
	"github.com/trs-80/leanproxy-mcp-bob/pkg/mcp"
)

// fakeMCPHandler records what the handler saw and returns a canned response.
type fakeMCPHandler struct {
	seenParams []json.RawMessage
	respond    func(req *mcp.Request) *mcp.Response
}

func (f *fakeMCPHandler) HandleRequest(_ context.Context, req *mcp.Request) (*mcp.Response, error) {
	f.seenParams = append(f.seenParams, append(json.RawMessage(nil), req.Params...))
	if f.respond != nil {
		return f.respond(req), nil
	}
	return &mcp.Response{JSONRPC: mcp.JSONRPCVersion, Result: json.RawMessage(`{}`), ID: req.ID}, nil
}

func withStdioRedactor(t *testing.T) {
	t.Helper()
	prev := globalRedactor.Load()
	t.Cleanup(func() { globalRedactor.Store(prev) })
	initRedactor(nil)
}

// The `server run --stdio` path is the documented entrypoint and must
// redact in both directions exactly like `serve`.
func TestRunStdioLoop_RedactsParamsBeforeHandler(t *testing.T) {
	withStdioRedactor(t)
	h := &fakeMCPHandler{}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"srv__tool","arguments":{"token":"` + testAWSKey + `"}}}` + "\n")
	var out bytes.Buffer

	if err := runStdioLoop(context.Background(), in, &out, h, nil); err != nil {
		t.Fatal(err)
	}
	if len(h.seenParams) != 1 {
		t.Fatalf("handler saw %d requests, want 1", len(h.seenParams))
	}
	if bytes.Contains(h.seenParams[0], []byte(testAWSKey)) {
		t.Fatalf("handler received unredacted secret: %s", h.seenParams[0])
	}
	if !bytes.Contains(h.seenParams[0], []byte(bouncer.SecretRedacted)) {
		t.Fatalf("expected redaction marker in params: %s", h.seenParams[0])
	}
}

func TestRunStdioLoop_RedactsResultAndError(t *testing.T) {
	withStdioRedactor(t)
	h := &fakeMCPHandler{respond: func(req *mcp.Request) *mcp.Response {
		if req.ID == float64(1) {
			return &mcp.Response{JSONRPC: mcp.JSONRPCVersion, Result: json.RawMessage(`{"content":[{"type":"text","text":"GITHUB_TOKEN=` + testGHPat + `"}]}`), ID: req.ID}
		}
		return &mcp.Response{JSONRPC: mcp.JSONRPCVersion, Error: mcp.NewError(mcp.ErrCodeServerError, "upstream said "+testAWSKey), ID: req.ID}
	}}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"x"}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"y"}}` + "\n")
	var out bytes.Buffer

	if err := runStdioLoop(context.Background(), in, &out, h, nil); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if strings.Contains(got, testGHPat) || strings.Contains(got, testAWSKey) {
		t.Fatalf("secret reached client: %s", got)
	}
	if strings.Count(got, bouncer.SecretRedacted) != 2 {
		t.Fatalf("expected both result and error redacted: %s", got)
	}
}

func TestRunStdioLoop_MalformedParamsStillRedacted(t *testing.T) {
	withStdioRedactor(t)
	h := &fakeMCPHandler{}
	// Valid envelope, but params is a string containing a secret — exercises
	// the non-object path through RedactJSON.
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":"key ` + testAWSKey + `"}` + "\n")
	var out bytes.Buffer
	if err := runStdioLoop(context.Background(), in, &out, h, nil); err != nil {
		t.Fatal(err)
	}
	if len(h.seenParams) != 1 || bytes.Contains(h.seenParams[0], []byte(testAWSKey)) {
		t.Fatalf("secret leaked in string params: %v", h.seenParams)
	}
}

func TestRunStdioLoop_RedactorDisabledPassesThrough(t *testing.T) {
	prev := globalRedactor.Load()
	t.Cleanup(func() { globalRedactor.Store(prev) })
	globalRedactor.Store(nil)
	h := &fakeMCPHandler{}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping","params":{"k":"v"}}` + "\n")
	var out bytes.Buffer
	if err := runStdioLoop(context.Background(), in, &out, h, nil); err != nil {
		t.Fatal(err)
	}
	if string(h.seenParams[0]) != `{"k":"v"}` {
		t.Fatalf("params altered with redactor disabled: %s", h.seenParams[0])
	}
}
