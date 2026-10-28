// Package scan builds berth's picture of the world: every listening process
// and every process started through `berth run`, annotated with the worktree,
// branch and agent it belongs to and anything that looks abandoned.
package scan

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/Fahnoee/berth/internal/gitwt"
	"github.com/Fahnoee/berth/internal/proc"
	"github.com/Fahnoee/berth/internal/state"
)

// Problem tags explain why an entry looks abandoned.
const (
	WorktreeGone = "worktree-gone" // its directory or worktree was deleted
	AgentGone    = "agent-gone"    // the agent session that started it has exited
	Expired      = "expired"       // it outlived its --ttl
	Detached     = "detached"      // untracked, reparented to init, running inside a repo
)

// Entry is one row in `berth ls`.
type Entry struct {
	ID         string    `json:"id,omitempty"` // berth id for tracked procs
	Service    string    `json:"service,omitempty"`
	PID        int       `json:"pid"`
	PPID       int       `json:"ppid"`
	Ports      []int     `json:"ports"`
	Name       string    `json:"name"`
	Command    string    `json:"command"`
	Cwd        string    `json:"cwd"`
	Worktree   string    `json:"worktree,omitempty"`
	Branch     string    `json:"branch,omitempty"`
	Started    time.Time `json:"started"`
	Agent      string    `json:"agent,omitempty"`
	AgentPID   int       `json:"agent_pid,omitempty"`
	AgentAlive bool      `json:"agent_alive"`
	Tracked    bool      `json:"tracked"`
	Log        string    `json:"log,omitempty"`
	TTL        string    `json:"ttl,omitempty"`
	Problems   []string  `json:"problems,omitempty"`
}

// Has reports whether the entry carries a problem tag.
func (e Entry) Has(p string) bool { return slices.Contains(e.Problems, p) }

// Abandoned reports whether the entry looks safe to reap. Detached processes
// only count when includeDetached is set, since they may be intentional.
func (e Entry) Abandoned(includeDetached bool) bool {
	for _, p := range e.Problems {
		if p != Detached || includeDetached {
			return true
		}
	}
	return false
}

// Label is a short human name for the entry.
func (e Entry) Label() string {
	if e.Service != "" {
		return e.Service
	}
	return e.Name
}

// Options tune a scan.
type Options struct {
	AllUsers bool // include other users' processes (where visible)
}

// Scan inspects the system. It also prunes dead processes from the registry.
func Scan(opts Options) ([]Entry, error) {
	st, err := state.Load()
	if err != nil {
		return nil, err
	}
	listeners, err := proc.Listeners()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	uid := os.Getuid()
	self := os.Getpid()
	ignore := ignorePatterns()

	var entries []Entry
	byPGID := map[int]int{} // tracked pgid -> index in entries
	var dead []string

	for _, p := range st.Procs {
		if !proc.Alive(p.PID, p.StartTime) {
			dead = append(dead, p.ID)
			continue
		}
		info, err := proc.Get(p.PID)
		if err != nil {
			dead = append(dead, p.ID)
			continue
		}
		e := Entry{
			ID: p.ID, Service: p.Name, PID: p.PID, PPID: info.PPID,
			Name: info.Exe(), Command: strings.Join(p.Cmd, " "), Cwd: p.Dir,
			Worktree: p.Worktree, Branch: p.Branch, Started: p.Started,
			Agent: p.Agent, AgentPID: p.AgentPID, Tracked: true, Log: p.Log,
		}
		if p.TTL > 0 {
			e.TTL = time.Duration(p.TTL).String()
		}
		e.Ports = append(e.Ports, listeners[p.PID]...)
		delete(listeners, p.PID)
		if !gitwt.Exists(p.Worktree) || !dirExists(p.Dir) {
			e.Problems = append(e.Problems, WorktreeGone)
		}
		if p.AgentPID > 0 {
			e.AgentAlive = proc.Alive(p.AgentPID, p.AgentST)
			if !e.AgentAlive {
				e.Problems = append(e.Problems, AgentGone)
			}
		}
		if p.Expired(now) {
			e.Problems = append(e.Problems, Expired)
		}
		byPGID[p.PID] = len(entries)
		entries = append(entries, e)
	}

	if len(dead) > 0 {
		_ = state.Update(func(s *state.State) error {
			s.Procs = slices.DeleteFunc(s.Procs, func(p state.Proc) bool { return slices.Contains(dead, p.ID) })
			return nil
		})
	}

	for pid, ports := range listeners {
		if pid == self {
			continue
		}
		info, err := proc.Get(pid)
		if err != nil {
			continue
		}
		if !opts.AllUsers && info.UID != uid {
			continue
		}
		if ignored(ignore, info) {
			continue
		}
		// Children of a tracked process group fold into their parent row.
		if i, ok := trackedOwner(info, byPGID); ok {
			entries[i].Ports = append(entries[i].Ports, ports...)
			continue
		}
		e := Entry{
			PID: pid, PPID: info.PPID, Ports: ports, Name: info.Exe(),
			Command: info.Command(), Cwd: info.Cwd, Started: info.Started,
		}
		if info.Cwd != "" {
			wt := gitwt.Resolve(info.Cwd)
			if wt.IsRepo {
				e.Worktree, e.Branch = wt.Root, wt.Branch
			}
		}
		if info.CwdDeleted {
			e.Problems = append(e.Problems, WorktreeGone)
		}
		if a, name, ok := FindAgent(pid); ok {
			e.Agent, e.AgentPID, e.AgentAlive = name, a.PID, true
		} else if orphaned(info) && e.Worktree != "" && !info.CwdDeleted {
			e.Problems = append(e.Problems, Detached)
		}
		entries = append(entries, e)
	}

	for i := range entries {
		slices.Sort(entries[i].Ports)
		entries[i].Ports = slices.Compact(entries[i].Ports)
	}
	slices.SortFunc(entries, func(a, b Entry) int {
		if c := strings.Compare(a.Worktree, b.Worktree); c != 0 {
			return c
		}
		return firstPort(a) - firstPort(b)
	})
	return entries, nil
}

func trackedOwner(info proc.Info, byPGID map[int]int) (int, bool) {
	if i, ok := byPGID[info.PGID]; ok {
		return i, true
	}
	for _, a := range proc.Ancestors(info.PID) {
		if i, ok := byPGID[a.PID]; ok {
			return i, true
		}
	}
	return 0, false
}

// ignorePatterns reads BERTH_IGNORE: comma-separated executable names or
// globs (e.g. "postgres,docker-*") for untracked listeners to hide.
func ignorePatterns() []string {
	var out []string
	for _, n := range strings.Split(os.Getenv("BERTH_IGNORE"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

func ignored(patterns []string, info proc.Info) bool {
	for _, pat := range patterns {
		for _, name := range []string{info.Name, info.Exe()} {
			if ok, _ := path.Match(pat, name); ok {
				return true
			}
		}
	}
	return false
}

// orphaned reports whether a process was reparented to init or to a
// subreaper such as `systemd --user`, which adopts orphans on most desktops.
func orphaned(info proc.Info) bool {
	if info.PPID == 1 {
		return true
	}
	parent, err := proc.Get(info.PPID)
	if err != nil {
		return false
	}
	switch parent.Exe() {
	case "systemd", "init", "launchd":
		return true
	}
	return false
}

func firstPort(e Entry) int {
	if len(e.Ports) == 0 {
		return 1 << 20
	}
	return e.Ports[0]
}

func dirExists(d string) bool {
	if d == "" {
		return true
	}
	_, err := os.Stat(d)
	return err == nil
}

// Within reports whether path is dir or inside it.
func Within(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}
