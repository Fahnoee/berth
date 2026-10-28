// Package mcp exposes berth to coding agents as a Model Context Protocol
// server speaking JSON-RPC 2.0 over newline-delimited JSON on stdio.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

const (
	latestProtocol = "2025-06-18"
	serverName     = "berth"
	serverVersion  = "0.1.0"
)

var supportedProtocols = []string{"2024-11-05", "2025-03-26", "2025-06-18"}

// Instructions are sent to the client in the initialize result.
const Instructions = `berth tracks the dev servers and long-running processes you start, gives every git worktree its own stable ports, and cleans up after you.
- Use berth_run for any long-running process (dev servers, watchers, databases) instead of backgrounding it with "&" or nohup. It returns the port, URL, log path and whether it is ready.
- Use berth_port to get a port for a service instead of hardcoding 3000, 5173, 8080 etc. — other agents in other worktrees are using those.
- Use berth_ps to see what is running, berth_logs to read a process's output.
- When you are done, call berth_stop on what you started (or berth_reap with worktree_only to clean up everything in this worktree).`

// JSON-RPC error codes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r request) isNotification() bool { return len(r.ID) == 0 }

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve runs an MCP server reading requests from in and writing responses to
// out until in reaches EOF or ctx is cancelled. Requests are handled one at a
// time, in order.
func Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	lines := make(chan []byte)
	readErr := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		r := bufio.NewReaderSize(in, 1<<16)
		for {
			line, err := r.ReadBytes('\n')
			if len(bytes.TrimSpace(line)) > 0 {
				select {
				case lines <- line:
				case <-done:
					return
				}
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	s := &server{out: out}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-readErr:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case line := <-lines:
			if err := s.handleLine(ctx, line); err != nil {
				return err
			}
		}
	}
}

type server struct {
	mu  sync.Mutex
	out io.Writer
}

func (s *server) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.out.Write(b)
	return err
}

func (s *server) handleLine(ctx context.Context, line []byte) error {
	line = bytes.TrimSpace(line)
	if len(line) > 0 && line[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(line, &batch); err != nil {
			return s.write(errResp(nil, codeParseError, "parse error: "+err.Error()))
		}
		if len(batch) == 0 {
			return s.write(errResp(nil, codeInvalidRequest, "empty batch"))
		}
		var resps []response
		for _, raw := range batch {
			if r := s.handleMessage(ctx, raw); r != nil {
				resps = append(resps, *r)
			}
		}
		if len(resps) == 0 {
			return nil
		}
		return s.write(resps)
	}
	if r := s.handleMessage(ctx, line); r != nil {
		return s.write(r)
	}
	return nil
}

func errResp(id json.RawMessage, code int, msg string) *response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return &response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// handleMessage processes one JSON-RPC message and returns the response to
// send, or nil for notifications and client responses.
func (s *server) handleMessage(ctx context.Context, raw []byte) *response {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return errResp(nil, codeParseError, "parse error: "+err.Error())
	}
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return errResp(probe["id"], codeInvalidRequest, "invalid request: "+err.Error())
	}
	if req.Method == "" {
		// A response to something we sent (we never send requests), or junk.
		if _, isResp := probe["result"]; isResp {
			return nil
		}
		if _, isResp := probe["error"]; isResp {
			return nil
		}
		if req.isNotification() {
			return nil
		}
		return errResp(req.ID, codeInvalidRequest, "missing method")
	}
	if string(req.ID) == "null" {
		req.ID = nil
	}

	result, rerr := s.dispatch(ctx, req)
	if req.isNotification() {
		return nil
	}
	if rerr != nil {
		return errResp(req.ID, rerr.Code, rerr.Message)
	}
	return &response{JSONRPC: "2.0", ID: req.ID, Result: result}
}

func (s *server) dispatch(ctx context.Context, req request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		version := latestProtocol
		if slices.Contains(supportedProtocols, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
			"instructions":    Instructions,
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": toolList()}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
		}
		t, ok := lookupTool(p.Name)
		if !ok {
			return nil, &rpcError{Code: codeInvalidParams, Message: fmt.Sprintf("unknown tool %q", p.Name)}
		}
		args := p.Arguments
		if len(args) == 0 || string(args) == "null" {
			args = json.RawMessage("{}")
		}
		return callTool(ctx, t, args), nil
	default:
		if strings.HasPrefix(req.Method, "notifications/") {
			return nil, nil // notifications/initialized, cancelled, ...: nothing to do
		}
		return nil, &rpcError{Code: codeMethodNotFound, Message: "method not found: " + req.Method}
	}
}

// toolResult is the MCP CallToolResult shape.
type toolResult struct {
	Content []textContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func textResult(format string, a ...any) toolResult {
	return toolResult{Content: []textContent{{Type: "text", Text: fmt.Sprintf(format, a...)}}}
}

func errorResult(format string, a ...any) toolResult {
	r := textResult(format, a...)
	r.IsError = true
	return r
}

func callTool(ctx context.Context, t tool, args json.RawMessage) (res toolResult) {
	defer func() {
		if r := recover(); r != nil {
			res = errorResult("internal error in %s: %v", t.Name, r)
		}
	}()
	return t.handler(ctx, args)
}
