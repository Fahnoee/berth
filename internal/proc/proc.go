// Package proc is a tiny, dependency-free view of the OS process table:
// who is listening on which TCP port, and what that process is.
package proc

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Info describes one process.
type Info struct {
	PID        int
	PPID       int
	PGID       int
	UID        int
	Name       string
	Cmdline    []string
	Cwd        string
	CwdDeleted bool
	StartTime  int64 // opaque, stable for the life of the process; guards against PID reuse
	Started    time.Time
}

// Command renders the command line for display.
func (i Info) Command() string {
	if len(i.Cmdline) == 0 {
		return i.Name
	}
	return strings.Join(i.Cmdline, " ")
}

// Exe returns the basename of argv[0] (or the comm name).
func (i Info) Exe() string {
	if len(i.Cmdline) > 0 && i.Cmdline[0] != "" {
		return filepath.Base(i.Cmdline[0])
	}
	return i.Name
}

// Alive reports whether pid is running and (if startTime != 0) is still the
// same process we recorded earlier.
func Alive(pid int, startTime int64) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	if startTime == 0 {
		return true
	}
	info, err := Get(pid)
	if err != nil {
		return false
	}
	return info.StartTime == startTime
}

// Terminate sends SIGTERM to the process (group, if it leads one), waits up to
// grace for it to exit, then SIGKILLs it.
func Terminate(pid int, grace time.Duration) error {
	if pid <= 1 || pid == os.Getpid() {
		return errors.New("refusing to kill this pid")
	}
	target := pid
	if pgid, err := syscall.Getpgid(pid); err == nil && pgid == pid && pgid != syscall.Getpgrp() {
		target = -pid // whole process group: dev servers love spawning children
	}
	if err := syscall.Kill(target, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !Alive(pid, 0) || isZombie(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := syscall.Kill(target, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

// Ancestors returns the parent chain of pid (closest first), stopping at init.
func Ancestors(pid int) []Info {
	var out []Info
	seen := map[int]bool{}
	for cur := pid; cur > 1 && !seen[cur]; {
		seen[cur] = true
		info, err := Get(cur)
		if err != nil {
			break
		}
		if cur != pid {
			out = append(out, info)
		}
		cur = info.PPID
	}
	return out
}
