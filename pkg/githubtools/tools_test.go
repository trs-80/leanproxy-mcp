package githubtools

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/go-github/v62/github"
)

func TestNewGitHubClient_ReadOnly(t *testing.T) {
	os.Unsetenv("GITHUB_TOKEN")
	client := NewGitHubClient(slog.Default())
	if client == nil {
		t.Fatal("expected non-nil client")
	}
	if !client.readOnly {
		t.Error("expected read-only mode when GITHUB_TOKEN is not set")
	}
	if client.rateLimiter == nil {
		t.Error("expected rate limiter")
	}
}

func TestNewGitHubClient_Authenticated(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token-12345")
	client := NewGitHubClient(slog.Default())
	if client == nil {
		t.Fatal("expected non-nil client")
	}
	if client.readOnly {
		t.Error("expected authenticated mode when GITHUB_TOKEN is set")
	}
}

func TestGetTools_ReadOnly(t *testing.T) {
	os.Unsetenv("GITHUB_TOKEN")
	client := NewGitHubClient(slog.Default())
	tools := client.GetTools()

	foundListRepos := false
	foundGetIssue := false
	foundCreatePR := false

	for _, tool := range tools {
		switch tool.Name {
		case toolListRepos:
			foundListRepos = true
		case toolGetIssue:
			foundGetIssue = true
		case toolCreatePR:
			foundCreatePR = true
		}
	}

	if !foundListRepos {
		t.Error("expected list_repos tool in read-only mode")
	}
	if !foundGetIssue {
		t.Error("expected get_issue tool in read-only mode")
	}
	if foundCreatePR {
		t.Error("expected create_pr tool NOT to be available in read-only mode")
	}
}

func TestGetTools_Authenticated(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	client := NewGitHubClient(slog.Default())
	tools := client.GetTools()

	foundCreatePR := false
	for _, tool := range tools {
		if tool.Name == toolCreatePR {
			foundCreatePR = true
			break
		}
	}

	if !foundCreatePR {
		t.Error("expected create_pr tool in authenticated mode")
	}
}

func TestToolDefinitions_HaveSchemas(t *testing.T) {
	os.Unsetenv("GITHUB_TOKEN")
	client := NewGitHubClient(slog.Default())
	tools := client.GetTools()

	for _, tool := range tools {
		if tool.Description == "" {
			t.Errorf("tool %q has empty description", tool.Name)
		}
		if tool.InputSchema == nil || string(tool.InputSchema) == "" || string(tool.InputSchema) == "{}" {
			t.Errorf("tool %q has empty or missing input schema", tool.Name)
		}
		var schema map[string]interface{}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Errorf("tool %q has invalid input schema JSON: %v", tool.Name, err)
		}
	}
}

func TestIsReadOnly(t *testing.T) {
	os.Unsetenv("GITHUB_TOKEN")
	client := NewGitHubClient(slog.Default())
	if !client.IsReadOnly() {
		t.Error("expected IsReadOnly to return true")
	}

	t.Setenv("GITHUB_TOKEN", "token")
	client2 := NewGitHubClient(slog.Default())
	if client2.IsReadOnly() {
		t.Error("expected IsReadOnly to return false with token")
	}
}

func TestCallTool_UnknownTool(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	client := NewGitHubClient(slog.Default())
	_, err := client.CallTool(context.Background(), "nonexistent", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
}

func TestCallTool_CreatePR_ReadOnly(t *testing.T) {
	os.Unsetenv("GITHUB_TOKEN")
	client := NewGitHubClient(slog.Default())
	_, err := client.CallTool(context.Background(), toolCreatePR, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error when calling create_pr in read-only mode")
	}
}

func TestCallTool_ListRepos_InvalidArgs(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	client := NewGitHubClient(slog.Default())
	_, err := client.CallTool(context.Background(), toolListRepos, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for list_repos without owner")
	}
}

func TestCallTool_GetIssue_InvalidArgs(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "test-token")
	client := NewGitHubClient(slog.Default())
	_, err := client.CallTool(context.Background(), toolGetIssue, json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected error for get_issue without required args")
	}
}

func TestGetRateLimiter(t *testing.T) {
	os.Unsetenv("GITHUB_TOKEN")
	client := NewGitHubClient(slog.Default())
	rl := client.GetRateLimiter()
	if rl == nil {
		t.Fatal("expected non-nil rate limiter")
	}
	if rl.Allow() {
		t.Log("rate limiter allows requests")
	}
}

func TestGetReadOnlyNotice(t *testing.T) {
	notice := GetReadOnlyNotice()
	if notice == "" {
		t.Error("expected non-empty read-only notice")
	}
}

func TestRepoResult_JSON(t *testing.T) {
	r := RepoResult{
		Name:     "test-repo",
		FullName: "owner/test-repo",
		Stars:    42,
		Private:  false,
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal RepoResult: %v", err)
	}
	var result RepoResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal RepoResult: %v", err)
	}
	if result.Name != "test-repo" {
		t.Errorf("name = %q, want %q", result.Name, "test-repo")
	}
}

func TestIssueResult_JSON(t *testing.T) {
	r := IssueResult{
		Number: 1,
		Title:  "Test Issue",
		State:  "open",
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal IssueResult: %v", err)
	}
	var result IssueResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("unmarshal IssueResult: %v", err)
	}
	if result.Number != 1 {
		t.Errorf("number = %d, want 1", result.Number)
	}
}

// newTestGitHubClient points a go-github client at a local httptest server so no
// test in this package ever dials api.github.com. Tests must stay hermetic: an
// unauthenticated live call would consume GitHub's anonymous rate limit and can
// hang indefinitely behind a firewall (go-github uses http.DefaultClient, which
// has no timeout).
func newTestGitHubClient(t *testing.T, h http.HandlerFunc) *github.Client {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	gh := github.NewClient(nil)
	// go-github requires BaseURL to end in a trailing slash (github.go:510).
	// Assign it directly rather than via WithEnterpriseURLs, which would append
	// "api/v3/" for a non-"api." host and break the path assertions below.
	base, err := url.Parse(ts.URL + "/")
	if err != nil {
		t.Fatalf("parse test server URL %q: %v", ts.URL+"/", err)
	}
	gh.BaseURL = base
	return gh
}

func TestHandleCreatePR_RejectsMissingRequiredFields(t *testing.T) {
	tests := []struct {
		name string
		args PRArgs
	}{
		{"missing owner", PRArgs{Owner: "", Repo: "r", Title: "t", Head: "h", Base: "b"}},
		{"missing repo", PRArgs{Owner: "o", Repo: "", Title: "t", Head: "h", Base: "b"}},
		{"missing title", PRArgs{Owner: "o", Repo: "r", Title: "", Head: "h", Base: "b"}},
		{"missing head", PRArgs{Owner: "o", Repo: "r", Title: "t", Head: "", Base: "b"}},
		{"missing base", PRArgs{Owner: "o", Repo: "r", Title: "t", Head: "h", Base: ""}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.args)
			if err != nil {
				t.Fatalf("marshal PRArgs %+v: %v", tt.args, err)
			}
			// The server fails the test if reached: validation must reject
			// before any HTTP call is issued.
			gh := newTestGitHubClient(t, func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("unexpected HTTP request %s %s: validation should have rejected %+v first", r.Method, r.URL.Path, tt.args)
				w.WriteHeader(http.StatusInternalServerError)
			})

			res, err := handleCreatePR(context.Background(), gh, data)
			if err == nil {
				t.Fatalf("handleCreatePR(%+v) = %v, nil; want a validation error", tt.args, res)
			}
			if res != nil {
				t.Errorf("handleCreatePR(%+v) returned result %v alongside error %v; want nil result", tt.args, res, err)
			}
			if !strings.Contains(err.Error(), "are required") {
				t.Errorf("handleCreatePR(%+v) error = %q, want it to mention the required fields (%q)", tt.args, err, "are required")
			}
		})
	}
}

func TestHandleCreatePR_Success(t *testing.T) {
	gh := newTestGitHubClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("request method = %q, want %q", r.Method, http.MethodPost)
		}
		if r.URL.Path != "/repos/o/r/pulls" {
			t.Errorf("request path = %q, want %q", r.URL.Path, "/repos/o/r/pulls")
		}
		var sent map[string]any
		if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		for field, want := range map[string]string{"title": "t", "head": "h", "base": "b", "body": "desc"} {
			if sent[field] != want {
				t.Errorf("request body %q = %v, want %q", field, sent[field], want)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":7,"title":"t","state":"open","url":"https://api.example/repos/o/r/pulls/7","html_url":"https://example/o/r/pull/7"}`))
	})

	data, err := json.Marshal(PRArgs{Owner: "o", Repo: "r", Title: "t", Head: "h", Base: "b", Body: "desc"})
	if err != nil {
		t.Fatalf("marshal PRArgs: %v", err)
	}

	res, err := handleCreatePR(context.Background(), gh, data)
	if err != nil {
		t.Fatalf("handleCreatePR with all required fields returned error: %v", err)
	}
	pr, ok := res.(PRResult)
	if !ok {
		t.Fatalf("handleCreatePR returned %T (%v), want PRResult", res, res)
	}
	if pr.Number != 7 {
		t.Errorf("PRResult.Number = %d, want 7", pr.Number)
	}
	if pr.Title != "t" {
		t.Errorf("PRResult.Title = %q, want %q", pr.Title, "t")
	}
	if pr.State != "open" {
		t.Errorf("PRResult.State = %q, want %q", pr.State, "open")
	}
	if pr.URL != "https://api.example/repos/o/r/pulls/7" {
		t.Errorf("PRResult.URL = %q, want %q", pr.URL, "https://api.example/repos/o/r/pulls/7")
	}
	if pr.HTMLURL != "https://example/o/r/pull/7" {
		t.Errorf("PRResult.HTMLURL = %q, want %q", pr.HTMLURL, "https://example/o/r/pull/7")
	}
}

func TestHandleListRepos_NoOwner(t *testing.T) {
	_, err := handleListRepos(context.Background(), github.NewClient(nil), json.RawMessage(`{"owner":""}`))
	if err == nil {
		t.Error("expected error for empty owner")
	}
}

func TestHandleGetIssue_NoOwner(t *testing.T) {
	_, err := handleGetIssue(context.Background(), github.NewClient(nil), json.RawMessage(`{"owner":"","repo":"r","issue_number":1}`))
	if err == nil {
		t.Error("expected error for empty owner")
	}
}

func TestHandleGetIssue_NoRepo(t *testing.T) {
	_, err := handleGetIssue(context.Background(), github.NewClient(nil), json.RawMessage(`{"owner":"o","repo":"","issue_number":1}`))
	if err == nil {
		t.Error("expected error for empty repo")
	}
}

func TestHandleGetIssue_InvalidIssueNumber(t *testing.T) {
	_, err := handleGetIssue(context.Background(), github.NewClient(nil), json.RawMessage(`{"owner":"o","repo":"r","issue_number":0}`))
	if err == nil {
		t.Error("expected error for issue_number <= 0")
	}
}

func TestRateLimitTokenBucket(t *testing.T) {
	os.Unsetenv("GITHUB_TOKEN")
	client := NewGitHubClient(slog.Default())
	rl := client.GetRateLimiter()

	if rl.Remaining() < 4990 {
		t.Errorf("expected near-full bucket, got %d", rl.Remaining())
	}
}
