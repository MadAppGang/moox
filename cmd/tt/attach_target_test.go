package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestInsideTarget_RefusesAnotherServer (G2-H8): inside server A,
// `tt --tmux-socket B` must not run switch-client against B (it would move
// one of B's own clients); it refuses. The same server — also through a
// symlinked socket dir — switches; outside tmux it attaches.
func TestInsideTarget_RefusesAnotherServer(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(real, "a"), filepath.Join(real, "b")
	if in, err := insideTarget("", b); in || err != nil {
		t.Fatalf("outside tmux: %v %v", in, err)
	}
	if in, err := insideTarget(a+",123,0", a); !in || err != nil {
		t.Fatalf("same server: %v %v", in, err)
	}
	if in, err := insideTarget(filepath.Join(link, "a")+",123,0", a); !in || err != nil {
		t.Fatalf("same server through a symlinked dir: %v %v", in, err)
	}
	if in, err := insideTarget(a+",123,0", b); in || err == nil {
		t.Fatalf("another server: %v %v, want a refusal", in, err)
	}
}
