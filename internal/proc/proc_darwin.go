package proc

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Get shells out to ps/lsof; macOS has no /proc.
func Get(pid int) (Info, error) {
	p := strconv.Itoa(pid)
	out, err := exec.Command("ps", "-o", "ppid=,pgid=,uid=,stat=,lstart=", "-p", p).Output()
	if err != nil {
		return Info{}, err
	}
	f := strings.Fields(string(out))
	if len(f) < 9 {
		return Info{}, os.ErrNotExist
	}
	info := Info{PID: pid}
	info.PPID, _ = strconv.Atoi(f[0])
	info.PGID, _ = strconv.Atoi(f[1])
	info.UID, _ = strconv.Atoi(f[2])
	if t, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(f[4:9], " "), time.Local); err == nil {
		info.Started = t
		info.StartTime = t.Unix()
	}
	if cmd, err := exec.Command("ps", "-o", "command=", "-p", p).Output(); err == nil {
		info.Cmdline = strings.Fields(strings.TrimSpace(string(cmd)))
	}
	if comm, err := exec.Command("ps", "-o", "comm=", "-p", p).Output(); err == nil {
		c := strings.TrimSpace(string(comm))
		info.Name = c[strings.LastIndexByte(c, '/')+1:]
	}
	if cwd, err := exec.Command("lsof", "-a", "-p", p, "-d", "cwd", "-Fn").Output(); err == nil {
		for _, line := range strings.Split(string(cwd), "\n") {
			if v, ok := strings.CutPrefix(line, "n"); ok {
				info.Cwd = v
			}
		}
		if info.Cwd != "" {
			if _, err := os.Stat(info.Cwd); err != nil {
				info.CwdDeleted = true
			}
		}
	}
	return info, nil
}

func isZombie(pid int) bool {
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && bytes.HasPrefix(bytes.TrimSpace(out), []byte("Z"))
}

// Listeners uses lsof to map PID -> listening TCP ports.
func Listeners() (map[int][]int, error) {
	out, err := exec.Command("lsof", "-nP", "-iTCP", "-sTCP:LISTEN", "-Fpn").Output()
	res := map[int][]int{}
	if err != nil && len(out) == 0 {
		return res, nil // lsof exits 1 when nothing matches
	}
	pid := 0
	seen := map[[2]int]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n"):
			i := strings.LastIndexByte(line, ':')
			port, err := strconv.Atoi(line[i+1:])
			if err != nil || seen[[2]int{pid, port}] {
				continue
			}
			seen[[2]int{pid, port}] = true
			res[pid] = append(res[pid], port)
		}
	}
	return res, nil
}
