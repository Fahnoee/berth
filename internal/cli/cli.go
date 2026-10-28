// Package cli is berth's command-line interface.
package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/Fahnoee/berth/internal/gitwt"
	"github.com/Fahnoee/berth/internal/mcp"
	"github.com/Fahnoee/berth/internal/ports"
	"github.com/Fahnoee/berth/internal/proc"
	"github.com/Fahnoee/berth/internal/runner"
	"github.com/Fahnoee/berth/internal/tui"
)

// Version is set at build time with -ldflags "-X .../cli.Version=...".
var Version = "dev"

// exitError carries a specific exit status out of a command.
type exitError struct{ code int }

func (e exitError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// Main runs berth with args and returns the process exit code.
func Main(args []string) int {
	root := newRoot()
	root.SetArgs(args)
	err := root.Execute()
	var ee exitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		return ee.code
	default:
		fmt.Fprintln(os.Stderr, "berth:", err)
		return 1
	}
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:   "berth",
		Short: "Stable ports and process hygiene for coding agents and git worktrees",
		Long: `berth is a harbor master for your dev machine.

Every git worktree gets its own stable ports, every dev server an agent starts
is tracked, and anything left behind by an agent that has exited or a worktree
that was deleted can be found and reaped.

Run berth with no arguments to open the live dashboard.`,
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		RunE:          func(*cobra.Command, []string) error { return tui.Run() },
	}
	root.AddCommand(
		runCmd(), portCmd(), lsCmd(), stopCmd(), logsCmd(), reapCmd(), releaseCmd(),
		&cobra.Command{
			Use:   "ui",
			Short: "Open the live dashboard",
			Args:  cobra.NoArgs,
			RunE:  func(*cobra.Command, []string) error { return tui.Run() },
		},
		&cobra.Command{
			Use:   "mcp",
			Short: "Serve berth's tools to coding agents over MCP (stdio)",
			Args:  cobra.NoArgs,
			RunE: func(c *cobra.Command, _ []string) error {
				ctx, stop := signal.NotifyContext(c.Context(), os.Interrupt, syscall.SIGTERM)
				defer stop()
				return mcp.Serve(ctx, os.Stdin, os.Stdout)
			},
		},
	)
	return root
}

func runCmd() *cobra.Command {
	var spec runner.Spec
	var wait time.Duration
	c := &cobra.Command{
		Use:   "run [flags] -- command [args...]",
		Short: "Run a command on this worktree's port for it, and track it",
		Long: `Run leases a stable port for the service in the current worktree, exports it
as $PORT (and $BERTH_PORT), substitutes "{port}" in the arguments, and tracks
the process so it shows up in "berth ls" and can be reaped later.

  berth run -- npm run dev
  berth run -d --name api -- uvicorn app:app --port {port}
  berth run -d --ttl 2h -- python -m http.server {port}`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			spec.Cmd = args
			h, err := runner.Start(spec)
			if errors.Is(err, runner.ErrAlreadyRunning) {
				fmt.Fprintf(os.Stderr, "%s is already running here (pid %d%s); use --replace to restart it\n",
					h.Proc.Name, h.Proc.PID, portNote(h.Proc.Port))
				return exitError{1}
			}
			if err != nil {
				return err
			}
			if spec.Detach {
				return reportDetached(h, wait)
			}
			if h.Proc.Port != 0 {
				fmt.Fprintf(os.Stderr, "⚓ %s → %s\n", h.Proc.Name, h.URL())
			}
			return foreground(h)
		},
	}
	f := c.Flags()
	f.SetInterspersed(false)
	f.StringVarP(&spec.Name, "name", "n", "", "service name (default: derived from the command)")
	f.StringVar(&spec.PortEnv, "port-env", "PORT", "environment variable that receives the port")
	f.BoolVar(&spec.NoPort, "no-port", false, "don't lease a port")
	f.BoolVarP(&spec.Detach, "detach", "d", false, "run in the background with output going to a log file")
	f.BoolVar(&spec.Replace, "replace", false, "stop an instance already running in this worktree first")
	f.DurationVar(&spec.TTL, "ttl", 0, "consider the process abandoned after this long (e.g. 2h)")
	f.DurationVar(&wait, "wait", 30*time.Second, "with --detach, wait this long for the port to accept connections (0 to skip)")
	return c
}

func reportDetached(h runner.Handle, wait time.Duration) error {
	p := h.Proc
	fmt.Printf("started %s (id %s, pid %d)\n", p.Name, p.ID, p.PID)
	if p.Port != 0 {
		fmt.Printf("  url  %s\n", h.URL())
	}
	fmt.Printf("  log  %s\n", p.Log)
	if wait <= 0 || p.Port == 0 {
		return nil
	}
	if err := runner.WaitReady(p, wait); err != nil {
		fmt.Fprintf(os.Stderr, "  not ready: %v\n", err)
		if !proc.Alive(p.PID, p.StartTime) {
			if out, terr := tail(p.Log, 20); terr == nil && out != "" {
				fmt.Fprintf(os.Stderr, "--- last lines of %s\n%s", p.Log, out)
			}
			return exitError{1}
		}
		return nil
	}
	fmt.Println("  ready")
	return nil
}

// foreground waits for an attached child, handing it the terminal so Ctrl-C
// and interactive dev-server shortcuts reach it, and forwards other signals.
func foreground(h runner.Handle) error {
	defer runner.Forget(h.Proc.ID)
	pid := h.Proc.PID
	isTTY := isTerminal(os.Stdin)
	if isTTY {
		signal.Ignore(syscall.SIGTTOU)
		_ = unix.IoctlSetPointerInt(int(os.Stdin.Fd()), unix.TIOCSPGRP, pid)
		defer func() { _ = unix.IoctlSetPointerInt(int(os.Stdin.Fd()), unix.TIOCSPGRP, syscall.Getpgrp()) }()
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)
	go func() {
		for s := range sigs {
			_ = syscall.Kill(-pid, s.(syscall.Signal))
		}
	}()
	err := h.Cmd.Wait()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return exitError{128 + int(ws.Signal())}
		}
		return exitError{ee.ExitCode()}
	}
	return err
}

func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios)
	return err == nil
}

func portCmd() *cobra.Command {
	var env bool
	c := &cobra.Command{
		Use:   "port [service...]",
		Short: "Print this worktree's stable port for a service",
		Long: `Port leases (or looks up) the port for each service in the current worktree.
The same worktree and service always get the same port, and no two worktrees
share one.

  PORT=$(berth port web) npm run dev
  berth port web api --env >> .env.local`,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) == 0 {
				args = []string{"web"}
			}
			wt := gitwt.Cwd()
			for _, svc := range args {
				port, _, err := ports.Lease(wt.Root, svc)
				if err != nil {
					return err
				}
				if env {
					fmt.Printf("%s=%d\n", envName(svc), port)
				} else {
					fmt.Println(port)
				}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&env, "env", false, "print NAME_PORT=port lines")
	return c
}

func envName(service string) string {
	s := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 32
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, service)
	return s + "_PORT"
}

func releaseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "release [service...]",
		Short: "Give up this worktree's port leases (all, or just the named services)",
		RunE: func(c *cobra.Command, args []string) error {
			wt := gitwt.Cwd()
			released, err := ports.Release(wt.Root, args...)
			if err != nil {
				return err
			}
			for _, l := range released {
				fmt.Printf("released :%d (%s)\n", l.Port, l.Service)
			}
			if len(released) == 0 {
				fmt.Println("no leases to release in", wt.Root)
			}
			return nil
		},
	}
}

func portNote(port int) string {
	if port == 0 {
		return ""
	}
	return fmt.Sprintf(", :%d", port)
}
