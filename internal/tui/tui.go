// Package tui is berth's live full-screen dashboard of agent-spawned servers.
package tui

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/Fahnoee/berth/internal/scan"
)

const (
	pollInterval  = 2 * time.Second
	statusTTL     = 4 * time.Second
	stopGrace     = 5 * time.Second
	logLines      = 200
	logReadLimit  = 256 << 10
	detailsHeight = 10
)

// Run starts the dashboard and blocks until the user quits.
func Run() error {
	_, err := tea.NewProgram(newModel(), tea.WithAltScreen()).Run()
	return err
}

type confirmKind int

const (
	confirmNone confirmKind = iota
	confirmStop
	confirmReap
)

type (
	tickMsg time.Time
	scanMsg struct {
		entries []scan.Entry
		err     error
	}
	stopMsg struct {
		label string
		err   error
	}
	reapMsg struct {
		reaped int
		errs   []error
	}
	logMsg struct {
		path    string
		content string
		err     error
	}
	clearStatusMsg struct{ id int }
)

type model struct {
	entries  []scan.Entry
	scanned  bool
	scanning bool
	err      error

	width, height int
	home          string
	now           func() time.Time

	groups   []group
	visible  []scan.Entry // visible entries in display order
	cursor   int
	selected string // key of the selected entry, kept across scans
	offset   int    // first visible body line

	onlyAbandoned bool
	filter        textinput.Model
	filtering     bool
	details       bool

	confirm     confirmKind
	target      scan.Entry
	reapTargets int

	status    string
	statusErr bool
	statusID  int

	logOpen  bool
	logEntry scan.Entry
	logView  viewport.Model
}

func newModel() model {
	ti := textinput.New()
	ti.Prompt = "/"
	ti.Placeholder = "filter"
	ti.CharLimit = 128
	home, _ := os.UserHomeDir()
	return model{
		filter:  ti,
		home:    home,
		now:     time.Now,
		width:   80,
		height:  24,
		logView: viewport.New(80, 20),
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(scanCmd(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(pollInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func scanCmd() tea.Cmd {
	return func() tea.Msg {
		entries, err := scan.Scan(scan.Options{})
		return scanMsg{entries, err}
	}
}

func stopCmd(e scan.Entry) tea.Cmd {
	return func() tea.Msg {
		return stopMsg{label: displayName(e), err: scan.Stop(e, stopGrace)}
	}
}

func reapCmd(entries []scan.Entry) tea.Cmd {
	return func() tea.Msg {
		reaped, _, errs := scan.Reap(entries, scan.ReapOptions{})
		return reapMsg{len(reaped), errs}
	}
}

func readLogCmd(path string) tea.Cmd {
	return func() tea.Msg {
		content, err := readTail(path)
		return logMsg{path, content, err}
	}
}

func openCmd(url string) tea.Cmd {
	return func() tea.Msg {
		bin := "xdg-open"
		if runtime.GOOS == "darwin" {
			bin = "open"
		}
		cmd := exec.Command(bin, url)
		if cmd.Start() == nil {
			go func() { _ = cmd.Wait() }()
		}
		return nil
	}
}

// readTail reads the last logLines lines of a file, looking only at its end.
func readTail(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > logReadLimit {
		if _, err := f.Seek(-logReadLimit, io.SeekEnd); err != nil {
			return "", err
		}
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return tail(string(b), logLines), nil
}

// displayName is the label plus the berth id or pid, e.g. "web-1a2b".
func displayName(e scan.Entry) string {
	if e.ID != "" {
		return e.ID
	}
	return fmt.Sprintf("%s (pid %d)", e.Label(), e.PID)
}

func (m *model) setStatus(msg string, isErr bool) tea.Cmd {
	m.statusID++
	m.status, m.statusErr = msg, isErr
	id := m.statusID
	return tea.Tick(statusTTL, func(time.Time) tea.Msg { return clearStatusMsg{id} })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	m.layout()
	return m, cmd
}

func (m *model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return nil

	case tickMsg:
		cmds := []tea.Cmd{tick()}
		if !m.scanning {
			m.scanning = true
			cmds = append(cmds, scanCmd())
		}
		if m.logOpen {
			cmds = append(cmds, readLogCmd(m.logEntry.Log))
		}
		return tea.Batch(cmds...)

	case scanMsg:
		m.scanning, m.scanned = false, true
		m.err = msg.err
		if msg.err == nil {
			m.entries = msg.entries
		}
		return nil

	case stopMsg:
		m.scanning = true
		if msg.err != nil {
			return tea.Batch(scanCmd(), m.setStatus(fmt.Sprintf("stop %s: %v", msg.label, msg.err), true))
		}
		return tea.Batch(scanCmd(), m.setStatus("stopped "+msg.label, false))

	case reapMsg:
		m.scanning = true
		text := fmt.Sprintf("reaped %d %s", msg.reaped, plural(msg.reaped, "process", "processes"))
		if len(msg.errs) > 0 {
			text += fmt.Sprintf(", %d failed: %v", len(msg.errs), msg.errs[0])
		}
		return tea.Batch(scanCmd(), m.setStatus(text, len(msg.errs) > 0))

	case logMsg:
		if !m.logOpen || msg.path != m.logEntry.Log {
			return nil
		}
		if msg.err != nil {
			m.logView.SetContent(styles.problem.Render(msg.err.Error()))
			return nil
		}
		atBottom := m.logView.AtBottom() || m.logView.TotalLineCount() == 0
		m.logView.SetContent(msg.content)
		if atBottom {
			m.logView.GotoBottom()
		}
		return nil

	case clearStatusMsg:
		if msg.id == m.statusID {
			m.status = ""
		}
		return nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	if m.logOpen {
		var cmd tea.Cmd
		m.logView, cmd = m.logView.Update(msg)
		return cmd
	}
	return nil
}

func (m *model) handleKey(msg tea.KeyMsg) tea.Cmd {
	key := msg.String()
	if key == "ctrl+c" {
		return tea.Quit
	}

	if m.confirm != confirmNone {
		kind := m.confirm
		m.confirm = confirmNone
		if key != "y" && key != "Y" {
			return nil
		}
		if kind == confirmStop {
			return tea.Batch(stopCmd(m.target), m.setStatus("stopping "+displayName(m.target)+"…", false))
		}
		return tea.Batch(reapCmd(m.entries), m.setStatus("reaping…", false))
	}

	if m.logOpen {
		switch key {
		case "esc", "q", "l":
			m.logOpen = false
			return nil
		}
		var cmd tea.Cmd
		m.logView, cmd = m.logView.Update(msg)
		return cmd
	}

	if m.filtering {
		switch key {
		case "esc":
			m.filtering = false
			m.filter.SetValue("")
			m.filter.Blur()
			return nil
		case "enter", "up", "down":
			m.filtering = false
			m.filter.Blur()
			if key == "enter" {
				return nil
			}
		default:
			var cmd tea.Cmd
			m.filter, cmd = m.filter.Update(msg)
			m.cursor = 0
			m.selected = ""
			return cmd
		}
	}

	sel, hasSel := m.current()
	switch key {
	case "q":
		return tea.Quit
	case "up", "k":
		m.move(-1)
	case "down", "j":
		m.move(1)
	case "pgup":
		m.move(-max(1, m.bodyHeight()/2))
	case "pgdown":
		m.move(max(1, m.bodyHeight()/2))
	case "home", "g":
		m.move(-len(m.visible))
	case "end", "G":
		m.move(len(m.visible))
	case "/":
		m.filtering = true
		return m.filter.Focus()
	case "esc":
		if m.details {
			m.details = false
		} else {
			m.filter.SetValue("")
		}
	case "a":
		m.onlyAbandoned = !m.onlyAbandoned
		m.cursor = 0
		m.selected = ""
	case "enter":
		m.details = !m.details
	case "x":
		if hasSel {
			m.confirm, m.target = confirmStop, sel
		}
	case "R":
		now := m.now()
		n := 0
		for _, e := range m.entries {
			if (scan.ReapOptions{}).Selected(e, now) {
				n++
			}
		}
		if n == 0 {
			return m.setStatus("nothing to reap", false)
		}
		m.confirm, m.reapTargets = confirmReap, n
	case "o":
		if !hasSel || len(sel.Ports) == 0 {
			return m.setStatus("no port to open", true)
		}
		url := "http://localhost:" + strconv.Itoa(sel.Ports[0])
		return tea.Batch(openCmd(url), m.setStatus("opening "+url, false))
	case "l":
		if !hasSel {
			return nil
		}
		if sel.Log == "" {
			return m.setStatus("no log for "+sel.Label()+" (only tracked processes have logs)", true)
		}
		m.logOpen, m.logEntry = true, sel
		m.logView.SetContent("")
		return readLogCmd(sel.Log)
	}
	return nil
}

func (m model) current() (scan.Entry, bool) {
	if m.cursor < 0 || m.cursor >= len(m.visible) {
		return scan.Entry{}, false
	}
	return m.visible[m.cursor], true
}

func (m *model) move(delta int) {
	if len(m.visible) == 0 {
		return
	}
	m.cursor = min(max(m.cursor+delta, 0), len(m.visible)-1)
	m.selected = entryKey(m.visible[m.cursor])
}

// layout recomputes the visible rows, restores the selection and keeps the
// cursor in view.
func (m *model) layout() {
	m.groups = groupEntries(m.entries, m.filter.Value(), m.onlyAbandoned)
	m.visible = nil
	for _, g := range m.groups {
		m.visible = append(m.visible, g.Entries...)
	}
	if m.selected != "" {
		for i, e := range m.visible {
			if entryKey(e) == m.selected {
				m.cursor = i
				break
			}
		}
	}
	m.cursor = min(max(m.cursor, 0), max(len(m.visible)-1, 0))
	if e, ok := m.current(); ok {
		m.selected = entryKey(e)
	}

	m.logView.Width = m.width
	m.logView.Height = max(m.height-3, 1)

	line := m.cursorLine()
	h := m.bodyHeight()
	if line-1 < m.offset { // keep the group header visible too
		m.offset = max(line-1, 0)
	}
	if line >= m.offset+h {
		m.offset = line - h + 1
	}
	m.offset = max(min(m.offset, m.bodyLines()-h), 0)
}

// cursorLine is the body line index of the selected row.
func (m *model) cursorLine() int {
	line, n := 0, 0
	for _, g := range m.groups {
		line++ // group header
		if m.cursor < n+len(g.Entries) {
			return line + m.cursor - n
		}
		line += len(g.Entries)
		n += len(g.Entries)
	}
	return 0
}

func (m *model) bodyLines() int {
	n := 0
	for _, g := range m.groups {
		n += 1 + len(g.Entries)
	}
	return n
}

// bodyHeight is the number of table lines that fit between the chrome.
func (m *model) bodyHeight() int {
	h := m.height - 3 // title bar, column header, footer
	if m.details {
		h -= detailsHeight
	}
	return max(h, 1)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
