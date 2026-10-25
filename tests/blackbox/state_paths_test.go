package blackbox_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// A deliberately absent *virtual* tmux dependency. It never executes real tmux,
// so even an offline legacy-default import cannot inspect or contact a default
// server. This is a dependency fixture, not an internal invocation-count oracle.
func offlineTmux(e *sandbox) map[string]string {
	e.t.Helper()
	path := filepath.Join(e.base, "control", "offline-tmux")
	writeFile(e.t, path, "#!/bin/sh\nif [ \"$#\" -eq 1 ] && [ \"$1\" = -V ]; then printf 'tmux 3.8\\n'; exit 0; fi\nexit 1\n", 0700)
	return map[string]string{"MOOX_TMUX_BIN": path, "TT_TMUX_BIN": path, "MOOX_TMUX_SOCKET": ""}
}

func requireMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("required path %s: %v", path, err)
	}
	if info.Mode().Perm() != mode {
		t.Errorf("%s mode=%04o, want %04o", path, info.Mode().Perm(), mode)
	}
}

func requireOnlyPaths(t *testing.T, got, want []string, label string) {
	t.Helper()
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s paths=%q, want exactly %q", label, got, want)
	}
}

// Scan all fake HOME/XDG roots, not just the correctly exported directory. This
// catches a correct primary write accompanied by a stray legacy/wrong-key copy.
func requireArtifactPlacement(e *sandbox, snapshots, runtime string) {
	e.t.Helper()
	var pointers []string
	for _, root := range []string{e.home, e.config, e.state, e.data} {
		for rel, row := range image(e.t, root) {
			if row.Mode.IsDir() {
				continue
			}
			path, base := filepath.Join(root, rel), filepath.Base(rel)
			parent := filepath.Dir(path)
			switch {
			case base == "moox-last":
				pointers = append(pointers, path)
				if parent != snapshots {
					e.t.Errorf("pointer outside exact contract dir: %s", path)
				}
			case strings.HasPrefix(base, "moox-") && (strings.HasSuffix(base, ".json") || strings.HasSuffix(base, ".contents.gz")):
				if parent != snapshots {
					e.t.Errorf("snapshot/contents outside exact contract dir: %s", path)
				}
			case base == "pins" || base == "restoring-from" || base == "active" || base == "saver.state" || base == "save.lock" || base == "daemon.log":
				if parent != runtime {
					e.t.Errorf("runtime file outside exact contract dir: %s", path)
				}
			}
		}
	}
	requireOnlyPaths(e.t, pointers, []string{filepath.Join(snapshots, "moox-last")}, "published pointer")
}

func exactParticipatingPaths(e *sandbox, s *server) (runtime, snapshots string) {
	e.t.Helper()
	key := s.globalEnv("MOOX_STATE_KEY")
	if key == "" || key == "default" || filepath.Base(key) != key {
		e.t.Fatalf("nondefault state key=%q", key)
	}
	// The key algorithm is not supplied: use the public exported key, but the
	// independent contract formulas below (never trust exported path strings).
	runtime = filepath.Join(e.state, "moox", "sockets", key)
	snapshots = filepath.Join(runtime, "snapshots")
	envPaths := map[string]string{
		"MOOX_STATE_DIR":   runtime,
		"MOOX_LAUNCH_DIR":  filepath.Join(runtime, "launch"),
		"MOOX_PENDING_DIR": filepath.Join(runtime, "pending"),
		"MOOX_PANES_DIR":   filepath.Join(runtime, "panes"),
	}
	for name, want := range envPaths {
		if got := s.globalEnv(name); got != want {
			e.t.Errorf("%s=%q, want exact %q", name, got, want)
		}
		requireMode(e.t, want, 0700)
	}
	options := map[string]string{
		"@claude-continuity-launch-dir":  envPaths["MOOX_LAUNCH_DIR"],
		"@claude-continuity-pending-dir": envPaths["MOOX_PENDING_DIR"],
		"@claude-continuity-panes-dir":   envPaths["MOOX_PANES_DIR"],
		"@moox-log-dir":                  filepath.Join(runtime, "log"),
	}
	for option, want := range options {
		if got := s.mustTmux("show-options", "-gqv", option); got != want {
			e.t.Errorf("%s=%q, want %q", option, got, want)
		}
	}
	logFile := s.mustTmux("show-options", "-gqv", "@claude-continuity-log-file")
	if filepath.Dir(logFile) != filepath.Join(runtime, "log") {
		e.t.Errorf("continuity log=%q, want under exact runtime/log", logFile)
	}
	if got := s.mustTmux("show-options", "-gqv", "@claude-continuity-by-pid-dir"); got != filepath.Join(e.base, "by-pid", strings.TrimPrefix(s.name, "moox-bb-"+e.nonce+"-")) {
		e.t.Errorf("gate overwrote caller's isolated by-pid override: %q", got)
	}
	return
}

// TEST-33, REQ-13/REQ-21/REQ-37: real participating nondefault servers use XS,
// never fake legacy HOME/XD locations even though those directories exist.
func TestPaths_NondefaultSaveUsesExactPerKeyPathsAndNoStrayLegacyArtifacts(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	legacy := []string{
		filepath.Join(e.home, ".config", "tmux-claude", "launch"),
		filepath.Join(e.home, ".config", "tmux-claude", "pending"),
		filepath.Join(e.home, ".config", "tmux-claude", "panes"),
		filepath.Join(e.data, "tmux", "resurrect"),
		filepath.Join(e.home, ".local", "state", "moox"),
		filepath.Join(e.home, ".local", "share", "tmux", "resurrect"),
	}
	before := map[string]map[string]fileImage{}
	for _, dir := range legacy {
		writeFile(t, filepath.Join(dir, "nonce"), e.nonce+"\n", 0600)
		before[dir] = image(t, dir)
	}
	s := e.newServer("paths", "SELF", false)
	s.makeRestorable()
	runtime, snapshots := exactParticipatingPaths(e, s)
	pointer, target := s.save()
	if pointer != filepath.Join(snapshots, "moox-last") || filepath.Dir(target) != snapshots {
		t.Errorf("actual pointer/target=%s/%s, want %s", pointer, target, snapshots)
	}
	requireMode(t, filepath.Join(runtime, "active"), 0600)
	requireArtifactPlacement(e, snapshots, runtime)
	key := s.globalEnv("MOOX_STATE_KEY")
	ttDir := filepath.Join(e.state, "tt", "sockets", key)
	// Any tt daemon state created by serve must be in the per-key root. Do not
	// assume undocumented PID-file contents or require an optional cache file.
	for _, root := range []string{e.home, e.config, e.state, e.data} {
		for rel, row := range image(t, root) {
			if row.Mode.IsDir() {
				continue
			}
			base := filepath.Base(rel)
			if base == "observer.pid" || base == "panel-mode" || strings.HasPrefix(base, "agents-cache.") {
				if filepath.Dir(filepath.Join(root, rel)) != ttDir {
					t.Errorf("tt state at wrong path: %s", filepath.Join(root, rel))
				}
			}
		}
	}
	for _, dir := range legacy {
		equalImage(t, image(t, dir), before[dir], "untouched fake legacy "+dir)
	}
}

// TEST-39, REQ-23/REQ-13: --publish accepts the exact -L NAME and -S PATH
// spellings. Both are offline, owned targets; no attach-live is requested.
func TestImport_PublishByNameOrPathWritesOnlySelectedPerKeySnapshotDirectory(t *testing.T) {
	for _, selector := range []string{"name", "path"} {
		t.Run(selector, func(t *testing.T) {
			e := newSandbox(t, "MOOX_BIN", "MOOX_SHARE")
			fixture := authorResurrectFixture(e)
			env := offlineTmux(e)
			name := "moox-bb-" + e.nonce + "-import"
			flags := []string{"-L", name}
			if selector == "path" {
				flags = []string{"-S", filepath.Join(e.base, "tmux", name)}
			}
			before := image(t, fixture.dir)
			args := append([]string{"import", "--from", fixture.file, "--publish", "--yes"}, flags...)
			requireCode(t, e.run(e.moox, env, "", args...), 0)
			pointer, target := e.singleSnapshot()
			snapshot := decodeWrittenSnapshot(t, readFile(t, target), "import-resurrect")
			assertImportedWorkspace(t, snapshot, fixture)
			if snapshot.Socket.Key == "" || snapshot.Socket.Key == "default" || filepath.Base(snapshot.Socket.Key) != snapshot.Socket.Key {
				t.Fatalf("invalid selected key=%q", snapshot.Socket.Key)
			}
			runtime := filepath.Join(e.state, "moox", "sockets", snapshot.Socket.Key)
			snapshots := filepath.Join(runtime, "snapshots")
			if pointer != filepath.Join(snapshots, "moox-last") {
				t.Errorf("published pointer=%s, want %s", pointer, snapshots)
			}
			requireArtifactPlacement(e, snapshots, runtime)
			equalImage(t, image(t, fixture.dir), before, "published import source")
		})
	}
}

// TEST-33 (legacy branch, revised by orchestrator ruling 2026-10-25), REQ-13/
// REQ-23/F1: the legacy key is ONLY the canonical default socket at the stock
// path. test-environment.md requires a relocated TMUX_TMPDIR, so `-L default`
// here is a different server: it must get its own `default@<hash>` key, need no
// --replace-live, and write nothing to any legacy path. The true legacy-key
// branch is untestable under test-environment.md (Known Gap in TEST-PLAN.md).
func TestPaths_RelocatedDefaultIsNotLegacyKeyAndWritesNoLegacyPaths(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "MOOX_SHARE")
	fixture := authorResurrectFixture(e)
	env := offlineTmux(e)
	sourceBefore := image(t, fixture.dir)
	// No --replace-live: that flag is only required on the legacy key.
	requireCode(t, e.run(e.moox, env, "", "import", "--from", fixture.file, "--publish", "-L", "default", "--yes"), 0)
	pointer, target := e.singleSnapshot()
	snapshot := decodeWrittenSnapshot(t, readFile(t, target), "import-resurrect")
	key := snapshot.Socket.Key
	if key == "default" || !strings.HasPrefix(key, "default@") || filepath.Base(key) != key {
		t.Fatalf("relocated default socket key=%q, want default@<hash>, never the legacy key", key)
	}
	runtime := filepath.Join(e.state, "moox", "sockets", key)
	snapshots := filepath.Join(runtime, "snapshots")
	if pointer != filepath.Join(snapshots, "moox-last") {
		t.Errorf("pointer=%s, want %s/moox-last", pointer, snapshots)
	}
	assertImportedWorkspace(t, snapshot, fixture)
	requireArtifactPlacement(e, snapshots, runtime)
	// Nothing in the legacy locations (§4.2 `default` column).
	for _, dir := range []string{
		filepath.Join(e.data, "tmux", "resurrect"),
		filepath.Join(e.state, "moox", "sockets", "default"),
		filepath.Join(e.state, "tt", "sockets", "default"),
		filepath.Join(e.home, ".config", "tmux-claude"),
		filepath.Join(e.home, ".config", "tmux-cc", "frozen", "default"),
	} {
		for rel, row := range image(t, dir) {
			if !row.Mode.IsDir() {
				t.Errorf("relocated default wrote a legacy path: %s", filepath.Join(dir, rel))
			}
		}
	}
	// unpin -L default acts on its own key, never on a legacy runtime file.
	pins := filepath.Join(runtime, "pins")
	writeFile(t, pins, filepath.Base(target)+"\n", 0600)
	decoy := filepath.Join(e.state, "moox", "sockets", "default", "pins")
	writeFile(t, decoy, filepath.Base(target)+"\n", 0600)
	requireCode(t, e.run(e.moox, env, "", "snapshot", "unpin", "-L", "default", "--all"), 0)
	requireNoPins(t, pins)
	if got := string(readFile(t, decoy)); got != filepath.Base(target)+"\n" {
		t.Errorf("unpin on a relocated default touched the legacy runtime pins: %q", got)
	}
	equalImage(t, image(t, fixture.dir), sourceBefore, "relocated-default import source")
}

func requireNoPins(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 {
		t.Errorf("pins remain: %q", data)
	}
}

// TEST-34, REQ-22: selective FILE versus --all; pin content belongs under
// XS/moox/sockets/<key>/pins, NOT beside the legacy or per-key JSON pointer.
func TestSnapshot_UnpinFileOrAllClearsOnlyRequestedPinsAndPreservesSnapshots(t *testing.T) {
	for _, mode := range []string{"file", "all"} {
		t.Run(mode, func(t *testing.T) {
			e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
			s := e.newServer("pins", "SELF", false)
			s.makeRestorable()
			pointer, target := s.save()
			runtime, snapshots := exactParticipatingPaths(e, s)
			second := filepath.Join(snapshots, "moox-20200101T000000.json")
			writeFile(t, second, string(readFile(t, target)), 0600)
			pins := filepath.Join(runtime, "pins")
			writeFile(t, pins, filepath.Base(target)+"\n"+filepath.Base(second)+"\n", 0600)
			// The gate evaluates pins on every load; this is not a restore.
			s.mustTmux("source-file", filepath.Join(e.share, "moox.conf"))
			eventually(t, 5*time.Second, "gate pin warning", func() (bool, string) {
				warning := s.mustTmux("show-options", "-gqv", "@moox-warning")
				return strings.Contains(warning, "2 pinned"), warning
			})
			before := image(t, snapshots)
			r := s.mooxRun("snapshot", "show", "--json")
			requireCode(t, r, 0)
			if !json.Valid([]byte(r.out)) {
				t.Errorf("snapshot show --json must print JSON: %q", r.out)
			}
			// JSON output schema isn't given; pins' exact on-disk bytes are the oracle.
			if mode == "file" {
				requireCode(t, s.mooxRun("snapshot", "unpin", target), 0)
				if got := string(readFile(t, pins)); got != filepath.Base(second)+"\n" {
					t.Errorf("selective pins=%q", got)
				}
				warning := s.mustTmux("show-options", "-gqv", "@moox-warning")
				if !strings.Contains(warning, "1 pinned") {
					t.Errorf("remaining-pin warning=%q", warning)
				}
			}
			requireCode(t, s.mooxRun("snapshot", "unpin", "--all"), 0)
			requireNoPins(t, pins)
			if got := s.mustTmux("show-options", "-gqv", "@moox-warning"); got != "" {
				t.Errorf("warning after unpin-all=%q", got)
			}
			equalImage(t, image(t, snapshots), before, "snapshots after unpin")
			if _, err := os.Stat(pointer); err != nil {
				t.Error(err)
			}
			requireArtifactPlacement(e, snapshots, runtime)
		})
	}
}
