package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/Fahnoee/berth/internal/scan"
)

var (
	colAccent   = lipgloss.Color("#2dd4bf")
	colAccentBg = lipgloss.Color("#0f766e")
	colSelBg    = lipgloss.Color("#134e4a")
	colGreen    = lipgloss.Color("#4ade80")
	colYellow   = lipgloss.Color("#facc15")
	colRed      = lipgloss.Color("#f87171")
	colDetached = lipgloss.Color("#a39150")
	colBranch   = lipgloss.Color("#c4b5fd")
	colDim      = lipgloss.Color("#6b7280")
	colText     = lipgloss.Color("#e5e7eb")
)

var styles = struct {
	badge, accent, branch, dim, header, ok, warn, problem, detached, selected, key, label lipgloss.Style
}{
	badge:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#f0fdfa")).Background(colAccentBg).Padding(0, 1),
	accent:   lipgloss.NewStyle().Foreground(colAccent),
	branch:   lipgloss.NewStyle().Foreground(colBranch),
	dim:      lipgloss.NewStyle().Foreground(colDim),
	header:   lipgloss.NewStyle().Foreground(colDim).Bold(true),
	ok:       lipgloss.NewStyle().Foreground(colGreen),
	warn:     lipgloss.NewStyle().Foreground(colYellow),
	problem:  lipgloss.NewStyle().Foreground(colRed),
	detached: lipgloss.NewStyle().Foreground(colDetached),
	selected: lipgloss.NewStyle().Foreground(colText).Background(colSelBg).Bold(true),
	key:      lipgloss.NewStyle().Foreground(colAccent).Bold(true),
	label:    lipgloss.NewStyle().Foreground(colDim).Width(10),
}

// columns are the table cell widths; command takes what is left.
type columns struct {
	label, pid, ports, agent, age, status, cmd, gap int
}

const indent = 2

func (m model) View() string {
	if m.logOpen {
		return m.logScreen()
	}
	var b strings.Builder
	b.WriteString(m.titleBar())
	b.WriteByte('\n')

	cols := m.columns()
	b.WriteString(m.columnHeader(cols))
	b.WriteByte('\n')

	body := m.body(cols)
	h := m.bodyHeight()
	for len(body) < h {
		body = append(body, "")
	}
	b.WriteString(strings.Join(body[:h], "\n"))
	b.WriteByte('\n')

	if m.details {
		b.WriteString(m.detailsPane())
		b.WriteByte('\n')
	}
	b.WriteString(m.footer())
	return b.String()
}

func (m model) titleBar() string {
	ports, abandoned := 0, 0
	for _, e := range m.entries {
		ports += len(e.Ports)
		if e.Abandoned(false) {
			abandoned++
		}
	}
	sep := styles.dim.Render(" · ")
	left := styles.badge.Render("⚓ berth") + " " +
		fmt.Sprintf("%d %s", len(m.entries), plural(len(m.entries), "process", "processes")) + sep +
		fmt.Sprintf("%d %s", ports, plural(ports, "port", "ports")) + sep
	ab := fmt.Sprintf("%d abandoned", abandoned)
	if abandoned > 0 {
		ab = styles.problem.Render(ab)
	} else {
		ab = styles.dim.Render(ab)
	}
	left += ab

	var right []string
	if m.err != nil {
		right = append(right, styles.problem.Render("scan failed: "+m.err.Error()))
	}
	if m.onlyAbandoned {
		right = append(right, styles.warn.Render("abandoned only"))
	}
	if q := m.filter.Value(); q != "" && !m.filtering {
		right = append(right, styles.accent.Render("filter: "+q))
	}
	r := strings.Join(right, sep)
	space := m.width - lipgloss.Width(left) - lipgloss.Width(r) - 1
	if r == "" || space < 1 {
		return truncate(left, m.width)
	}
	return left + strings.Repeat(" ", space) + r
}

func (m model) columns() columns {
	c := columns{label: 7, pid: 3, ports: 5, agent: 5, age: 3, status: 6, gap: 2}
	for _, e := range m.visible {
		c.label = max(c.label, ansi.StringWidth(e.Label()))
		c.pid = max(c.pid, len(strconv.Itoa(e.PID)))
		c.ports = max(c.ports, len(formatPorts(e.Ports)))
		c.agent = max(c.agent, ansi.StringWidth(agentText(e)))
		c.age = max(c.age, len(m.age(e)))
		c.status = max(c.status, len(statusText(e)))
	}
	c.label, c.ports, c.agent, c.status = min(c.label, 24), min(c.ports, 24), min(c.agent, 14), min(c.status, 28)
	if m.width < 120 {
		c.gap = 1
	}
	// Shrink the widest flexible column until the command gets a fair share.
	minCmd := min(40, m.width/4)
	flex := []*int{&c.label, &c.ports, &c.status, &c.agent}
	mins := []int{8, 6, 7, 5}
	for c.cmd = m.width - c.fixed(); c.cmd < minCmd; c.cmd = m.width - c.fixed() {
		best := -1
		for i, p := range flex {
			if *p > mins[i] && (best < 0 || *p > *flex[best]) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		*flex[best]--
	}
	c.cmd = max(c.cmd, 0)
	return c
}

func (c columns) fixed() int {
	return indent + c.label + c.pid + c.ports + c.agent + c.age + c.status + 6*c.gap
}

func (m model) columnHeader(c columns) string {
	g := strings.Repeat(" ", c.gap)
	line := strings.Repeat(" ", indent) + pad("SERVICE", c.label) + g + padLeft("PID", c.pid) + g +
		pad("PORTS", c.ports) + g + pad("AGENT", c.agent) + g + padLeft("AGE", c.age) + g +
		pad("STATUS", c.status) + g + "COMMAND"
	return styles.header.Render(truncate(line, m.width))
}

func (m model) body(c columns) []string {
	if len(m.groups) == 0 {
		msg := "scanning…"
		switch {
		case m.scanned && len(m.entries) == 0:
			msg = "no dev servers running — start one with `berth run -- <cmd>`"
		case m.scanned:
			msg = "nothing matches"
			if m.onlyAbandoned && m.filter.Value() == "" {
				msg = "nothing abandoned"
			}
		}
		return []string{"", strings.Repeat(" ", indent) + styles.dim.Render(truncate(msg, m.width-indent))}
	}
	var lines []string
	i := 0
	for _, g := range m.groups {
		lines = append(lines, m.groupHeader(g))
		for _, e := range g.Entries {
			lines = append(lines, m.row(e, c, i == m.cursor))
			i++
		}
	}
	end := min(m.offset+m.bodyHeight(), len(lines))
	return lines[min(m.offset, end):end]
}

func (m model) groupHeader(g group) string {
	count := styles.dim.Render(fmt.Sprintf("  %d", len(g.Entries)))
	if g.Worktree == "" {
		return styles.dim.Bold(true).Render(outsideGroup) + count
	}
	branch := ""
	if g.Branch != "" {
		branch = "  " + styles.branch.Render(g.Branch)
	}
	room := m.width - lipgloss.Width(branch) - lipgloss.Width(count)
	path := truncateLeft(shortenPath(g.Worktree, m.home), max(room, 10))
	return truncate(styles.accent.Bold(true).Render(path)+branch+count, m.width)
}

func (m model) row(e scan.Entry, c columns, selected bool) string {
	g := strings.Repeat(" ", c.gap)
	label, pid := pad(e.Label(), c.label), padLeft(strconv.Itoa(e.PID), c.pid)
	ports, age := pad(formatPorts(e.Ports), c.ports), padLeft(m.age(e), c.age)
	agent := pad(agentText(e), c.agent)
	status := pad(statusText(e), c.status)
	cmd := truncate(e.Command, c.cmd)

	if selected {
		line := styles.accent.Background(colSelBg).Render("▌ ") +
			styles.selected.Render(pad(label+g+pid+g+ports+g+agent+g+age+g+status+g+cmd, max(m.width-indent, 0)))
		return line
	}

	if e.Agent != "" && !e.AgentAlive {
		agent = strings.Replace(agent, "×", styles.dim.Render("×"), 1)
	} else if e.Agent == "" {
		agent = styles.dim.Render(agent)
	}
	line := strings.Repeat(" ", indent) + label + g + styles.dim.Render(pid) + g + styles.accent.Render(ports) + g +
		agent + g + styles.dim.Render(age) + g + statusStyle(e).Render(status) + g + styles.dim.Render(cmd)
	return line
}

func (m model) age(e scan.Entry) string {
	if e.Started.IsZero() {
		return "-"
	}
	return humanAge(m.now().Sub(e.Started))
}

func agentText(e scan.Entry) string {
	switch {
	case e.Agent == "":
		return "-"
	case !e.AgentAlive:
		return e.Agent + " ×"
	}
	return e.Agent
}

func statusText(e scan.Entry) string {
	if len(e.Problems) == 0 {
		return "ok"
	}
	return strings.Join(e.Problems, " ")
}

// statusStyle colours a status by its most severe problem.
func statusStyle(e scan.Entry) lipgloss.Style {
	switch {
	case len(e.Problems) == 0:
		return styles.ok
	case e.Has(scan.WorktreeGone), e.Has(scan.AgentGone):
		return styles.problem
	case e.Has(scan.Expired):
		return styles.warn
	}
	return styles.detached
}

func (m model) detailsPane() string {
	e, ok := m.current()
	rule := styles.dim.Render(strings.Repeat("─", max(m.width, 0)))
	lines := []string{rule}
	if !ok {
		lines = append(lines, styles.dim.Render("  nothing selected"))
	} else {
		w := max(m.width-12, 10)
		cmd := ansi.Hardwrap(e.Command, w, true)
		cmdLines := strings.SplitN(cmd, "\n", 3)
		if len(cmdLines) > 2 {
			cmdLines = cmdLines[:2]
			cmdLines[1] = truncate(cmdLines[1]+"…", w)
		}
		field := func(name, value string) string {
			return "  " + styles.label.Render(name) + truncate(value, w)
		}
		lines = append(lines, field("command", cmdLines[0]))
		if len(cmdLines) > 1 {
			lines = append(lines, "  "+styles.label.Render("")+cmdLines[1])
		}
		started := "-"
		if !e.Started.IsZero() {
			started = e.Started.Format("2006-01-02 15:04:05") + styles.dim.Render(" ("+m.age(e)+" ago)")
		}
		agent := "-"
		if e.Agent != "" {
			agent = fmt.Sprintf("%s (pid %d)", e.Agent, e.AgentPID)
			if !e.AgentAlive {
				agent += styles.problem.Render(" gone")
			}
		}
		id := ""
		if e.ID != "" {
			id = styles.dim.Render("  id " + e.ID)
		}
		lines = append(lines,
			field("cwd", shortenPath(e.Cwd, m.home)),
			field("pid", fmt.Sprintf("%d  ppid %d", e.PID, e.PPID)+id),
			field("started", started),
			field("agent", agent),
			field("log", orDash(shortenPath(e.Log, m.home))),
			field("ttl", orDash(e.TTL)),
			field("problems", statusStyle(e).Render(statusText(e))),
		)
	}
	for len(lines) < detailsHeight {
		lines = append(lines, "")
	}
	return strings.Join(lines[:detailsHeight], "\n")
}

func (m model) footer() string {
	var s string
	switch {
	case m.confirm == confirmStop:
		s = styles.warn.Render(fmt.Sprintf("stop %s (pid %d)?", displayName(m.target), m.target.PID)) + " " + yn()
	case m.confirm == confirmReap:
		s = styles.warn.Render(fmt.Sprintf("reap %d abandoned %s?", m.reapTargets, plural(m.reapTargets, "process", "processes"))) + " " + yn()
	case m.filtering:
		s = m.filter.View() + styles.dim.Render("   enter apply · esc clear")
	case m.status != "":
		st := styles.ok
		if m.statusErr {
			st = styles.problem
		}
		s = st.Render(m.status)
	default:
		s = help([][2]string{
			{"q", "quit"}, {"↑↓", "move"}, {"x", "stop"}, {"R", "reap"}, {"a", "abandoned"}, {"/", "filter"},
			{"l", "logs"}, {"o", "open"}, {"enter", "details"},
		})
	}
	return truncate(s, m.width)
}

func (m model) logScreen() string {
	e := m.logEntry
	title := m.titleBar()
	info := styles.accent.Bold(true).Render("logs ") + displayName(e) + "  " + styles.dim.Render(shortenPath(e.Log, m.home))
	pct := styles.dim.Render(fmt.Sprintf("%3.0f%%", m.logView.ScrollPercent()*100))
	space := m.width - lipgloss.Width(info) - lipgloss.Width(pct)
	if space > 0 {
		info += strings.Repeat(" ", space) + pct
	}
	foot := help([][2]string{{"↑↓", "scroll"}, {"pgup/pgdn", "page"}, {"esc", "close"}})
	return title + "\n" + truncate(info, m.width) + "\n" + m.logView.View() + "\n" + truncate(foot, m.width)
}

func help(keys [][2]string) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = styles.key.Render(k[0]) + " " + styles.dim.Render(k[1])
	}
	return strings.Join(parts, "  ")
}

func yn() string {
	return styles.key.Render("y") + styles.dim.Render("/") + styles.key.Render("n")
}

func padLeft(s string, w int) string {
	s = truncate(s, w)
	return strings.Repeat(" ", max(w-ansi.StringWidth(s), 0)) + s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
