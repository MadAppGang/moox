package main

// serve_boot.go is the boot half of `tt serve` (moox §6.4): the cold gate's
// BootGate call inside --ensure and the mark that names the daemon as the
// boot restorer. The restore itself — the bounded bootstrap poll,
// Engine.Restore and the LAUNCH verdict — runs in the daemon's saver loop
// (internal/saver, --foreground --boot).

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/MadAppGang/moox/internal/engine"
	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// envStub is the --stub-commands equivalent in the env that starts a server.
const envStub = "MOOX_STUB_COMMANDS"

func newEngine(t tmuxtarget.Target, claudeRoot string, log io.Writer) (*engine.Engine, error) {
	return engine.New(engine.Options{Target: t, ClaudeRoot: claudeRoot, Log: log,
		Producer: "moox " + observer.ResolveVersion()})
}

// bootGate runs BootGate on a matching server (§6.4 checks 2–6, both purges
// on a cold server). It returns whether the daemon is to boot-restore.
func (g *gate) bootGate(ctx context.Context, sessions int) bool {
	e, err := newEngine(g.t, g.claude, g.log)
	if err != nil {
		g.logf("BOOT-GATE engine: %v", err)
		return false
	}
	dec, err := e.BootGate(ctx, sessions)
	if err != nil {
		g.logf("BOOT-GATE %v", err)
	}
	g.logf("%s", dec.Line())
	return dec.Restore
}

// markBootRestore is the chained call right after the fork (PR-M2): the
// value names the daemon, which exists, so a save that reads it is refused.
func (g *gate) markBootRestore(ctx context.Context, daemonPid int) {
	v := fmt.Sprintf("%s:%d:%d", engine.KindBoot, daemonPid, time.Now().Unix())
	if _, err := g.t.Run(ctx, tmuxtarget.Chain(
		[]string{"set-option", "-g", "@moox-restoring", v},
		[]string{"set-option", "-g", "@moox-active", "1"},
	)...); err != nil {
		g.logf("BOOT-MARK %v", err)
		return
	}
	g.logf("BOOT-RESTORE decided: @moox-restoring %s", v)
}
