// Package ports hands out stable, collision-free ports per (worktree, service).
//
// The preferred port is a hash of the worktree path and service name, so the
// same worktree gets the same port every time (bookmarks and .env files keep
// working), while two worktrees of the same repo never fight over :3000.
package ports

import (
	"fmt"
	"hash/fnv"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/Fahnoee/berth/internal/state"
)

// Range returns the [min, max] port range, overridable with BERTH_PORT_MIN /
// BERTH_PORT_MAX. The default stays below Linux's ephemeral range (32768+).
func Range() (int, int) {
	lo, hi := 20000, 29999
	if v, err := strconv.Atoi(os.Getenv("BERTH_PORT_MIN")); err == nil && v > 0 {
		lo = v
	}
	if v, err := strconv.Atoi(os.Getenv("BERTH_PORT_MAX")); err == nil && v >= lo {
		hi = v
	}
	return lo, hi
}

// Preferred is the deterministic first-choice port for a worktree/service.
func Preferred(worktree, service string) int {
	lo, hi := Range()
	h := fnv.New32a()
	h.Write([]byte(worktree))
	h.Write([]byte{0})
	h.Write([]byte(service))
	return lo + int(h.Sum32()%uint32(hi-lo+1))
}

// Free reports whether nothing is currently bound to port on any interface.
func Free(port int) bool {
	for _, addr := range []string{fmt.Sprintf(":%d", port), fmt.Sprintf("127.0.0.1:%d", port)} {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return false
		}
		l.Close()
	}
	return true
}

// Lease returns the port leased to (worktree, service), allocating one if
// needed. Existing leases are returned even if the port is busy — usually it
// is busy because the service is already running.
func Lease(worktree, service string) (port int, fresh bool, err error) {
	err = state.Update(func(s *state.State) error {
		now := time.Now()
		for i := range s.Leases {
			l := &s.Leases[i]
			if l.Worktree == worktree && l.Service == service {
				l.LastUsed = now
				port = l.Port
				return nil
			}
		}
		taken := map[int]bool{}
		for _, l := range s.Leases {
			taken[l.Port] = true
		}
		lo, hi := Range()
		span := hi - lo + 1
		start := Preferred(worktree, service)
		for i := 0; i < span; i++ {
			p := lo + (start-lo+i)%span
			if taken[p] || !Free(p) {
				continue
			}
			port, fresh = p, true
			s.Leases = append(s.Leases, state.Lease{
				Port: p, Worktree: worktree, Service: service, Created: now, LastUsed: now,
			})
			return nil
		}
		return fmt.Errorf("no free port in %d-%d", lo, hi)
	})
	return port, fresh, err
}

// Release drops leases for a worktree; if services is empty, all of them.
func Release(worktree string, services ...string) (released []state.Lease, err error) {
	want := map[string]bool{}
	for _, s := range services {
		want[s] = true
	}
	err = state.Update(func(s *state.State) error {
		kept := s.Leases[:0]
		for _, l := range s.Leases {
			if l.Worktree == worktree && (len(want) == 0 || want[l.Service]) {
				released = append(released, l)
				continue
			}
			kept = append(kept, l)
		}
		s.Leases = kept
		return nil
	})
	return released, err
}
