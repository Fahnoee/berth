// Package runner starts processes on behalf of `berth run`: it leases a port,
// injects it into the environment, records the process in the registry and
// optionally detaches it with its output going to a log file.
package runner

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Fahnoee/berth/internal/gitwt"
	"github.com/Fahnoee/berth/internal/ports"
	"github.com/Fahnoee/berth/internal/proc"
	"github.com/Fahnoee/berth/internal/scan"
	"github.com/Fahnoee/berth/internal/state"
)

// Spec describes what to run.
type Spec struct {
	Name    string        // service name; also the port lease key
	Cmd     []string      // argv; "{port}" is substituted
	Dir     string        // working directory (default: cwd)
	PortEnv string        // env var to receive the port (default PORT)
	NoPort  bool          // don't lease a port
	Detach  bool          // run in the background, log to file
	Replace bool          // stop an existing instance first
	TTL     time.Duration // reap after this long (0 = never)
}

// ErrAlreadyRunning is returned (with the existing proc) when the service is
// already running in this worktree and Replace is false.
var ErrAlreadyRunning = errors.New("already running")

// Handle is a started (or already running) process.
type Handle struct {
	Proc state.Proc
	Cmd  *exec.Cmd // nil when detached or already running
}

// URL is a convenience http URL for the leased port.
func (h Handle) URL() string {
	if h.Proc.Port == 0 {
		return ""
	}
	return fmt.Sprintf("http://localhost:%d", h.Proc.Port)
}

// DefaultName derives a service name from a command line.
func DefaultName(cmd []string) string {
	if len(cmd) == 0 {
		return "app"
	}
	base := filepath.Base(cmd[0])
	switch base {
	case "npm", "pnpm", "yarn", "bun", "deno", "make", "just", "task", "cargo", "go", "uv", "poetry", "mix":
		for _, a := range cmd[1:] {
			if a == "run" || strings.HasPrefix(a, "-") {
				continue
			}
			return base + "-" + a
		}
	}
	return base
}

// Start launches spec.
func Start(spec Spec) (Handle, error) {
	if len(spec.Cmd) == 0 {
		return Handle{}, errors.New("no command given")
	}
	if spec.Dir == "" {
		spec.Dir, _ = os.Getwd()
	}
	if abs, err := filepath.Abs(spec.Dir); err == nil {
		spec.Dir = abs
	}
	if real, err := filepath.EvalSymlinks(spec.Dir); err == nil {
		spec.Dir = real
	}
	if spec.Name == "" {
		spec.Name = DefaultName(spec.Cmd)
	}
	if spec.PortEnv == "" {
		spec.PortEnv = "PORT"
	}
	wt := gitwt.Resolve(spec.Dir)

	if existing, ok := running(wt.Root, spec.Name); ok {
		if !spec.Replace {
			return Handle{Proc: existing}, ErrAlreadyRunning
		}
		if err := proc.Terminate(existing.PID, 5*time.Second); err != nil {
			return Handle{}, fmt.Errorf("stop previous %s: %w", existing.ID, err)
		}
		forget(existing.ID)
	}

	p := state.Proc{
		ID: newID(spec.Name), Name: spec.Name, Dir: spec.Dir, Worktree: wt.Root,
		Branch: wt.Branch, Started: time.Now(), TTL: state.Duration(spec.TTL), Detached: spec.Detach,
	}
	env := os.Environ()
	if !spec.NoPort {
		port, _, err := ports.Lease(wt.Root, spec.Name)
		if err != nil {
			return Handle{}, err
		}
		p.Port = port
		ps := strconv.Itoa(port)
		env = append(env, spec.PortEnv+"="+ps, "BERTH_PORT="+ps)
	}
	env = append(env, "BERTH_SERVICE="+spec.Name, "BERTH_WORKTREE="+wt.Root, "BERTH_ID="+p.ID)
	p.Cmd = substitute(spec.Cmd, p.Port)

	if a, name, ok := scan.FindAgent(os.Getpid()); ok {
		p.Agent, p.AgentPID, p.AgentST = name, a.PID, a.StartTime
	}

	path, err := exec.LookPath(p.Cmd[0])
	if err != nil {
		return Handle{}, err
	}
	cmd := exec.Command(path, p.Cmd[1:]...)
	cmd.Args[0] = p.Cmd[0]
	cmd.Dir = spec.Dir
	cmd.Env = env

	if spec.Detach {
		if err := os.MkdirAll(state.LogDir(), 0o755); err != nil {
			return Handle{}, err
		}
		p.Log = filepath.Join(state.LogDir(), p.ID+".log")
		logf, err := os.OpenFile(p.Log, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			return Handle{}, err
		}
		defer logf.Close()
		devnull, _ := os.Open(os.DevNull)
		defer devnull.Close()
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, logf, logf
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	} else {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}

	if err := cmd.Start(); err != nil {
		return Handle{}, err
	}
	p.PID = cmd.Process.Pid
	if info, err := proc.Get(p.PID); err == nil {
		p.StartTime = info.StartTime
	}
	if err := state.Update(func(s *state.State) error {
		s.Procs = append(s.Procs, p)
		return nil
	}); err != nil {
		_ = proc.Terminate(p.PID, time.Second)
		return Handle{}, err
	}
	if spec.Detach {
		// Reap the child in the background if we're still around when it exits,
		// otherwise it is reparented and init does it.
		go cmd.Wait() //nolint:errcheck
		return Handle{Proc: p}, nil
	}
	return Handle{Proc: p, Cmd: cmd}, nil
}

// Forget removes a proc from the registry (used after a foreground run exits).
func Forget(id string) { forget(id) }

func forget(id string) {
	_ = state.Update(func(s *state.State) error {
		s.Procs = slices.DeleteFunc(s.Procs, func(p state.Proc) bool { return p.ID == id })
		return nil
	})
}

func running(worktree, name string) (state.Proc, bool) {
	s, err := state.Load()
	if err != nil {
		return state.Proc{}, false
	}
	for _, p := range s.Procs {
		if p.Worktree == worktree && p.Name == name && proc.Alive(p.PID, p.StartTime) {
			return p, true
		}
	}
	return state.Proc{}, false
}

// WaitReady polls until something accepts connections on port, the process
// dies, or timeout elapses.
func WaitReady(p state.Proc, timeout time.Duration) error {
	if p.Port == 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(p.Port))
	for {
		if c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		if c, err := net.DialTimeout("tcp", net.JoinHostPort("::1", strconv.Itoa(p.Port)), 300*time.Millisecond); err == nil {
			c.Close()
			return nil
		}
		if !proc.Alive(p.PID, p.StartTime) {
			return fmt.Errorf("process exited before listening on :%d", p.Port)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("nothing listening on :%d after %s", p.Port, timeout)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func substitute(cmd []string, port int) []string {
	out := make([]string, len(cmd))
	ps := strconv.Itoa(port)
	for i, a := range cmd {
		out[i] = strings.ReplaceAll(a, "{port}", ps)
	}
	return out
}

func newID(name string) string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	clean := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + 32
		}
		return '-'
	}, name)
	return clean + "-" + hex.EncodeToString(b)
}
