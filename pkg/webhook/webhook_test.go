package webhook

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockAlert struct {
	team    string
	project string
	metric  string
	usage   int64
	limit   int64
	pct     float64
}

func (m mockAlert) TeamName() string         { return m.team }
func (m mockAlert) ProjectName() string      { return m.project }
func (m mockAlert) MetricName() string       { return m.metric }
func (m mockAlert) UsageAmount() int64       { return m.usage }
func (m mockAlert) LimitAmount() int64       { return m.limit }
func (m mockAlert) PercentageValue() float64 { return m.pct }

// plainAlert does not implement BudgetAlert, so SendAlert must fall through to
// the default branch that re-derives the payload from the value's JSON tags.
type plainAlert struct {
	Team       string  `json:"team"`
	Project    string  `json:"project"`
	Metric     string  `json:"metric"`
	Usage      int64   `json:"usage"`
	Limit      int64   `json:"limit"`
	Percentage float64 `json:"percentage"`
}

type capturedRequest struct {
	method      string
	contentType string
	body        []byte
}

// newCapturingServer starts a stand-in webhook endpoint that records the
// request SendAlert makes and replies with status. The record is guarded by a
// mutex because the handler runs on the test server's own goroutine.
func newCapturingServer(t *testing.T, status int) (*httptest.Server, func() capturedRequest) {
	t.Helper()

	var (
		mu  sync.Mutex
		got capturedRequest
	)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		// assert (not require) because this runs in the server goroutine.
		assert.NoError(t, err, "test webhook server could not read the request body")

		mu.Lock()
		got = capturedRequest{
			method:      r.Method,
			contentType: r.Header.Get("Content-Type"),
			body:        body,
		}
		mu.Unlock()

		w.WriteHeader(status)
	}))
	t.Cleanup(ts.Close)

	return ts, func() capturedRequest {
		mu.Lock()
		defer mu.Unlock()
		return got
	}
}

func TestNewDispatcher(t *testing.T) {
	d := NewDispatcher("https://hooks.example.com", slog.Default())
	assert.NotNil(t, d)
	assert.Equal(t, "https://hooks.example.com", d.webhookURL)
}

func TestNewDispatcher_NilLogger(t *testing.T) {
	d := NewDispatcher("https://hooks.example.com", nil)
	assert.NotNil(t, d)
}

func TestDispatcher_EmptyURL(t *testing.T) {
	d := NewDispatcher("", slog.Default())
	assert.NotNil(t, d)

	err := d.SendAlert(mockAlert{team: "eng"})
	assert.NoError(t, err)
}

func TestDispatcher_SendAlert_InvalidURL(t *testing.T) {
	d := NewDispatcher("http://127.0.0.1:1", slog.Default())

	err := d.SendAlert(mockAlert{team: "eng", usage: 100, limit: 200, pct: 50.0})
	assert.Error(t, err)
}

func TestSendAlertPostsBudgetAlertFieldsAsJSON(t *testing.T) {
	ts, captured := newCapturingServer(t, http.StatusOK)

	d := NewDispatcher(ts.URL, slog.Default())
	err := d.SendAlert(mockAlert{
		team:    "eng",
		project: "web",
		metric:  "daily",
		usage:   80000,
		limit:   100000,
		pct:     80.0,
	})
	require.NoError(t, err, "SendAlert to a healthy webhook endpoint")

	got := captured()
	assert.Equal(t, http.MethodPost, got.method, "webhook request method")
	assert.Equal(t, "application/json", got.contentType, "webhook Content-Type header")

	var payload BudgetAlertPayload
	require.NoError(t, json.Unmarshal(got.body, &payload),
		"posted body is not valid BudgetAlertPayload JSON: %s", string(got.body))

	// Timestamp is generated at send time and so cannot be compared literally;
	// assert it is RFC3339 and zero it before comparing the rest of the payload.
	_, tsErr := time.Parse(time.RFC3339, payload.Timestamp)
	assert.NoError(t, tsErr, "payload timestamp %q is not RFC3339", payload.Timestamp)
	payload.Timestamp = ""

	assert.Equal(t, BudgetAlertPayload{
		Team:       "eng",
		Project:    "web",
		Metric:     "daily",
		Usage:      80000,
		Limit:      100000,
		Percentage: 80.0,
	}, payload, "posted budget alert payload")
}

func TestSendAlertPostsNonBudgetAlertFieldsFromItsJSONTags(t *testing.T) {
	ts, captured := newCapturingServer(t, http.StatusOK)

	d := NewDispatcher(ts.URL, slog.Default())
	err := d.SendAlert(plainAlert{
		Team:       "platform",
		Project:    "api",
		Metric:     "monthly",
		Usage:      450000,
		Limit:      500000,
		Percentage: 90.0,
	})
	require.NoError(t, err, "SendAlert with a non-BudgetAlert value")

	got := captured()
	var payload BudgetAlertPayload
	require.NoError(t, json.Unmarshal(got.body, &payload),
		"posted body is not valid BudgetAlertPayload JSON: %s", string(got.body))
	payload.Timestamp = ""

	assert.Equal(t, BudgetAlertPayload{
		Team:       "platform",
		Project:    "api",
		Metric:     "monthly",
		Usage:      450000,
		Limit:      500000,
		Percentage: 90.0,
	}, payload, "posted payload derived from the alert's JSON tags")
}

func TestSendAlertReturnsErrorWhenWebhookRespondsWithFailureStatus(t *testing.T) {
	ts, _ := newCapturingServer(t, http.StatusInternalServerError)

	d := NewDispatcher(ts.URL, slog.Default())
	err := d.SendAlert(mockAlert{team: "eng", usage: 100, limit: 200, pct: 50.0})

	require.Error(t, err, "SendAlert should fail when the webhook answers 500")
	assert.Contains(t, err.Error(), "500", "error should name the rejected status code")
}
