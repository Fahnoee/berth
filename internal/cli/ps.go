package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Fahnoee/berth/internal/gitwt"
	"github.com/Fahnoee/berth/internal/proc"
	"github.com/Fahnoee/berth/internal/scan"
)

const stopGrace = 5 * time.Second

func lsCmd() *cobra.Command {
	var asJSON, here, abandoned, allUsers bool
	c := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"ps"},
		Short:   "List listening processes and everything started with berth run",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			entries, err := scan.Scan(scan.Options{AllUsers: allUsers})
			if err != nil {
				return err
			}
			if here {
				root := gitwt.Cwd().Root
				entries = slices.DeleteFunc(entries, func(e scan.Entry) bool { return !inWorktree(e, root) })
			}
			if abandoned {
				entries = slices.DeleteFunc(entries, func(e scan.Entry) bool { return !e.Abandoned(true) })
			}
			if asJSON {
				if entries == nil {
					entries = []scan.Entry{}
				}
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(entries)
			}
			printTable(os.Stdout, entries)
			return nil
		},
	}
	f := c.Flags()
	f.BoolVar(&asJSON, "json", false, "print JSON")
	f.BoolVar(&here, "here", false, "only the current worktree")
	f.BoolVarP(&abandoned, "abandoned", "a", false, "only processes that look abandoned")
	f.BoolVar(&allUsers, "all-users", false, "include other users' processes")
	return c
}

func printTable(w io.Writer, entries []scan.Entry) {
	if len(entries) == 0 {
		fmt.Fprintln(w, "nothing listening")
		return
	}
	home, _ := os.UserHomeDir()
	now := time.Now()
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tPID\tPORTS\tWORKTREE\tBRANCH\tAGENT\tAGE\tSTATUS\tCOMMAND")
	for _, e := range entries {
		status := "ok"
		if len(e.Problems) > 0 {
			status = strings.Join(e.Problems, ",")
		}
		agent := e.Agent
		if agent != "" && !e.AgentAlive {
			agent += " (gone)"
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			e.Label(), e.PID, dash(joinPorts(e.Ports)), dash(shortPath(e.Worktree, home)), dash(e.Branch),
			dash(agent), age(now.Sub(e.Started)), status, clip(e.Command, 60))
	}
	tw.Flush()
}

func stopCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "stop <id|service|:port|pid>...",
		Short: "Stop processes (a service name means the one in this worktree)",
		Long: `Stop terminates a process and its process group (SIGTERM, then SIGKILL after
5s). Targets can be a berth id, a service name in the current worktree, a
port written as :3000, or a pid. --here stops everything in this worktree.`,
		RunE: func(c *cobra.Command, args []string) error {
			if all == (len(args) > 0) {
				return errors.New("give targets to stop, or --here")
			}
			entries, err := scan.Scan(scan.Options{})
			if err != nil {
				return err
			}
			var targets []scan.Entry
			if all {
				root := gitwt.Cwd().Root
				for _, e := range entries {
					if inWorktree(e, root) {
						targets = append(targets, e)
					}
				}
				if len(targets) == 0 {
					fmt.Println("nothing running in", root)
				}
			}
			for _, a := range args {
				e, err := resolve(entries, a)
				if err != nil {
					return err
				}
				targets = append(targets, e)
			}
			failed := false
			for _, e := range targets {
				if err := scan.Stop(e, stopGrace); err != nil {
					fmt.Fprintf(os.Stderr, "stop %s (pid %d): %v\n", e.Label(), e.PID, err)
					failed = true
					continue
				}
				fmt.Printf("stopped %s (pid %d%s)\n", e.Label(), e.PID, portsNote(e.Ports))
			}
			if failed {
				return exitError{1}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&all, "here", false, "stop everything running in this worktree")
	return c
}

// resolve finds the single entry a user-supplied target refers to.
func resolve(entries []scan.Entry, target string) (scan.Entry, error) {
	var match []scan.Entry
	add := func(pred func(scan.Entry) bool) {
		if len(match) == 0 {
			for _, e := range entries {
				if pred(e) {
					match = append(match, e)
				}
			}
		}
	}
	if p, ok := strings.CutPrefix(target, ":"); ok {
		port, err := strconv.Atoi(p)
		if err != nil {
			return scan.Entry{}, fmt.Errorf("bad port %q", target)
		}
		add(func(e scan.Entry) bool { return slices.Contains(e.Ports, port) })
	} else if n, err := strconv.Atoi(target); err == nil {
		add(func(e scan.Entry) bool { return e.PID == n })
		add(func(e scan.Entry) bool { return slices.Contains(e.Ports, n) })
	} else {
		root := gitwt.Cwd().Root
		add(func(e scan.Entry) bool { return e.ID == target })
		add(func(e scan.Entry) bool { return e.Service == target && inWorktree(e, root) })
		add(func(e scan.Entry) bool { return e.Service == target })
		add(func(e scan.Entry) bool { return e.ID == "" && e.Name == target && inWorktree(e, root) })
	}
	switch len(match) {
	case 0:
		return scan.Entry{}, fmt.Errorf("nothing running matches %q (see berth ls)", target)
	case 1:
		return match[0], nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%q matches %d processes:", target, len(match))
	for _, e := range match {
		fmt.Fprintf(&b, "\n  %s  pid %d  %s", dash(e.ID), e.PID, dash(e.Worktree))
	}
	return scan.Entry{}, errors.New(b.String())
}

func logsCmd() *cobra.Command {
	var follow bool
	var lines int
	c := &cobra.Command{
		Use:   "logs <id|service>",
		Short: "Show the output of a detached process",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			entries, err := scan.Scan(scan.Options{})
			if err != nil {
				return err
			}
			e, err := resolve(entries, args[0])
			if err != nil {
				return err
			}
			if e.Log == "" {
				return fmt.Errorf("%s has no log: only processes started with berth run -d are logged", e.Label())
			}
			out, err := tail(e.Log, lines)
			if err != nil {
				return err
			}
			fmt.Print(out)
			if !follow {
				return nil
			}
			return followFile(e)
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing output until the process exits")
	c.Flags().IntVarP(&lines, "lines", "n", 50, "number of lines to show")
	return c
}

func followFile(e scan.Entry) error {
	f, err := os.Open(e.Log)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := io.Copy(os.Stdout, f); err != nil {
			return err
		}
		if !proc.Alive(e.PID, 0) {
			fmt.Fprintf(os.Stderr, "--- %s exited\n", e.Label())
			return nil
		}
		select {
		case <-sigs:
			return nil
		case <-tick.C:
		}
	}
}

func reapCmd() *cobra.Command {
	var opts scan.ReapOptions
	var here bool
	c := &cobra.Command{
		Use:   "reap",
		Short: "Stop abandoned processes and drop leases for deleted worktrees",
		Long: `Reap stops processes that look abandoned:

  worktree-gone  its directory or worktree was deleted
  agent-gone     the coding agent session that started it has exited
  expired        it outlived its --ttl

With --detached it also stops untracked servers in a repo whose parent exited
(usually something an agent backgrounded with "&" or nohup). With --here it
stops everything in the current worktree, abandoned or not, and releases its
ports — handy right before "git worktree remove".`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if here {
				opts.Worktree = gitwt.Cwd().Root
			}
			entries, err := scan.Scan(scan.Options{})
			if err != nil {
				return err
			}
			reaped, leases, errs := scan.Reap(entries, opts)
			verb := "stopped"
			if opts.DryRun {
				verb = "would stop"
			}
			for _, e := range reaped {
				why := strings.Join(e.Problems, ",")
				if why == "" {
					why = "selected"
				}
				fmt.Printf("%s %s (pid %d%s) — %s\n", verb, e.Label(), e.PID, portsNote(e.Ports), why)
			}
			for _, l := range leases {
				fmt.Printf("released :%d (%s in %s)\n", l.Port, l.Service, l.Worktree)
			}
			for _, err := range errs {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
			if len(reaped) == 0 && len(leases) == 0 {
				fmt.Println("nothing to reap")
			}
			if len(errs) > 0 {
				return exitError{1}
			}
			return nil
		},
	}
	f := c.Flags()
	f.BoolVarP(&opts.DryRun, "dry-run", "n", false, "only show what would be reaped")
	f.BoolVar(&opts.IncludeDetached, "detached", false, "also reap untracked servers orphaned inside a repo")
	f.BoolVar(&here, "here", false, "reap everything in the current worktree and release its ports")
	f.DurationVar(&opts.OlderThan, "older-than", 0, "also reap anything in a repo running longer than this")
	return c
}

func inWorktree(e scan.Entry, root string) bool {
	return e.Worktree == root || scan.Within(e.Cwd, root)
}

func tail(path string, n int) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	b = bytes.TrimRight(b, "\n")
	if len(b) == 0 {
		return "", nil
	}
	lines := bytes.Split(b, []byte("\n"))
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return string(bytes.Join(lines, []byte("\n"))) + "\n", nil
}

func joinPorts(ps []int) string {
	s := make([]string, len(ps))
	for i, p := range ps {
		s[i] = ":" + strconv.Itoa(p)
	}
	return strings.Join(s, ",")
}

func portsNote(ps []int) string {
	if len(ps) == 0 {
		return ""
	}
	return ", " + joinPorts(ps)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func shortPath(p, home string) string {
	if home != "" && scan.Within(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
