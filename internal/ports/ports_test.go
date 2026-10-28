package ports

import (
	"net"
	"strconv"
	"testing"
)

func TestLeaseStableAndUnique(t *testing.T) {
	t.Setenv("BERTH_HOME", t.TempDir())
	t.Setenv("BERTH_PORT_MIN", "41000")
	t.Setenv("BERTH_PORT_MAX", "41009")

	a, fresh, err := Lease("/src/app", "web")
	if err != nil || !fresh {
		t.Fatalf("first lease: port %d fresh %v err %v", a, fresh, err)
	}
	again, fresh, err := Lease("/src/app", "web")
	if err != nil || fresh || again != a {
		t.Fatalf("re-lease: got %d fresh %v err %v, want %d", again, fresh, err, a)
	}

	seen := map[int]bool{a: true}
	for _, wt := range []string{"/src/app-a", "/src/app-b", "/src/app-c", "/src/app-d"} {
		p, _, err := Lease(wt, "web")
		if err != nil {
			t.Fatal(err)
		}
		if seen[p] {
			t.Fatalf("port %d handed out twice", p)
		}
		seen[p] = true
	}

	released, err := Release("/src/app")
	if err != nil || len(released) != 1 || released[0].Port != a {
		t.Fatalf("release: %v %v", released, err)
	}
}

func TestLeaseSkipsBusyPorts(t *testing.T) {
	t.Setenv("BERTH_HOME", t.TempDir())
	want := Preferred("/src/app", "web")
	l, err := net.Listen("tcp", ":"+strconv.Itoa(want))
	if err != nil {
		t.Skipf("can't bind preferred port %d: %v", want, err)
	}
	defer l.Close()
	got, _, err := Lease("/src/app", "web")
	if err != nil {
		t.Fatal(err)
	}
	if got == want {
		t.Fatalf("leased busy port %d", got)
	}
}
