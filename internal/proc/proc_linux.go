package proc

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const clkTck = 100 // USER_HZ is 100 on every Linux arch that matters

var (
	bootOnce sync.Once
	bootTime time.Time
)

func boot() time.Time {
	bootOnce.Do(func() {
		b, err := os.ReadFile("/proc/stat")
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "btime "); ok {
				n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
				bootTime = time.Unix(n, 0)
			}
		}
	})
	return bootTime
}

// Get reads /proc/<pid>.
func Get(pid int) (Info, error) {
	dir := fmt.Sprintf("/proc/%d", pid)
	stat, err := os.ReadFile(dir + "/stat")
	if err != nil {
		return Info{}, err
	}
	// pid (comm) state ppid pgrp ... ; comm may contain spaces/parens.
	s := string(stat)
	lp, rp := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if lp < 0 || rp < lp {
		return Info{}, fmt.Errorf("malformed stat for %d", pid)
	}
	info := Info{PID: pid, Name: s[lp+1 : rp]}
	fields := strings.Fields(s[rp+1:])
	if len(fields) < 20 {
		return Info{}, fmt.Errorf("short stat for %d", pid)
	}
	info.PPID, _ = strconv.Atoi(fields[1])
	info.PGID, _ = strconv.Atoi(fields[2])
	info.StartTime, _ = strconv.ParseInt(fields[19], 10, 64)
	info.Started = boot().Add(time.Duration(info.StartTime) * time.Second / clkTck)

	if fi, err := os.Stat(dir); err == nil {
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			info.UID = int(st.Uid)
		}
	}
	if b, err := os.ReadFile(dir + "/cmdline"); err == nil && len(b) > 0 {
		b = bytes.TrimRight(b, "\x00")
		info.Cmdline = strings.Split(string(b), "\x00")
	}
	if cwd, err := os.Readlink(dir + "/cwd"); err == nil {
		if c, ok := strings.CutSuffix(cwd, " (deleted)"); ok {
			info.Cwd, info.CwdDeleted = c, true
		} else {
			info.Cwd = cwd
		}
	}
	return info, nil
}

func isZombie(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(b)
	rp := strings.LastIndexByte(s, ')')
	return rp > 0 && rp+2 < len(s) && s[rp+2] == 'Z'
}

// Listeners maps PID -> listening TCP ports for every process we can inspect.
func Listeners() (map[int][]int, error) {
	inodes := map[uint64]int{} // socket inode -> port
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if err := parseNetTCP(f, inodes); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	out := map[int][]int{}
	if len(inodes) == 0 {
		return out, nil
	}
	procs, _ := filepath.Glob("/proc/[0-9]*")
	for _, p := range procs {
		pid, err := strconv.Atoi(filepath.Base(p))
		if err != nil {
			continue
		}
		fds, err := os.ReadDir(p + "/fd")
		if err != nil {
			continue // not ours
		}
		seen := map[int]bool{}
		for _, fd := range fds {
			link, err := os.Readlink(p + "/fd/" + fd.Name())
			if err != nil || !strings.HasPrefix(link, "socket:[") {
				continue
			}
			ino, err := strconv.ParseUint(link[8:len(link)-1], 10, 64)
			if err != nil {
				continue
			}
			if port, ok := inodes[ino]; ok && !seen[port] {
				seen[port] = true
				out[pid] = append(out[pid], port)
			}
		}
	}
	return out, nil
}

func parseNetTCP(path string, inodes map[uint64]int) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 || fields[3] != "0A" { // 0A = TCP_LISTEN
			continue
		}
		local := fields[1]
		i := strings.LastIndexByte(local, ':')
		if i < 0 {
			continue
		}
		port, err := strconv.ParseInt(local[i+1:], 16, 32)
		if err != nil {
			continue
		}
		ino, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil || ino == 0 {
			continue
		}
		inodes[ino] = int(port)
	}
	return sc.Err()
}
