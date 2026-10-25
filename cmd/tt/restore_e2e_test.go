//go:build tmuxe2e

package main

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
	"github.com/MadAppGang/moox/internal/tmuxtest"
)

func scratchOf(s *tmuxtest.Server) tmuxtarget.MapEnv {
	return tmuxtarget.MapEnv{"HOME": s.Home, "XDG_STATE_HOME": s.XS, "XDG_DATA_HOME": s.XD, "XDG_CONFIG_HOME": s.XC}
}

// TestE2E_TUIHandoverThenRestore (§6.4a): the empty state's offer with no
// server running bootstraps one with handover (the gate does not restore by
// itself), then the daemon restores the offered snapshot on the request.
func TestE2E_TUIHandoverThenRestore(t *testing.T) {
	a := tmuxtest.Start(t, tmuxtest.WithSession("boot"))
	for _, name := range []string{"magus", "notes"} {
		a.Run(t, tmuxtarget.Chain(tmuxtarget.NewSession{Name: tmuxtarget.Literal(name), Cwd: tmuxtarget.Literal(a.Home)}.Args())...)
	}
	a.Run(t, "new-window", "-t", "magus:", "-d")
	a.Run(t, "kill-session", "-t", "boot")
	a.Run(t, "set-option", "-g", "@moox-active", "1")
	e, err := engine.New(engine.Options{Target: a.T, Env: scratchOf(a), Producer: "moox e2e"})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := e.Save(context.Background(), engine.SaveRequest{Reason: "seed", Force: true}); err != nil || !res.Published {
		t.Fatalf("seed: %+v %v", res, err)
	}

	b := tmuxtest.Prepare(t, tmuxtest.WithBinaries(), tmuxtest.WithSnapshot(a.Paths(t).Snapshots))
	snap := readMooxSnapshot(b.T, scratchOf(b))
	if got := strings.Join(snap.sessions, ","); got != "magus,notes" || snap.windows != 3 {
		t.Fatalf("offer = %+v", snap)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := requestRestore(ctx, b.T, b.Env(), snap); err != nil {
		t.Fatalf("requestRestore: %v", err)
	}
	if r := b.Opt(t, "@moox-boot-result"); r != "HANDOVER" {
		t.Fatalf("@moox-boot-result = %q, want HANDOVER", r)
	}
	out := b.Run(t, "list-sessions", "-F", "#{session_name}")
	got := strings.Fields(out)
	sort.Strings(got)
	if strings.Join(got, ",") != "magus,notes" {
		t.Fatalf("sessions = %v (bootstrap %q must be gone)", got, bootstrapSession)
	}
	if !tmuxtest.Poll(60*time.Second, 200*time.Millisecond, func() bool { return b.Opt(t, "@moox-restoring") == "" }) {
		t.Fatal("the LAUNCH verdict did not end the restore")
	}
}
