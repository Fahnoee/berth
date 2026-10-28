package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Fahnoee/berth/internal/gitwt"
	"github.com/Fahnoee/berth/internal/ports"
	"github.com/Fahnoee/berth/internal/proc"
	"github.com/Fahnoee/berth/internal/runner"
	"github.com/Fahnoee/berth/internal/scan"
	"github.com/Fahnoee/berth/internal/state"
)

type tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	handler     func(context.Context, json.RawMessage) toolResult
}

const stopGrace = 5 * time.Second

var cwdProp = map[string]any{
	"type":        "string",
	"description": "Directory (git worktree) to act on. Defaults to the berth MCP server's working directory. Pass your current project/worktree directory if you work in several.",
}

func schema(required []string, props map[string]any) map[string]any {
	props["cwd"] = cwdProp
	s := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

var tools []tool

func init() {
	tools = []tool{
		{
			Name:  "berth_port",
			Title: "Get a stable port",
			Description: "Get a port for a service in this worktree instead of hardcoding 3000/5173/8080. " +
				"The port is stable for (worktree, service): asking again returns the same number, and different worktrees never collide. " +
				"Use it when you need a port for something you'll start yourself or put in config; prefer berth_run for actually starting servers (it leases the port for you).",
			InputSchema: schema([]string{"service"}, map[string]any{
				"service": map[string]any{"type": "string", "description": `Service name the port is for, e.g. "web", "api", "storybook", "postgres".`},
			}),
			handler: toolPort,
		},
		{
			Name:  "berth_run",
			Title: "Run a long-running process",
			Description: "Start a long-running process (dev server, watcher, database, worker) in the background — use this INSTEAD of `cmd &`, nohup or a background shell. " +
				"berth leases a stable port for the worktree and passes it via the $PORT env var (and $BERTH_PORT); any literal \"{port}\" in the argv is replaced with the port too, so e.g. [\"npm\",\"run\",\"dev\",\"--\",\"--port\",\"{port}\"] works for tools that ignore $PORT. " +
				"Output goes to a log file (read it with berth_logs). It waits until the port accepts connections (up to wait_seconds) and reports id, pid, port, URL and log path. " +
				"If the same service is already running in this worktree it returns the existing one (pass replace:true to restart). Stop it with berth_stop when you're done.",
			InputSchema: schema([]string{"command"}, map[string]any{
				"command": map[string]any{
					"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1,
					"description": `Program and arguments as an argv array, e.g. ["npm","run","dev"] or ["python3","-m","http.server","{port}"]. For shell syntax use ["sh","-c","..."].`,
				},
				"service":      map[string]any{"type": "string", "description": `Service name (port lease key and display name). Defaults to one derived from the command, e.g. "npm-dev".`},
				"port_env":     map[string]any{"type": "string", "description": "Environment variable that receives the port. Default PORT."},
				"replace":      map[string]any{"type": "boolean", "description": "If this service is already running in this worktree, stop it and start a fresh one. Default false."},
				"ttl":          map[string]any{"type": "string", "description": `Optional lifetime as a Go duration ("30m", "2h"); after it the process counts as expired and berth_reap will stop it.`},
				"wait_seconds": map[string]any{"type": "number", "minimum": 0, "description": "How long to wait for the port to accept connections. Default 30. Use 0 to not wait (e.g. for processes that don't listen on a port)."},
			}),
			handler: toolRun,
		},
		{
			Name:  "berth_ps",
			Title: "List running servers",
			Description: "List dev servers and processes listening on ports, with their berth id, pid, ports, service, branch, agent and problem tags " +
				"(worktree-gone, agent-gone, expired, detached = probably abandoned). By default only shows this worktree; pass all:true to see everything on the machine.",
			InputSchema: schema(nil, map[string]any{
				"all": map[string]any{"type": "boolean", "description": "Show processes from every worktree, not just the current one. Default false."},
			}),
			handler: toolPs,
		},
		{
			Name:        "berth_logs",
			Title:       "Read a process's log",
			Description: "Show the last lines of output (stdout+stderr) of a process started with berth_run. Use it to check for compile errors, the actual URL, or why a server crashed.",
			InputSchema: schema([]string{"id"}, map[string]any{
				"id":    map[string]any{"type": "string", "description": "The berth id returned by berth_run / shown by berth_ps (e.g. \"web-3f2a\"). A service name works too if it's unambiguous."},
				"lines": map[string]any{"type": "integer", "minimum": 1, "description": "Number of lines from the end. Default 100."},
			}),
			handler: toolLogs,
		},
		{
			Name:  "berth_stop",
			Title: "Stop a process",
			Description: "Stop a process (and its process group) gracefully: SIGTERM, then SIGKILL after 5s. Identify it by berth id, by a port it listens on, or by pid — give exactly one. " +
				"Call this on whatever you started with berth_run once you no longer need it.",
			InputSchema: schema(nil, map[string]any{
				"id":   map[string]any{"type": "string", "description": "berth id from berth_run / berth_ps."},
				"port": map[string]any{"type": "integer", "description": "A TCP port the process is listening on."},
				"pid":  map[string]any{"type": "integer", "description": "Process id."},
			}),
			handler: toolStop,
		},
		{
			Name:  "berth_reap",
			Title: "Clean up abandoned processes",
			Description: "Stop abandoned processes: ones whose worktree was deleted, whose agent session exited, or that outlived their ttl. " +
				"With worktree_only:true it instead cleans up everything I started in this worktree (every server running in the current worktree, abandoned or not) — use that when you're finishing a task. " +
				"Use dry_run:true first to see what would be stopped.",
			InputSchema: schema(nil, map[string]any{
				"dry_run":       map[string]any{"type": "boolean", "description": "Only report what would be stopped. Default false."},
				"worktree_only": map[string]any{"type": "boolean", "description": "Clean up everything I started in this worktree: stop every process running in the current worktree (and release its port leases), not just abandoned ones. Default false."},
			}),
			handler: toolReap,
		},
	}
}

func toolList() []tool { return tools }

func lookupTool(name string) (tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return tool{}, false
}

func decode(args json.RawMessage, v any) error {
	d := json.NewDecoder(bytes.NewReader(args))
	if err := d.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// where resolves the cwd argument to an absolute directory and its worktree.
func where(cwd string) (string, gitwt.Info, error) {
	if cwd == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", gitwt.Info{}, err
		}
		cwd = wd
	}
	if strings.HasPrefix(cwd, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			cwd = filepath.Join(home, cwd[2:])
		}
	}
	abs, err := filepath.Abs(cwd)
	if err != nil {
		return "", gitwt.Info{}, err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", gitwt.Info{}, fmt.Errorf("cwd %s: %w", abs, err)
	}
	if !fi.IsDir() {
		return "", gitwt.Info{}, fmt.Errorf("cwd %s is not a directory", abs)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	return abs, gitwt.Resolve(abs), nil
}

// ---- berth_port

func toolPort(_ context.Context, raw json.RawMessage) toolResult {
	var a struct {
		Service string `json:"service"`
		Cwd     string `json:"cwd"`
	}
	if err := decode(raw, &a); err != nil {
		return errorResult("%v", err)
	}
	a.Service = strings.TrimSpace(a.Service)
	if a.Service == "" {
		return errorResult("service is required")
	}
	_, wt, err := where(a.Cwd)
	if err != nil {
		return errorResult("%v", err)
	}
	port, fresh, err := ports.Lease(wt.Root, a.Service)
	if err != nil {
		return errorResult("lease port: %v", err)
	}
	how := "is leased"
	if fresh {
		how = "was newly leased"
	}
	return textResult("%d\nPort %d %s to service %q in worktree %s; it stays the same for this worktree and service. Use it instead of a hardcoded default port.",
		port, port, how, a.Service, wt.Root)
}

// ---- berth_run

func toolRun(ctx context.Context, raw json.RawMessage) toolResult {
	var a struct {
		Command     []string `json:"command"`
		Service     string   `json:"service"`
		Cwd         string   `json:"cwd"`
		PortEnv     string   `json:"port_env"`
		Replace     bool     `json:"replace"`
		TTL         string   `json:"ttl"`
		WaitSeconds *float64 `json:"wait_seconds"`
	}
	if err := decode(raw, &a); err != nil {
		return errorResult("%v", err)
	}
	if len(a.Command) == 0 || a.Command[0] == "" {
		return errorResult("command is required: an argv array such as [\"npm\",\"run\",\"dev\"]")
	}
	dir, _, err := where(a.Cwd)
	if err != nil {
		return errorResult("%v", err)
	}
	var ttl time.Duration
	if a.TTL != "" {
		if ttl, err = time.ParseDuration(a.TTL); err != nil {
			return errorResult("invalid ttl %q: %v (use a Go duration like \"30m\" or \"2h\")", a.TTL, err)
		}
	}
	wait := 30 * time.Second
	if a.WaitSeconds != nil {
		if *a.WaitSeconds < 0 {
			return errorResult("wait_seconds must be >= 0")
		}
		wait = time.Duration(*a.WaitSeconds * float64(time.Second))
	}

	h, err := runner.Start(runner.Spec{
		Name: a.Service, Cmd: a.Command, Dir: dir, PortEnv: a.PortEnv,
		Detach: true, Replace: a.Replace, TTL: ttl,
	})
	if errors.Is(err, runner.ErrAlreadyRunning) {
		return textResult("%s was already running in this worktree (not restarted; pass replace:true to restart).\n%s",
			h.Proc.Name, describe(h))
	}
	if err != nil {
		return errorResult("start %s: %v", strings.Join(a.Command, " "), err)
	}

	if wait <= 0 || h.Proc.Port == 0 {
		return textResult("Started (not waiting for readiness).\n%s", describe(h))
	}
	werr := waitReady(ctx, h.Proc, wait)
	if werr == nil {
		return textResult("Started and ready: %s is accepting connections.\n%s", h.URL(), describe(h))
	}
	if !proc.Alive(h.Proc.PID, h.Proc.StartTime) {
		return errorResult("%s exited before becoming ready: %v\n%s\nLast 40 lines of %s:\n%s",
			h.Proc.Name, werr, describe(h), h.Proc.Log, tailOrNote(h.Proc.Log, 40))
	}
	return textResult("Started, but not ready yet: %v. It is still running (it may still be compiling, or it may listen on a different port than $%s — check berth_logs).\n%s\nLast 20 lines of log:\n%s",
		werr, portEnvName(a.PortEnv), describe(h), tailOrNote(h.Proc.Log, 20))
}

// waitReady wraps runner.WaitReady so a cancelled request doesn't block.
func waitReady(ctx context.Context, p state.Proc, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- runner.WaitReady(p, timeout) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func portEnvName(s string) string {
	if s == "" {
		return "PORT"
	}
	return s
}

func describe(h runner.Handle) string {
	p := h.Proc
	var b strings.Builder
	fmt.Fprintf(&b, "id: %s\npid: %d\n", p.ID, p.PID)
	if p.Port != 0 {
		fmt.Fprintf(&b, "port: %d\nurl: %s\n", p.Port, h.URL())
	}
	fmt.Fprintf(&b, "service: %s\ncommand: %s\ndir: %s\n", p.Name, strings.Join(p.Cmd, " "), p.Dir)
	if p.Log != "" {
		fmt.Fprintf(&b, "log: %s\n", p.Log)
	}
	if p.TTL > 0 {
		fmt.Fprintf(&b, "ttl: %s\n", time.Duration(p.TTL))
	}
	fmt.Fprintf(&b, "Stop it with berth_stop {\"id\": %q} when you're done.", p.ID)
	return b.String()
}

// ---- berth_ps

func toolPs(_ context.Context, raw json.RawMessage) toolResult {
	var a struct {
		All bool   `json:"all"`
		Cwd string `json:"cwd"`
	}
	if err := decode(raw, &a); err != nil {
		return errorResult("%v", err)
	}
	_, wt, err := where(a.Cwd)
	if err != nil {
		return errorResult("%v", err)
	}
	entries, err := scan.Scan(scan.Options{})
	if err != nil {
		return errorResult("scan: %v", err)
	}
	if !a.All {
		entries = slices.DeleteFunc(entries, func(e scan.Entry) bool { return !inWorktree(e, wt.Root) })
	}
	if len(entries) == 0 {
		if a.All {
			return textResult("Nothing is listening or tracked by berth.")
		}
		return textResult("Nothing running in worktree %s (pass all:true to see other worktrees).", wt.Root)
	}
	var b strings.Builder
	if a.All {
		fmt.Fprintf(&b, "%d process(es) on this machine:\n", len(entries))
	} else {
		fmt.Fprintf(&b, "%d process(es) in worktree %s:\n", len(entries), wt.Root)
	}
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPID\tPORTS\tSERVICE\tBRANCH\tAGENT\tUPTIME\tPROBLEMS\tCOMMAND")
	now := time.Now()
	for _, e := range entries {
		agent := e.Agent
		if agent != "" && e.AgentPID > 0 && !e.AgentAlive {
			agent += "(gone)"
		}
		probs := strings.Join(e.Problems, ",")
		if e.TTL != "" && probs == "" {
			probs = "ttl=" + e.TTL
		}
		branch := e.Branch
		if a.All && e.Worktree != "" {
			branch = filepath.Base(e.Worktree) + ":" + branch
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			dash(e.ID), e.PID, dash(joinInts(e.Ports)), dash(e.Label()), dash(branch), dash(agent),
			dash(uptime(now, e.Started)), dash(probs), truncate(e.Command, 80))
	}
	tw.Flush()
	if slices.ContainsFunc(entries, func(e scan.Entry) bool { return len(e.Problems) > 0 }) {
		b.WriteString("\nProblem tags mean the process looks abandoned; berth_reap stops those (detached ones need care: they may be intentional).")
	}
	return textResult("%s", strings.TrimRight(b.String(), "\n"))
}

func inWorktree(e scan.Entry, root string) bool {
	return e.Worktree == root || scan.Within(e.Cwd, root)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func joinInts(xs []int) string {
	s := make([]string, len(xs))
	for i, x := range xs {
		s[i] = strconv.Itoa(x)
	}
	return strings.Join(s, ",")
}

func uptime(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ---- berth_logs

func toolLogs(_ context.Context, raw json.RawMessage) toolResult {
	var a struct {
		ID    string `json:"id"`
		Lines int    `json:"lines"`
		Cwd   string `json:"cwd"`
	}
	if err := decode(raw, &a); err != nil {
		return errorResult("%v", err)
	}
	if a.ID == "" {
		return errorResult("id is required (see berth_ps)")
	}
	if a.Lines <= 0 {
		a.Lines = 100
	}
	st, err := state.Load()
	if err != nil {
		return errorResult("load state: %v", err)
	}
	p, ok := findProc(st.Procs, a.ID, a.Cwd)
	if !ok {
		return errorResult("no tracked process %q (it may have exited; only processes started with berth_run have logs — see berth_ps)", a.ID)
	}
	if p.Log == "" {
		return errorResult("%s has no log file (it was started in the foreground)", p.ID)
	}
	text, err := tail(p.Log, a.Lines)
	if err != nil {
		return errorResult("read log %s: %v", p.Log, err)
	}
	status := "running"
	if !proc.Alive(p.PID, p.StartTime) {
		status = "exited"
	}
	if text == "" {
		text = "(log is empty)"
	}
	return textResult("%s (pid %d, %s) — last %d lines of %s:\n%s", p.ID, p.PID, status, a.Lines, p.Log, text)
}

// findProc matches by exact id, then by service name (preferring the given
// worktree) if unambiguous.
func findProc(procs []state.Proc, id, cwd string) (state.Proc, bool) {
	for _, p := range procs {
		if p.ID == id {
			return p, true
		}
	}
	var byName []state.Proc
	for _, p := range procs {
		if p.Name == id {
			byName = append(byName, p)
		}
	}
	if len(byName) > 1 {
		if _, wt, err := where(cwd); err == nil {
			byName = slices.DeleteFunc(byName, func(p state.Proc) bool { return p.Worktree != wt.Root })
		}
	}
	if len(byName) == 1 {
		return byName[0], true
	}
	return state.Proc{}, false
}

func tailOrNote(path string, n int) string {
	if path == "" {
		return "(no log)"
	}
	s, err := tail(path, n)
	if err != nil {
		return fmt.Sprintf("(could not read log: %v)", err)
	}
	if s == "" {
		return "(log is empty)"
	}
	return s
}

// tail returns the last n lines of a file, reading at most the last 1 MiB.
func tail(path string, n int) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	const max = 1 << 20
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	off := int64(0)
	if fi.Size() > max {
		off = fi.Size() - max
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return "", err
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	s := strings.TrimRight(string(b), "\n")
	if s == "" {
		return "", nil
	}
	lines := strings.Split(s, "\n")
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // first line is probably partial
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), nil
}

// ---- berth_stop

func toolStop(_ context.Context, raw json.RawMessage) toolResult {
	var a struct {
		ID   string `json:"id"`
		Port int    `json:"port"`
		PID  int    `json:"pid"`
		Cwd  string `json:"cwd"`
	}
	if err := decode(raw, &a); err != nil {
		return errorResult("%v", err)
	}
	if a.ID == "" && a.Port == 0 && a.PID == 0 {
		return errorResult("give one of id, port or pid (see berth_ps)")
	}
	entries, err := scan.Scan(scan.Options{})
	if err != nil {
		return errorResult("scan: %v", err)
	}
	var match []scan.Entry
	for _, e := range entries {
		switch {
		case a.ID != "" && e.ID == a.ID,
			a.PID != 0 && e.PID == a.PID,
			a.Port != 0 && slices.Contains(e.Ports, a.Port):
			match = append(match, e)
		}
	}
	if len(match) == 0 && a.ID != "" {
		// Fall back to a service name in this worktree.
		if _, wt, err := where(a.Cwd); err == nil {
			for _, e := range entries {
				if e.Tracked && e.Service == a.ID && inWorktree(e, wt.Root) {
					match = append(match, e)
				}
			}
		}
	}
	switch len(match) {
	case 0:
		return errorResult("nothing found matching %s (it may already have exited; see berth_ps)", criteria(a.ID, a.Port, a.PID))
	case 1:
	default:
		var b strings.Builder
		for _, e := range match {
			fmt.Fprintf(&b, "\n  %s pid %d ports %s", dash(e.ID), e.PID, dash(joinInts(e.Ports)))
		}
		return errorResult("%s matches %d processes; be more specific:%s", criteria(a.ID, a.Port, a.PID), len(match), b.String())
	}
	e := match[0]
	if err := scan.Stop(e, stopGrace); err != nil {
		return errorResult("stop pid %d: %v", e.PID, err)
	}
	return textResult("Stopped %s (pid %d%s).", label(e), e.PID, portsSuffix(e))
}

func criteria(id string, port, pid int) string {
	var c []string
	if id != "" {
		c = append(c, "id "+strconv.Quote(id))
	}
	if port != 0 {
		c = append(c, fmt.Sprintf("port %d", port))
	}
	if pid != 0 {
		c = append(c, fmt.Sprintf("pid %d", pid))
	}
	return strings.Join(c, " / ")
}

func label(e scan.Entry) string {
	if e.ID != "" {
		return e.ID
	}
	return e.Label()
}

func portsSuffix(e scan.Entry) string {
	if len(e.Ports) == 0 {
		return ""
	}
	return ", ports " + joinInts(e.Ports)
}

// ---- berth_reap

func toolReap(_ context.Context, raw json.RawMessage) toolResult {
	var a struct {
		DryRun       bool   `json:"dry_run"`
		WorktreeOnly bool   `json:"worktree_only"`
		Cwd          string `json:"cwd"`
	}
	if err := decode(raw, &a); err != nil {
		return errorResult("%v", err)
	}
	opts := scan.ReapOptions{DryRun: a.DryRun, Grace: stopGrace}
	if a.WorktreeOnly {
		_, wt, err := where(a.Cwd)
		if err != nil {
			return errorResult("%v", err)
		}
		opts.Worktree = wt.Root
	}
	entries, err := scan.Scan(scan.Options{})
	if err != nil {
		return errorResult("scan: %v", err)
	}
	reaped, leases, errs := scan.Reap(entries, opts)

	var b strings.Builder
	verb := "Stopped"
	if a.DryRun {
		verb = "Would stop"
	}
	scope := "abandoned process(es)"
	if a.WorktreeOnly {
		scope = "process(es) in worktree " + opts.Worktree
	}
	if len(reaped) == 0 {
		b.WriteString("No abandoned processes to stop.")
		if a.WorktreeOnly {
			b.Reset()
			fmt.Fprintf(&b, "Nothing running in worktree %s.", opts.Worktree)
		}
	} else {
		fmt.Fprintf(&b, "%s %d %s:", verb, len(reaped), scope)
		for _, e := range reaped {
			why := strings.Join(e.Problems, ",")
			if why != "" {
				why = " [" + why + "]"
			}
			fmt.Fprintf(&b, "\n  %s pid %d%s%s — %s", label(e), e.PID, portsSuffix(e), why, truncate(e.Command, 80))
		}
	}
	if len(leases) > 0 {
		lv := "Released"
		if a.DryRun {
			lv = "Would release"
		}
		fmt.Fprintf(&b, "\n%s %d port lease(s):", lv, len(leases))
		for _, l := range leases {
			fmt.Fprintf(&b, "\n  %d %s (%s)", l.Port, l.Service, l.Worktree)
		}
	}
	if len(errs) > 0 {
		b.WriteString("\nErrors:")
		for _, err := range errs {
			fmt.Fprintf(&b, "\n  %v", err)
		}
		r := textResult("%s", b.String())
		r.IsError = len(reaped) == 0
		return r
	}
	return textResult("%s", b.String())
}
