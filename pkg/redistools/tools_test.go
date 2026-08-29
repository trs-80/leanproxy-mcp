package redistools

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testClient() *RedisClient {
	c := &RedisClient{
		config: DefaultConfig(),
		logger: slog.Default(),
		pool:   make(chan *pooledConn, 1),
	}
	c.tools = map[string]ToolHandler{
		toolGet:    c.handleGet,
		toolSet:    c.handleSet,
		toolDelete: c.handleDelete,
		toolKeys:   c.handleKeys,
		toolExists: c.handleExists,
	}
	return c
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	assert.Equal(t, "127.0.0.1:6379", cfg.Address)
	assert.Equal(t, DefaultPoolSize, cfg.PoolSize)
	assert.Equal(t, 0, cfg.DB)
}

func TestGetTools(t *testing.T) {
	client := testClient()
	tools := client.GetTools()
	require.Len(t, tools, 5)

	names := make(map[string]bool)
	for _, tool := range tools {
		names[tool.Name] = true
	}
	assert.True(t, names["redis_get"])
	assert.True(t, names["redis_set"])
	assert.True(t, names["redis_delete"])
	assert.True(t, names["redis_keys"])
	assert.True(t, names["redis_exists"])
}

func TestCallTool_Unknown(t *testing.T) {
	client := testClient()
	_, err := client.CallTool(context.Background(), "unknown", nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown tool")
}

func TestHandleGet_EmptyKey(t *testing.T) {
	client := testClient()
	args, _ := json.Marshal(map[string]string{"key": ""})
	_, err := client.CallTool(context.Background(), toolGet, args)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "'key' is required")
}

func TestHandleSet_EmptyKey(t *testing.T) {
	client := testClient()
	args, _ := json.Marshal(map[string]string{"key": "", "value": "test"})
	_, err := client.CallTool(context.Background(), toolSet, args)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "'key' is required")
}

func TestHandleDelete_EmptyKeys(t *testing.T) {
	client := testClient()
	args, _ := json.Marshal(map[string]interface{}{"keys": []string{}})
	_, err := client.CallTool(context.Background(), toolDelete, args)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "non-empty array")
}

func TestHandleKeys_EmptyPattern(t *testing.T) {
	client := testClient()
	args, _ := json.Marshal(map[string]string{"pattern": ""})
	_, err := client.CallTool(context.Background(), toolKeys, args)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "'pattern' is required")
}

func TestHandleExists_EmptyKeys(t *testing.T) {
	client := testClient()
	args, _ := json.Marshal(map[string]interface{}{"keys": []string{}})
	_, err := client.CallTool(context.Background(), toolExists, args)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "non-empty array")
}

func TestToolSchemasValid(t *testing.T) {
	client := testClient()
	tools := client.GetTools()
	for _, tool := range tools {
		assert.NotEmpty(t, tool.Name, "tool name should not be empty")
		assert.NotEmpty(t, tool.Description, "tool description should not be empty")
		assert.NotEmpty(t, tool.InputSchema, "tool input schema should not be empty")

		var schema map[string]interface{}
		err := json.Unmarshal(tool.InputSchema, &schema)
		assert.NoError(t, err, "tool %s has invalid JSON schema", tool.Name)
		assert.Equal(t, "object", schema["type"], "tool %s should have object type schema", tool.Name)
	}
}

// TestWriteCommandEncodesRESP drives the real encoder over net.Pipe rather than
// rebuilding the RESP framing in the test body. A test that re-implements the
// serializer can never fail — a wrong bulk-string length prefix or a dropped CRLF
// would make a real Redis server reject every command while the test stayed green.
// net.Pipe is unbuffered and synchronous, so the read must run in a goroutine
// concurrently with the write; io.ReadAll (rather than reading exactly len(want)
// bytes) catches an over-long encoding as well as a truncated one, and closing the
// writing end is what gives the reader its EOF.
func TestWriteCommandEncodesRESP(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "simple GET",
			args: []string{"GET", "foo"},
			want: "*2\r\n$3\r\nGET\r\n$3\r\nfoo\r\n",
		},
		{
			name: "SET with value",
			args: []string{"SET", "key", "value"},
			want: "*3\r\n$3\r\nSET\r\n$3\r\nkey\r\n$5\r\nvalue\r\n",
		},
		{
			name: "DEL multiple",
			args: []string{"DEL", "a", "b"},
			want: "*3\r\n$3\r\nDEL\r\n$1\r\na\r\n$1\r\nb\r\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			t.Cleanup(func() {
				clientConn.Close()
				serverConn.Close()
			})

			got := make(chan string, 1)
			go func() {
				b, _ := io.ReadAll(serverConn)
				got <- string(b)
			}()

			c := testClient()
			require.NoError(t, c.writeCommand(clientConn, tt.args...), "writeCommand(%q) returned an error", tt.args)
			require.NoError(t, clientConn.Close(), "closing the write end of the pipe failed")

			select {
			case s := <-got:
				assert.Equal(t, tt.want, s, "writeCommand(%q) wrote the wrong RESP bytes", tt.args)
			case <-time.After(5 * time.Second):
				t.Fatalf("timed out after 5s reading the RESP encoding of %q", tt.args)
			}
		})
	}
}
