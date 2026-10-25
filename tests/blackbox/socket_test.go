package blackbox_test

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func requireSlots(t *testing.T, s *server, active bool) {
	t.Helper()
	hooks := s.mustTmux("show-hooks", "-g")
	for _, slot := range []int{101, 102, 103, 104} {
		if got := containsHook(hooks, slot); got != active {
			t.Errorf("hook slot %d present=%v, want %v\nhooks:\n%s", slot, got, active, hooks)
		}
	}
	for _, key := range []string{"C-s", "Z"} {
		r := s.tmuxRun("list-keys", "-T", "prefix", key)
		present := r.code == 0 && strings.TrimSpace(r.out) != ""
		if present != active {
			t.Errorf("prefix %s present=%v, want %v: %s %s", key, present, active, r.out, r.err)
		}
	}
}

// keyDirs lists the §4.2 per-key directories that exist now.
func keyDirs(e *sandbox) []string {
	e.t.Helper()
	var dirs []string
	for _, root := range []string{filepath.Join(e.state, "moox", "sockets"), filepath.Join(e.state, "tt", "sockets")} {
		entries, err := os.ReadDir(root)
		if err != nil && !os.IsNotExist(err) {
			e.t.Fatal(err)
		}
		for _, entry := range entries {
			dirs = append(dirs, filepath.Join(root, entry.Name()))
		}
	}
	return dirs
}

// §12.4: a non-matching socket has "no files under its key". The contract does
// not say an excluded gate exports MOOX_STATE_KEY, so the oracle is that no
// per-key directory beyond `allowed` exists under XS/moox/sockets or
// XS/tt/sockets; if a key IS exported, nothing may exist under it either.
func requireNoKeyFiles(e *sandbox, s *server, allowed ...string) {
	e.t.Helper()
	for _, dir := range keyDirs(e) {
		ok := false
		for _, key := range allowed {
			ok = ok || filepath.Base(dir) == key
		}
		if !ok {
			e.t.Errorf("nonmatched socket created per-key state: %s", dir)
		}
	}
	key := s.globalEnv("MOOX_STATE_KEY")
	if key == "" {
		return
	}
	// This exclusion check uses exported paths; TEST-33 independently asserts
	// the now-supplied exact formulas, including scans for stray writes.
	for _, env := range []string{"MOOX_STATE_DIR", "MOOX_LAUNCH_DIR", "MOOX_PENDING_DIR", "MOOX_PANES_DIR"} {
		dir := s.globalEnv(env)
		if dir == "" {
			continue
		}
		e.requireInside(dir)
		for rel, row := range image(e.t, dir) {
			if !row.Mode.IsDir() {
				e.t.Errorf("nonmatched socket has file in %s: %s", env, rel)
			}
		}
	}
	for _, root := range []string{e.home, e.config, e.state, e.data} {
		for rel, row := range image(e.t, root) {
			if row.Mode.IsDir() {
				continue
			}
			for _, part := range strings.Split(rel, string(filepath.Separator)) {
				if part == key {
					e.t.Errorf("nonmatched socket has file under its state key %q: %s", key, filepath.Join(root, rel))
				}
			}
		}
	}
}

// TEST-20, REQ-12/REQ-13/REQ-37: source the actual shipped config for BOTH
// sockets. Only the test wrapper provides isolation, shell choice, and options.
func TestSockets_MatchingServerSavesButNonmatchingColdServerDoesNotRestoreOrWriteOtherKey(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	// Fake legacy dirs replace the forbidden live-home checksum oracle.
	legacy := []string{
		filepath.Join(e.home, ".config", "tmux-claude"),
		filepath.Join(e.home, ".config", "tmux-cc"),
		filepath.Join(e.home, ".local", "state", "moox"),
		filepath.Join(e.home, ".local", "state", "tt"),
		filepath.Join(e.home, ".local", "share", "tmux", "resurrect"),
	}
	legacyImages := map[string]map[string]fileImage{}
	for _, dir := range legacy {
		writeFile(t, filepath.Join(dir, "blackbox-nonce"), "legacy-"+e.nonce+"\n", 0600)
		legacyImages[dir] = image(t, dir)
	}
	a := e.newServer("a", "SELF", false)
	a.makeRestorable()
	a.requireActive("1")
	requireSlots(t, a, true)
	pointer, _ := a.save()
	keyA := a.globalEnv("MOOX_STATE_KEY")
	if keyA == "" {
		t.Fatal("active gate did not export a state key")
	}
	for _, env := range []string{"MOOX_STATE_DIR", "MOOX_LAUNCH_DIR", "MOOX_PENDING_DIR", "MOOX_PANES_DIR"} {
		path := a.globalEnv(env)
		e.requireInside(path)
	}
	stateA := a.globalEnv("MOOX_STATE_DIR")
	beforeState := image(t, stateA)
	beforeSnapshots := image(t, filepath.Dir(pointer))
	for _, dir := range legacy {
		equalImage(t, image(t, dir), legacyImages[dir], "fake legacy directory after matching save "+dir)
	}
	// Do NOT pass NO_AUTO_RESTORE to b: exclusion itself must suppress restore.
	b := e.newServer("b", a.name, true)
	b.requireActive("0")
	requireSlots(t, b, false)
	if got := b.mustTmux("list-sessions", "-F", "#{session_name}"); got != "bootstrap" {
		t.Errorf("excluded cold server restored foreign sessions: %q", got)
	}
	if got := b.mustTmux("list-panes", "-a", "-F", "#{pane_index}"); got != "0" {
		t.Errorf("excluded cold server has more than its bootstrap pane: %q", got)
	}
	requireCode(t, b.mooxRun("save", "--direct"), 3)
	if keyB := b.globalEnv("MOOX_STATE_KEY"); keyB == keyA {
		t.Errorf("excluded server reuses the matched socket's state key: a=%q b=%q", keyA, keyB)
	}
	requireNoKeyFiles(e, b, keyA)
	if got := e.pointerFiles(); len(got) != 1 || got[0] != pointer {
		t.Errorf("excluded server created another snapshot pointer: %q", got)
	}
	equalImage(t, image(t, stateA), beforeState, "other socket's active state")
	equalImage(t, image(t, filepath.Dir(pointer)), beforeSnapshots, "other socket's snapshots")
	for _, dir := range legacy {
		equalImage(t, image(t, dir), legacyImages[dir], "fake legacy directory "+dir)
	}
}

// TEST-21, REQ-12: exercise default policy, name lists, globs, and wildcard.
// The default policy is checked on a safe nondefault socket, never on default.
func TestSockets_ListsGlobsAndWildcardActivateOnlyMatchingSocket(t *testing.T) {
	for _, kind := range []string{"list", "glob", "wildcard", "nonmatching", "default_policy"} {
		t.Run(kind, func(t *testing.T) {
			e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
			name := "moox-bb-" + e.nonce + "-pattern"
			pattern, active := "", "1"
			switch kind {
			case "list":
				pattern = "never-match " + name + " also-never"
			case "glob":
				pattern = "moox-bb-" + e.nonce + "-pat*"
			case "wildcard":
				pattern = "*"
			case "nonmatching":
				pattern, active = "never-match-*", "0"
			case "default_policy":
				pattern, active = "DEFAULT_POLICY", "0"
			}
			s := e.newServer("pattern", pattern, false)
			if kind == "default_policy" {
				pattern = "default"
			}
			s.requireActive(active)
			requireSlots(t, s, active == "1")
			if got := s.mustTmux("show-options", "-gqv", "@moox-sockets"); got != pattern {
				t.Errorf("shipped config overwrote user's socket policy: %q != %q", got, pattern)
			}
		})
	}
}

// TEST-22, REQ-12/REQ-37: a reload must remove participation, not merely avoid
// installing hooks the first time.
func TestSockets_ReloadExclusionRemovesHooksKeysAndActiveMarker(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	s := e.newServer("exclude", "SELF", false)
	s.makeRestorable()
	pointer, _ := s.save()
	state := s.globalEnv("MOOX_STATE_DIR")
	e.requireInside(state)
	before := image(t, filepath.Dir(pointer))
	s.mustTmux("set-option", "-g", "@moox-sockets", "never-match-*")
	s.mustTmux("source-file", filepath.Join(e.share, "moox.conf"))
	s.requireActive("0")
	requireSlots(t, s, false)
	if _, err := os.Stat(filepath.Join(state, "active")); !os.IsNotExist(err) {
		t.Errorf("active marker survived exclusion: %v", err)
	}
	requireCode(t, s.mooxRun("save", "--direct"), 3)
	equalImage(t, image(t, filepath.Dir(pointer)), before, "excluded server's snapshots")
}

// TEST-23, REQ-38/REQ-27: reload the shipped config, not the wrapper which
// deliberately resets a few test options.
func TestSockets_ReloadNeverRestoresAnExistingSnapshot(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	s := e.newServer("reload", "SELF", false)
	s.makeRestorable()
	s.save()
	s.mustTmux("new-window", "-d", "-t", "magus:", "-n", "survives-reload", "-c", e.home, "/bin/sh")
	// Identity view: pane_index is display-relative (moox.conf's own
	// pane-base-index renumbers it on reload), so compare ids, pids and metadata.
	identity := func() []string {
		rows := strings.Split(s.mustTmux("list-panes", "-a", "-F", strings.Join([]string{"#{session_id}", "#{session_name}",
			"#{window_id}", "#{window_name}", "#{pane_id}", "#{pane_pid}", "#{pane_current_path}", "#{pane_title}"}, viewSep)), "\n")
		sort.Strings(rows)
		return rows
	}
	before := identity()
	s.mustTmux("source-file", filepath.Join(e.share, "moox.conf"))
	s.requireActive("1")
	if got := identity(); !reflect.DeepEqual(got, before) {
		t.Errorf("reload restored or replaced panes\nbefore=%q\nafter=%q", before, got)
	}
}

// TEST-24, REQ-36: RR12 requires literal exit 0 AND no output on repeated
// ensure, not merely a daemon that happens to be present.
func TestTT_EnsureIsIdempotentSilentAndReturns0OnMatchedAndExcludedServers(t *testing.T) {
	for _, active := range []bool{true, false} {
		name := "excluded"
		if active {
			name = "matched"
		}
		t.Run(name, func(t *testing.T) {
			e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
			pattern, want := "never-match-*", "0"
			if active {
				pattern, want = "SELF", "1"
			}
			s := e.newServer("ensure", pattern, false)
			for i := 0; i < 2; i++ {
				r := e.run(e.tt, nil, "", "serve", "--ensure", "--tmux-socket", s.path)
				requireCode(t, r, 0)
				if r.out != "" || r.err != "" {
					t.Errorf("ensure must print nothing: stdout=%q stderr=%q", r.out, r.err)
				}
			}
			s.requireActive(want)
			if !active {
				requireNoKeyFiles(e, s)
			}
		})
	}
}
