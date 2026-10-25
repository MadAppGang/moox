package main

// restore.go is the empty state's "restore the whole workspace" offer. Since
// moox P7 it no longer restores anything itself (PR-C3, §6.4a): it reads the
// server's moox-last, and on enter it sends a per-nonce restore REQUEST to
// the server's daemon — bootstrapping the server with handover when none
// runs — and returns once the daemon has created the panes. The LAUNCH
// verdict keeps running in the daemon, so quitting tt never cuts it short.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/MadAppGang/moox/internal/engine"
	"github.com/MadAppGang/moox/internal/mooxroot"
	"github.com/MadAppGang/moox/internal/saver"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// bootstrapSession is the session tt starts a server with when none runs
// (§6.4a step 0: tt uses "0", moox uses "main"); the daemon's restore renames
// it out of the way and drops it once the workspace exists.
const bootstrapSession = "0"

// snapshot describes the moox snapshot a restore would bring back. A zero
// snapshot means "nothing worth restoring", which is what every failure path
// here returns — the empty state simply does not offer the action rather
// than showing an error for a file the user never asked about.
type snapshot struct {
	path string
	// sessions holds the session names in the snapshot.
	sessions []string
	windows  int
	savedAt  time.Time
}

func (s snapshot) empty() bool { return len(s.sessions) == 0 }

func (s snapshot) hasSession(name string) bool {
	for _, n := range s.sessions {
		if n == name {
			return true
		}
	}
	return false
}

// contents describes what the snapshot holds, e.g. "12 sessions, 31 windows".
func (s snapshot) contents() string {
	return plural(len(s.sessions), "session") + ", " + plural(s.windows, "window")
}

// summary is contents plus how old it is, e.g.
// "12 sessions, 31 windows, saved 2h ago". The age is what tells you whether
// this is the snapshot you wanted, so it belongs anywhere you are being ASKED to
// restore — and nowhere once you already have.
//
// It takes the current time rather than calling time.Now() so the rendering is a
// pure function and can be pinned by a test; the View passes time.Now().
func (s snapshot) summary(now time.Time) string {
	return fmt.Sprintf("%s, saved %s", s.contents(), humanAge(now.Sub(s.savedAt)))
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// humanAge renders a duration at the coarsest unit that still says something
// useful. A negative age (a snapshot stamped in the future — clock skew, or a
// file copied off another machine) reads as "just now" rather than as a
// nonsense negative.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
}

// The two halves of the feature are variables, like the mutators in tmux.go, so
// tests can substitute a fake snapshot and a recording restore instead of
// touching the developer's real tmux server and workspace.
var (
	loadSnapshot    = func() snapshot { return readMooxSnapshot(ttTarget(), tmuxtarget.OSEnv{}) }
	restoreSnapshot = func() error {
		return requestRestore(context.Background(), ttTarget(), os.Environ(), loadSnapshot())
	}
)

// readMooxSnapshot reads the server's moox-last (healing through the newest
// valid snapshot, never writing the pointer) and counts what it holds.
func readMooxSnapshot(t tmuxtarget.Target, env tmuxtarget.Env) snapshot {
	if t.Socket == "" {
		return snapshot{}
	}
	e, err := engine.New(engine.Options{Target: t, Env: env})
	if err != nil {
		return snapshot{}
	}
	info, err := e.Inspect()
	if err != nil || info.LastErr != nil || info.LastSnapshot == nil {
		return snapshot{}
	}
	s := snapshot{path: info.LastPath, windows: len(info.LastSnapshot.Windows)}
	for _, ss := range info.LastSnapshot.Sessions {
		s.sessions = append(s.sessions, ss.Name)
	}
	if ts, err := time.Parse(time.RFC3339, info.LastSnapshot.CreatedAt); err == nil {
		s.savedAt = ts
	} else if fi, err := os.Stat(info.LastPath); err == nil {
		s.savedAt = fi.ModTime()
	}
	return s
}

// shareRoot is the moox share root for the bootstrap's moox.conf: $MOOX_ROOT
// when it holds moox.conf, else this executable's stable root (§3.2).
func shareRoot(environ []string) (string, error) {
	for _, kv := range environ {
		if v, ok := cutPrefix(kv, "MOOX_ROOT="); ok && filepath.IsAbs(v) {
			if _, err := os.Stat(filepath.Join(v, "moox.conf")); err == nil {
				return v, nil
			}
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	wd, _ := os.Getwd()
	return mooxroot.Stable(exe, wd)
}

func cutPrefix(s, p string) (string, bool) {
	if len(s) >= len(p) && s[:len(p)] == p {
		return s[len(p):], true
	}
	return "", false
}

// requestRestore is the requester half of §6.4a for the TUI: bootstrap with
// handover when no server answers, make sure a daemon serves it, then one
// per-nonce request for the snapshot the offer showed, waiting until its
// panes exist.
func requestRestore(ctx context.Context, t tmuxtarget.Target, environ []string, snap snapshot) error {
	if snap.empty() || snap.path == "" {
		return errors.New("no saved session to restore")
	}
	root, err := shareRoot(environ)
	if err != nil {
		return fmt.Errorf("moox share root: %w", err)
	}
	e, err := engine.New(engine.Options{Target: t, Env: environMap(environ)})
	if err != nil {
		return err
	}
	// tt serve --ensure runs as #{@moox-bin}/tt, the binary the server's
	// own config resolved (the gate set it while the bootstrap started).
	c := saver.Client{Target: t, Paths: e.Paths(), Root: root, Environ: environ, Session: bootstrapSession}
	if _, err := c.Bootstrap(ctx); err != nil {
		return err
	}
	if err := c.EnsureDaemon(ctx); err != nil {
		return err
	}
	if err := e.ManualPreconditions(ctx); err != nil {
		return err
	}
	_, err = c.Request(ctx, snap.path, false)
	return err
}

func environMap(environ []string) tmuxtarget.MapEnv {
	m := tmuxtarget.MapEnv{}
	for _, kv := range environ {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				m[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return m
}
