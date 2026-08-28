// Package e2e contains the end-to-end A/B benchmark harness. Unlike
// tests/bench/token_economy_bench_test.go, which is a pure accounting model,
// this package starts the real proxy and measures the bytes a client actually
// receives.
package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
)

// Client is a minimal stdio JSON-RPC MCP client. It speaks just enough of the
// protocol to capture a tools/list payload: initialize, initialized, tools/list.
type Client struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	next int
}

// Dial starts bin with args and attaches to its stdin/stdout.
func Dial(bin string, args ...string) (*Client, error) {
	cmd := exec.Command(bin, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}
	return &Client{cmd: cmd, in: stdin, out: bufio.NewReaderSize(stdout, 1<<20), next: 1}, nil
}

// call sends one JSON-RPC request and returns the raw `result` bytes.
func (c *Client) call(method string, params any) (json.RawMessage, error) {
	id := c.next
	c.next++

	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal %s: %w", method, err)
	}
	if _, err := c.in.Write(append(body, '\n')); err != nil {
		return nil, fmt.Errorf("write %s: %w", method, err)
	}

	line, err := c.out.ReadBytes('\n')
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", method, err)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("parse %s response: %w (line: %s)", method, err, line)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("%s failed: %d %s", method, resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

// notify sends a JSON-RPC notification, which takes no response.
func (c *Client) notify(method string) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return err
	}
	_, err = c.in.Write(append(body, '\n'))
	return err
}

// Initialize performs the MCP handshake.
func (c *Client) Initialize() error {
	_, err := c.call("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "leanproxy-ab-harness", "version": "1"},
	})
	if err != nil {
		return err
	}
	return c.notify("notifications/initialized")
}

// ToolsListRaw returns the raw result object from tools/list. This is the exact
// payload a client holds in context, which is the quantity under measurement.
func (c *Client) ToolsListRaw() ([]byte, error) {
	res, err := c.call("tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// Close shuts stdin and waits for the subprocess to exit.
func (c *Client) Close() error {
	_ = c.in.Close()
	_ = c.cmd.Process.Kill()
	_, _ = c.cmd.Process.Wait()
	return nil
}
