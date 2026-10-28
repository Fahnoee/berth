package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type client struct {
	t      *testing.T
	w      io.WriteCloser
	r      *bufio.Reader
	nextID int
	done   chan error
}

func newClient(t *testing.T) *client {
	t.Helper()
	t.Setenv("BERTH_HOME", t.TempDir())
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	c := &client{t: t, w: inW, r: bufio.NewReader(outR), done: make(chan error, 1)}
	go func() {
		err := Serve(ctx, inR, outW)
		outW.Close()
		c.done <- err
	}()
	t.Cleanup(func() {
		inW.Close()
		select {
		case err := <-c.done:
			if err != nil {
				t.Errorf("Serve returned %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Serve did not return after stdin closed")
		}
		cancel()
	})
	return c
}

func (c *client) send(v any) {
	c.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.w.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
}

type rpcResp struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

func (c *client) recv() rpcResp {
	c.t.Helper()
	type res struct {
		line []byte
		err  error
	}
	ch := make(chan res, 1)
	go func() {
		l, err := c.r.ReadBytes('\n')
		ch <- res{l, err}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			c.t.Fatalf("read response: %v", r.err)
		}
		var resp rpcResp
		if err := json.Unmarshal(r.line, &resp); err != nil {
			c.t.Fatalf("bad response %q: %v", r.line, err)
		}
		if resp.JSONRPC != "2.0" {
			c.t.Fatalf("bad jsonrpc version in %s", r.line)
		}
		return resp
	case <-time.After(60 * time.Second):
		c.t.Fatal("timed out waiting for response")
	}
	return rpcResp{}
}

func (c *client) call(method string, params any) rpcResp {
	c.t.Helper()
	c.nextID++
	id := c.nextID
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	c.send(msg)
	resp := c.recv()
	if string(resp.ID) != strconv.Itoa(id) {
		c.t.Fatalf("response id %s, want %d", resp.ID, id)
	}
	return resp
}

func (c *client) callTool(name string, args map[string]any) toolResult {
	c.t.Helper()
	resp := c.call("tools/call", map[string]any{"name": name, "arguments": args})
	if resp.Error != nil {
		c.t.Fatalf("%s: rpc error %+v", name, resp.Error)
	}
	var r toolResult
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		c.t.Fatal(err)
	}
	if len(r.Content) == 0 || r.Content[0].Type != "text" {
		c.t.Fatalf("%s: unexpected content %s", name, resp.Result)
	}
	return r
}

func (c *client) initialize(version string) map[string]any {
	c.t.Helper()
	resp := c.call("initialize", map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "0"},
	})
	if resp.Error != nil {
		c.t.Fatalf("initialize: %+v", resp.Error)
	}
	var r map[string]any
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		c.t.Fatal(err)
	}
	c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	return r
}

func TestInitialize(t *testing.T) {
	for _, tc := range []struct{ ask, want string }{
		{"2024-11-05", "2024-11-05"},
		{"2025-03-26", "2025-03-26"},
		{"2025-06-18", "2025-06-18"},
		{"1999-01-01", "2025-06-18"},
	} {
		c := newClient(t)
		r := c.initialize(tc.ask)
		if r["protocolVersion"] != tc.want {
			t.Errorf("asked %s: got protocolVersion %v, want %s", tc.ask, r["protocolVersion"], tc.want)
		}
		info, _ := r["serverInfo"].(map[string]any)
		if info["name"] != "berth" {
			t.Errorf("serverInfo = %v", r["serverInfo"])
		}
		caps, _ := r["capabilities"].(map[string]any)
		if _, ok := caps["tools"]; !ok {
			t.Errorf("capabilities lack tools: %v", caps)
		}
		instr, _ := r["instructions"].(string)
		for _, w := range []string{"berth_run", "berth_port", "berth_stop", "&"} {
			if !strings.Contains(instr, w) {
				t.Errorf("instructions missing %q", w)
			}
		}
		// The initialized notification must not produce a response: the next
		// thing we read must be the ping reply.
		if resp := c.call("ping", nil); resp.Error != nil || string(resp.Result) != "{}" {
			t.Errorf("ping: %+v %s", resp.Error, resp.Result)
		}
	}
}

func TestToolsList(t *testing.T) {
	c := newClient(t)
	c.initialize("2025-06-18")
	resp := c.call("tools/list", map[string]any{})
	if resp.Error != nil {
		t.Fatal(resp.Error)
	}
	var r struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"berth_port": {"service"},
		"berth_run":  {"command"},
		"berth_ps":   nil,
		"berth_logs": {"id"},
		"berth_stop": nil,
		"berth_reap": nil,
	}
	got := map[string]bool{}
	for _, tl := range r.Tools {
		got[tl.Name] = true
		req, ok := want[tl.Name]
		if !ok {
			t.Errorf("unexpected tool %s", tl.Name)
			continue
		}
		if tl.Description == "" {
			t.Errorf("%s has no description", tl.Name)
		}
		s := tl.InputSchema
		if s["type"] != "object" {
			t.Errorf("%s schema type = %v", tl.Name, s["type"])
		}
		props, _ := s["properties"].(map[string]any)
		if _, ok := props["cwd"]; !ok {
			t.Errorf("%s schema lacks cwd", tl.Name)
		}
		var gotReq []string
		if rr, ok := s["required"].([]any); ok {
			for _, x := range rr {
				gotReq = append(gotReq, x.(string))
				if _, ok := props[x.(string)]; !ok {
					t.Errorf("%s requires undeclared %v", tl.Name, x)
				}
			}
		}
		if fmt.Sprint(gotReq) != fmt.Sprint(req) {
			t.Errorf("%s required = %v, want %v", tl.Name, gotReq, req)
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("tool %s missing", name)
		}
	}
}

func TestPort(t *testing.T) {
	c := newClient(t)
	c.initialize("2025-06-18")
	dir := t.TempDir()
	r := c.callTool("berth_port", map[string]any{"service": "web", "cwd": dir})
	if r.IsError {
		t.Fatalf("error: %s", r.Content[0].Text)
	}
	first := strings.SplitN(r.Content[0].Text, "\n", 2)[0]
	port, err := strconv.Atoi(first)
	if err != nil || port < 20000 || port > 29999 {
		t.Fatalf("port %q not in 20000-29999 (text %q)", first, r.Content[0].Text)
	}
	// Stable on repeat.
	r2 := c.callTool("berth_port", map[string]any{"service": "web", "cwd": dir})
	if got := strings.SplitN(r2.Content[0].Text, "\n", 2)[0]; got != first {
		t.Errorf("second lease %s != %s", got, first)
	}
	// Missing service is a tool error, not an rpc error.
	if r := c.callTool("berth_port", map[string]any{}); !r.IsError {
		t.Errorf("expected isError for missing service")
	}
}

func TestErrors(t *testing.T) {
	c := newClient(t)
	c.initialize("2025-06-18")
	resp := c.call("does/not/exist", nil)
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("want -32601, got %+v", resp.Error)
	}
	// Unknown notifications are ignored silently; malformed JSON gets a parse error.
	c.send(map[string]any{"jsonrpc": "2.0", "method": "bogus/notification"})
	if _, err := c.w.Write([]byte("{not json\n")); err != nil {
		t.Fatal(err)
	}
	if resp := c.recv(); resp.Error == nil || resp.Error.Code != -32700 {
		t.Fatalf("want -32700, got %+v", resp.Error)
	}
	if r := c.callTool("berth_stop", map[string]any{}); !r.IsError {
		t.Errorf("berth_stop with no target should be an error")
	}
	if resp := c.call("tools/call", map[string]any{"name": "nope"}); resp.Error == nil {
		t.Errorf("unknown tool should be an rpc error")
	}
}

func TestRunStop(t *testing.T) {
	c := newClient(t)
	c.initialize("2025-06-18")
	dir := t.TempDir()
	r := c.callTool("berth_run", map[string]any{
		"command": []string{"sleep", "30"}, "service": "sleeper", "cwd": dir, "wait_seconds": 0,
	})
	if r.IsError {
		t.Fatalf("run: %s", r.Content[0].Text)
	}
	text := r.Content[0].Text
	id := regexp.MustCompile(`(?m)^id: (\S+)`).FindStringSubmatch(text)
	pidm := regexp.MustCompile(`(?m)^pid: (\d+)`).FindStringSubmatch(text)
	if id == nil || pidm == nil {
		t.Fatalf("no id/pid in %q", text)
	}
	pid, _ := strconv.Atoi(pidm[1])
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL); _ = syscall.Kill(pid, syscall.SIGKILL) })

	// Starting again returns the existing one.
	r = c.callTool("berth_run", map[string]any{
		"command": []string{"sleep", "30"}, "service": "sleeper", "cwd": dir, "wait_seconds": 0,
	})
	if r.IsError || !strings.Contains(r.Content[0].Text, "already running") || !strings.Contains(r.Content[0].Text, id[1]) {
		t.Errorf("second run: %s", r.Content[0].Text)
	}

	r = c.callTool("berth_ps", map[string]any{"cwd": dir})
	if r.IsError || !strings.Contains(r.Content[0].Text, id[1]) {
		t.Errorf("ps: %s", r.Content[0].Text)
	}

	r = c.callTool("berth_logs", map[string]any{"id": id[1]})
	if r.IsError {
		t.Errorf("logs: %s", r.Content[0].Text)
	}

	r = c.callTool("berth_stop", map[string]any{"id": id[1]})
	if r.IsError {
		t.Fatalf("stop: %s", r.Content[0].Text)
	}
	if err := syscall.Kill(pid, 0); err == nil {
		// Might be a zombie briefly; give the reaper goroutine a moment.
		time.Sleep(200 * time.Millisecond)
		if err := syscall.Kill(pid, 0); err == nil {
			t.Errorf("pid %d still alive after berth_stop", pid)
		}
	}
	r = c.callTool("berth_ps", map[string]any{"cwd": dir})
	if strings.Contains(r.Content[0].Text, id[1]) {
		t.Errorf("still listed after stop: %s", r.Content[0].Text)
	}
}

func TestRunDiesEarly(t *testing.T) {
	c := newClient(t)
	c.initialize("2025-06-18")
	r := c.callTool("berth_run", map[string]any{
		"command": []string{"sh", "-c", "echo boom-from-child; exit 3"}, "service": "crasher",
		"cwd": t.TempDir(), "wait_seconds": 10,
	})
	if !r.IsError || !strings.Contains(r.Content[0].Text, "boom-from-child") {
		t.Errorf("want error with log tail, got isError=%v %s", r.IsError, r.Content[0].Text)
	}
}
