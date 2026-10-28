package scan

import (
	"slices"
	"time"

	"github.com/Fahnoee/berth/internal/gitwt"
	"github.com/Fahnoee/berth/internal/proc"
	"github.com/Fahnoee/berth/internal/state"
)

// ReapOptions select what to clean up.
type ReapOptions struct {
	DryRun          bool
	IncludeDetached bool          // also reap untracked servers reparented to init
	Worktree        string        // reap everything running in this worktree, abandoned or not
	OlderThan       time.Duration // reap anything (in a repo) older than this
	Grace           time.Duration
}

// Selected reports whether an entry would be reaped under opts.
func (o ReapOptions) Selected(e Entry, now time.Time) bool {
	if o.Worktree != "" {
		return e.Worktree == o.Worktree || Within(e.Cwd, o.Worktree)
	}
	if o.OlderThan > 0 && e.Worktree != "" && now.Sub(e.Started) > o.OlderThan {
		return true
	}
	return e.Abandoned(o.IncludeDetached)
}

// Reap terminates selected entries, forgets them, and drops port leases whose
// worktree no longer exists. It returns what was (or would be) reaped.
func Reap(entries []Entry, opts ReapOptions) (reaped []Entry, leases []state.Lease, errs []error) {
	if opts.Grace == 0 {
		opts.Grace = 5 * time.Second
	}
	now := time.Now()
	var ids []string
	for _, e := range entries {
		if !opts.Selected(e, now) {
			continue
		}
		if !opts.DryRun {
			if err := proc.Terminate(e.PID, opts.Grace); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		reaped = append(reaped, e)
		if e.ID != "" {
			ids = append(ids, e.ID)
		}
	}
	update := func(s *state.State) error {
		s.Procs = slices.DeleteFunc(s.Procs, func(p state.Proc) bool { return slices.Contains(ids, p.ID) })
		s.Leases = slices.DeleteFunc(s.Leases, func(l state.Lease) bool {
			gone := !gitwt.Exists(l.Worktree) || (opts.Worktree != "" && l.Worktree == opts.Worktree)
			if gone {
				leases = append(leases, l)
			}
			return gone
		})
		return nil
	}
	if opts.DryRun {
		s, err := state.Load()
		if err == nil {
			_ = update(s)
		}
	} else if err := state.Update(update); err != nil {
		errs = append(errs, err)
	}
	return reaped, leases, errs
}

// Stop terminates a single entry and forgets it.
func Stop(e Entry, grace time.Duration) error {
	if err := proc.Terminate(e.PID, grace); err != nil {
		return err
	}
	if e.ID == "" {
		return nil
	}
	return state.Update(func(s *state.State) error {
		s.Procs = slices.DeleteFunc(s.Procs, func(p state.Proc) bool { return p.ID == e.ID })
		return nil
	})
}
