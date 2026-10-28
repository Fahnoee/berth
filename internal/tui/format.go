package tui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/Fahnoee/berth/internal/scan"
)

// outsideGroup names the group for entries that are not in any repo.
const outsideGroup = "outside any repo"

// group is a worktree and the visible entries running in it.
type group struct {
	Worktree string
	Branch   string
	Entries  []scan.Entry
}

// humanAge renders d as a compact age like 45s, 3m, 2h or 4d.
func humanAge(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// shortenPath replaces a leading home directory with ~.
func shortenPath(p, home string) string {
	if home == "" || p == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rel, ok := strings.CutPrefix(p, strings.TrimSuffix(home, "/")+"/"); ok {
		return filepath.Join("~", rel)
	}
	return p
}

// formatPorts renders ports as ":3000,:3001".
func formatPorts(ports []int) string {
	parts := make([]string, len(ports))
	for i, p := range ports {
		parts[i] = ":" + strconv.Itoa(p)
	}
	return strings.Join(parts, ",")
}

// matches reports whether e matches a case-insensitive filter query.
func matches(e scan.Entry, query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return true
	}
	for _, s := range []string{e.Label(), e.Command, e.Worktree, e.Branch, formatPorts(e.Ports)} {
		if strings.Contains(strings.ToLower(s), q) {
			return true
		}
	}
	return false
}

// groupEntries filters entries and groups them by worktree, preserving order
// of first appearance, with entries outside any repo last.
func groupEntries(entries []scan.Entry, query string, onlyAbandoned bool) []group {
	var groups []group
	var outside []scan.Entry
	idx := map[string]int{}
	for _, e := range entries {
		if onlyAbandoned && !e.Abandoned(false) || !matches(e, query) {
			continue
		}
		if e.Worktree == "" {
			outside = append(outside, e)
			continue
		}
		i, ok := idx[e.Worktree]
		if !ok {
			i = len(groups)
			idx[e.Worktree] = i
			groups = append(groups, group{Worktree: e.Worktree, Branch: e.Branch})
		}
		if groups[i].Branch == "" {
			groups[i].Branch = e.Branch
		}
		groups[i].Entries = append(groups[i].Entries, e)
	}
	if len(outside) > 0 {
		groups = append(groups, group{Entries: outside})
	}
	return groups
}

// entryKey identifies an entry across scans.
func entryKey(e scan.Entry) string {
	if e.ID != "" {
		return e.ID
	}
	return strconv.Itoa(e.PID)
}

// truncate shortens s to w cells, ending in an ellipsis when cut.
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	return ansi.Truncate(s, w, "…")
}

// truncateLeft shortens s to w cells by cutting from the front.
func truncateLeft(s string, w int) string {
	if ansi.StringWidth(s) <= w {
		return s
	}
	if w <= 1 {
		return truncate("…", w)
	}
	return "…" + ansi.TruncateLeft(s, ansi.StringWidth(s)-w+1, "")
}

// pad truncates or right-pads s to exactly w cells.
func pad(s string, w int) string {
	s = truncate(s, w)
	if n := ansi.StringWidth(s); n < w {
		s += strings.Repeat(" ", w-n)
	}
	return s
}

// tail returns the last n lines of s.
func tail(s string, n int) string {
	s = strings.TrimRight(s, "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
