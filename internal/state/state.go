// Package state persists berth's port leases and tracked processes in a single
// JSON file guarded by an advisory file lock, so concurrent agents in
// different worktrees never hand out the same port.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Lease reserves a port for a (worktree, service) pair.
type Lease struct {
	Port     int       `json:"port"`
	Worktree string    `json:"worktree"`
	Service  string    `json:"service"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used"`
}

// Proc is a process started through `berth run`.
type Proc struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	PID       int       `json:"pid"`
	StartTime int64     `json:"start_time"` // process start time (ticks/epoch) to guard against PID reuse
	Cmd       []string  `json:"cmd"`
	Dir       string    `json:"dir"`
	Worktree  string    `json:"worktree"`
	Branch    string    `json:"branch,omitempty"`
	Port      int       `json:"port,omitempty"`
	Started   time.Time `json:"started"`
	TTL       Duration  `json:"ttl,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	AgentPID  int       `json:"agent_pid,omitempty"`
	AgentST   int64     `json:"agent_start_time,omitempty"`
	Log       string    `json:"log,omitempty"`
	Detached  bool      `json:"detached,omitempty"`
}

// Expired reports whether the proc has outlived its TTL.
func (p Proc) Expired(now time.Time) bool {
	return p.TTL > 0 && now.Sub(p.Started) > time.Duration(p.TTL)
}

// State is the whole on-disk database.
type State struct {
	Leases []Lease `json:"leases"`
	Procs  []Proc  `json:"procs"`
}

// Duration marshals as a Go duration string.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	*d = Duration(v)
	return err
}

// Dir returns the state directory: $BERTH_HOME, else $XDG_STATE_HOME/berth,
// else ~/.local/state/berth.
func Dir() string {
	if d := os.Getenv("BERTH_HOME"); d != "" {
		return d
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "berth")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "berth")
}

// LogDir is where detached processes write their output.
func LogDir() string { return filepath.Join(Dir(), "logs") }

func dbPath() string { return filepath.Join(Dir(), "state.json") }

func lock(exclusive bool) (*os.File, error) {
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(Dir(), "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		return nil, fmt.Errorf("lock state: %w", err)
	}
	return f, nil
}

func read() (*State, error) {
	s := &State{}
	b, err := os.ReadFile(dbPath())
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", dbPath(), err)
	}
	return s, nil
}

// Load returns a snapshot of the state.
func Load() (*State, error) {
	l, err := lock(false)
	if err != nil {
		return nil, err
	}
	defer l.Close()
	return read()
}

// Update runs fn against the state under an exclusive lock and saves the
// result atomically if fn returns nil.
func Update(fn func(*State) error) error {
	l, err := lock(true)
	if err != nil {
		return err
	}
	defer l.Close()
	s, err := read()
	if err != nil {
		return err
	}
	if err := fn(s); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := dbPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dbPath())
}
