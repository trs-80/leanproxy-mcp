package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/textproto"
	"sync"
	"time"

	"github.com/mmornati/leanproxy-mcp/pkg/errors"
)

type Proxy struct {
	upstreamAddr string
	conn         net.Conn
	logger       *slog.Logger
	mu           sync.Mutex
}

func NewProxy(upstreamAddr string, logger *slog.Logger) *Proxy {
	return &Proxy{
		upstreamAddr: upstreamAddr,
		logger:       logger,
	}
}

func (p *Proxy) Connect(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	conn, err := dialer.DialContext(ctx, "tcp", p.upstreamAddr)
	if err != nil {
		return fmt.Errorf("proxy: connect: %w", err)
	}

	p.conn = conn
	p.logger.Debug("connected to upstream", "addr", p.upstreamAddr)
	return nil
}

func (p *Proxy) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.conn != nil {
		p.conn.Close()
		p.logger.Debug("connection closed")
	}
	return nil
}

func (p *Proxy) ForwardLoop(ctx context.Context, ideConn net.Conn) error {
	if err := p.Connect(ctx); err != nil {
		return err
	}
	defer p.Close()

	errChan := make(chan error, 2)

	go func() {
		_, err := io.Copy(p.conn, ideConn)
		if err != nil {
			p.logger.Debug("ide to upstream copy done", "error", err)
		}
		p.conn.Close()
		errChan <- err
	}()

	go func() {
		_, err := io.Copy(ideConn, p.conn)
		if err != nil {
			p.logger.Debug("upstream to ide copy done", "error", err)
		}
		ideConn.Close()
		errChan <- err
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *Proxy) ForwardLoopWithJSONRPC(ctx context.Context, ideConn net.Conn) error {
	if err := p.Connect(ctx); err != nil {
		return err
	}
	defer p.Close()

	ideReader := textproto.NewReader(bufio.NewReader(ideConn))
	upstreamWriter := bufio.NewWriter(p.conn)
	upstreamReader := textproto.NewReader(bufio.NewReader(p.conn))
	ideWriter := bufio.NewWriter(ideConn)

	var wg sync.WaitGroup
	errChan := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			line, err := ideReader.ReadLine()
			if err != nil {
				if err != io.EOF {
					errChan <- fmt.Errorf("proxy: read from ide: %w", err)
				}
				return
			}

			if _, err := fmt.Fprintln(upstreamWriter, line); err != nil {
				errChan <- fmt.Errorf("proxy: write to upstream: %w", err)
				return
			}
			if err := upstreamWriter.Flush(); err != nil {
				errChan <- fmt.Errorf("proxy: flush to upstream: %w", err)
				return
			}
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			line, err := upstreamReader.ReadLine()
			if err != nil {
				if err != io.EOF {
					errChan <- fmt.Errorf("proxy: read from upstream: %w", err)
				}
				return
			}

			if _, err := fmt.Fprintln(ideWriter, line); err != nil {
				errChan <- fmt.Errorf("proxy: write to ide: %w", err)
				return
			}
			if err := ideWriter.Flush(); err != nil {
				errChan <- fmt.Errorf("proxy: flush to ide: %w", err)
				return
			}
		}
	}()

	go func() {
		wg.Wait()
		close(errChan)
	}()

	select {
	case err := <-errChan:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func ParseJSONRPCRequest(data []byte) (*JSONRPCRequest, error) {
	var req JSONRPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return nil, fmt.Errorf("proxy: parse request: %w", err)
	}
	return &req, nil
}

func ParseJSONRPCResponse(data []byte) (*JSONRPCResponse, error) {
	var resp JSONRPCResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("proxy: parse response: %w", err)
	}
	return &resp, nil
}

// IsBatchRequest reports whether data is a non-empty JSON array. It inspects
// only the leading bytes so the payload is not parsed twice (the caller parses
// it for real afterwards); malformed input is rejected by that later parse.
func IsBatchRequest(data []byte) bool {
	i := skipJSONSpace(data, 0)
	if i >= len(data) || data[i] != '[' {
		return false
	}
	j := skipJSONSpace(data, i+1)
	return j < len(data) && data[j] != ']'
}

func skipJSONSpace(data []byte, i int) int {
	for i < len(data) {
		switch data[i] {
		case ' ', '\t', '\r', '\n':
			i++
		default:
			return i
		}
	}
	return i
}

func ParseJSONRPCBatchRequest(data []byte, maxBatchSize int) ([]JSONRPCRequest, error) {
	var reqs []JSONRPCRequest
	if err := json.Unmarshal(data, &reqs); err != nil {
		return nil, fmt.Errorf("proxy: parse batch request: %w", err)
	}
	if maxBatchSize > 0 && len(reqs) > maxBatchSize {
		return nil, fmt.Errorf("proxy: batch size %d exceeds limit %d", len(reqs), maxBatchSize)
	}
	return reqs, nil
}

type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	ID      interface{}     `json:"id"`
}

type JSONRPCResponse struct {
	JSONRPC string               `json:"jsonrpc"`
	Result  json.RawMessage      `json:"result,omitempty"`
	Error   *errors.JSONRPCError `json:"error,omitempty"`
	ID      interface{}          `json:"id"`
}
