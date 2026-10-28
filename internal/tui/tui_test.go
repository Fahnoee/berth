package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/Fahnoee/berth/internal/scan"
)

func TestHumanAge(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second:          "0s",
		45 * time.Second:      "45s",
		3 * time.Minute:       "3m",
		119 * time.Minute:     "1h",
		26 * time.Hour:        "1d",
		4*24*time.Hour + 3600: "4d",
	} {
		if got := humanAge(d); got != want {
			t.Errorf("humanAge(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestShortenPath(t *testing.T) {
	for _, c := range []struct{ p, home, want string }{
		{"/home/u/code/app", "/home/u", "~/code/app"},
		{"/home/u", "/home/u", "~"},
		{"/home/user2/x", "/home/u", "/home/user2/x"},
		{"/srv/app", "", "/srv/app"},
	} {
		if got := shortenPath(c.p, c.home); got != c.want {
			t.Errorf("shortenPath(%q, %q) = %q, want %q", c.p, c.home, got, c.want)
		}
	}
}

func TestTruncateLeft(t *testing.T) {
	if got := truncateLeft("~/code/very/long/path", 10); got != "…long/path" {
		t.Errorf("got %q", got)
	}
	if got := truncateLeft("short", 10); got != "short" {
		t.Errorf("got %q", got)
	}
}

func TestFormatPorts(t *testing.T) {
	if got := formatPorts([]int{3000, 20341}); got != ":3000,:20341" {
		t.Errorf("got %q", got)
	}
}

func TestTail(t *testing.T) {
	if got := tail("a\nb\nc\n", 2); got != "b\nc" {
		t.Errorf("got %q", got)
	}
}

func fakeEntries(now time.Time) []scan.Entry {
	return []scan.Entry{
		{Name: "python3", PID: 900, Ports: []int{8000}, Command: "python3 -m http.server", Started: now.Add(-4 * 24 * time.Hour)},
		{ID: "web-1a2b", Service: "web", PID: 101, Ports: []int{20341}, Command: "npm run dev -- --port 20341",
			Worktree: "/home/u/code/app", Branch: "main", Agent: "claude", AgentAlive: true, Tracked: true,
			Started: now.Add(-3 * time.Minute), Log: "/tmp/web.log"},
		{ID: "api-9f9f", Service: "api", PID: 202, Ports: []int{20342, 20343}, Command: "go run ./cmd/api",
			Worktree: "/home/u/code/app-feat", Branch: "feat/login", Agent: "codex", AgentPID: 55, Tracked: true,
			Started: now.Add(-2 * time.Hour), Problems: []string{scan.AgentGone, scan.Expired}},
		{Name: "vite", PID: 303, Ports: []int{5173}, Command: "node vite", Worktree: "/home/u/code/app",
			Branch: "main", Started: now.Add(-time.Hour), Problems: []string{scan.Detached}},
	}
}

func TestGroupEntries(t *testing.T) {
	es := fakeEntries(time.Now())
	gs := groupEntries(es, "", false)
	if len(gs) != 3 || gs[0].Worktree != "/home/u/code/app" || len(gs[0].Entries) != 2 || gs[2].Worktree != "" {
		t.Fatalf("unexpected groups: %+v", gs)
	}
	if gs := groupEntries(es, "", true); len(gs) != 1 || gs[0].Entries[0].ID != "api-9f9f" {
		t.Errorf("abandoned filter: %+v", gs)
	}
	if gs := groupEntries(es, "LOGIN", false); len(gs) != 1 || gs[0].Branch != "feat/login" {
		t.Errorf("branch filter: %+v", gs)
	}
	if gs := groupEntries(es, ":5173", false); len(gs) != 1 || gs[0].Entries[0].PID != 303 {
		t.Errorf("port filter: %+v", gs)
	}
}

func testModel(t *testing.T, width, height int) model {
	t.Helper()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	m := newModel()
	m.home = "/home/u"
	m.now = func() time.Time { return now }
	tm, _ := m.Update(tea.WindowSizeMsg{Width: width, Height: height})
	tm, _ = tm.Update(scanMsg{entries: fakeEntries(now)})
	return tm.(model)
}

func send(m model, keys ...string) model {
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		tm, _ := m.Update(msg)
		m = tm.(model)
	}
	return m
}

func TestView(t *testing.T) {
	for _, width := range []int{80, 200} {
		m := testModel(t, width, 24)
		out := ansi.Strip(m.View())
		for _, want := range []string{
			"⚓ berth", "4 processes", "5 ports", "1 abandoned",
			"~/code/app", "main", "~/code/app-feat", "feat/login", outsideGroup,
			"web", ":20341", "claude", "3m", "2h", "4d", "agent-gone", "detached", "ok", "SERVICE", "quit",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("width %d: view missing %q:\n%s", width, want, out)
			}
		}
		lines := strings.Split(out, "\n")
		if len(lines) != 24 {
			t.Errorf("width %d: got %d lines, want 24", width, len(lines))
		}
		for i, l := range lines {
			if w := ansi.StringWidth(l); w > width {
				t.Errorf("width %d: line %d is %d wide: %q", width, i, w, l)
			}
		}
		// outside-repo group must come last
		if strings.Index(out, outsideGroup) < strings.Index(out, "~/code/app-feat") {
			t.Errorf("outside group not last")
		}
	}
}

func TestInteractions(t *testing.T) {
	m := testModel(t, 100, 30)
	if e, _ := m.current(); e.ID != "web-1a2b" {
		t.Fatalf("initial selection %q", e.ID)
	}
	m = send(m, "j", "j")
	if e, _ := m.current(); e.ID != "api-9f9f" {
		t.Fatalf("after j j selection %q", e.ID)
	}

	m = send(m, "enter")
	if out := ansi.Strip(m.View()); !strings.Contains(out, "go run ./cmd/api") || !strings.Contains(out, "codex (pid 55) gone") {
		t.Errorf("details pane missing:\n%s", out)
	}

	m = send(m, "x")
	if out := ansi.Strip(m.View()); !strings.Contains(out, "stop api-9f9f (pid 202)? y/n") {
		t.Errorf("stop confirmation missing:\n%s", out)
	}
	m = send(m, "n")
	if m.confirm != confirmNone {
		t.Errorf("confirmation not cancelled")
	}

	m = send(m, "/", "v", "i", "t", "e", "enter")
	if len(m.visible) != 1 || m.visible[0].PID != 303 {
		t.Errorf("filter: %+v", m.visible)
	}
	m = send(m, "esc", "esc") // close details, then clear filter
	if len(m.visible) != 4 {
		t.Errorf("filter not cleared: %d visible", len(m.visible))
	}

	m = send(m, "a")
	if len(m.visible) != 1 {
		t.Errorf("abandoned toggle: %d visible", len(m.visible))
	}

	// Selection survives a rescan that reorders entries.
	m = send(m, "a", "G")
	sel, _ := m.current()
	es := fakeEntries(m.now())
	es[0], es[3] = es[3], es[0]
	tm, _ := m.Update(scanMsg{entries: es})
	if e, _ := tm.(model).current(); entryKey(e) != entryKey(sel) {
		t.Errorf("selection moved from %s to %s", entryKey(sel), entryKey(e))
	}
}

func TestScrollKeepsCursorVisible(t *testing.T) {
	now := time.Now()
	var es []scan.Entry
	for i := range 40 {
		es = append(es, scan.Entry{PID: 1000 + i, Name: "srv", Ports: []int{3000 + i}, Worktree: "/w", Started: now})
	}
	m := newModel()
	tm, _ := m.Update(tea.WindowSizeMsg{Width: 80, Height: 12})
	tm, _ = tm.Update(scanMsg{entries: es})
	m = send(tm.(model), "G")
	if out := ansi.Strip(m.View()); !strings.Contains(out, ":3039") {
		t.Errorf("last row not visible after G:\n%s", out)
	}
	m = send(m, "g")
	if out := ansi.Strip(m.View()); !strings.Contains(out, "/w") || !strings.Contains(out, ":3000") {
		t.Errorf("top not visible after g:\n%s", out)
	}
}
