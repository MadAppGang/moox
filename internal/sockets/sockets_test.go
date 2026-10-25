package sockets

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// scratchTmp builds <tmp>/private/tmp and a symlink <tmp>/tmp -> private/tmp,
// the shape macOS has, and points tmpRoot at the symlink for the test. Nothing
// is created under the real /tmp.
func scratchTmp(t *testing.T) (link, real string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real = filepath.Join(root, "private", "tmp")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(root, "tmp")
	if err := os.Symlink(filepath.Join("private", "tmp"), link); err != nil {
		t.Fatal(err)
	}
	old := tmpRoot
	tmpRoot = link
	t.Cleanup(func() { tmpRoot = old })
	return link, real
}

func uidDir() string { return "tmux-" + strconv.Itoa(os.Getuid()) }

// TestPaths_DistinctPerKey (X1, X6, X8, X10): every per-server location —
// snapshots and their pointer, launch/pending recordings, position sidecars,
// the freeze store, the log, the lock, the marker and tt's state — differs
// between two servers, including the legacy default and two sockets that
// share a basename; only the by-pid registry, tt's panel.conf, the user
// config and the veto are shared.
func TestPaths_DistinctPerKey(t *testing.T) {
	link, _ := scratchTmp(t)
	root := t.TempDir()
	env := mapEnv{"HOME": "/h"}
	sockets := []string{
		filepath.Join(link, uidDir(), "default"), // the legacy default
		filepath.Join(root, "a", "work"),
		filepath.Join(root, "b", "work"), // same basename, other dir
		filepath.Join(root, uidDir(), "default"),
	}
	var all []Paths
	for _, s := range sockets {
		p, err := For(s, env)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, p)
	}
	perKey := func(p Paths) map[string]string {
		return map[string]string{"Key": p.Key, "Snapshots": p.Snapshots, "Launch": p.Launch, "Pending": p.Pending,
			"Panes": p.Panes, "FreezeDir": p.FreezeDir, "State": p.State, "Log": p.Log, "Lock": p.Lock,
			"Marker": p.Marker, "TTState": p.TTState}
	}
	shared := func(p Paths) [4]string { return [4]string{p.ByPidDir, p.TTPanelConf, p.UserConfig, p.Veto} }
	if !all[0].Legacy || all[1].Legacy || all[2].Legacy || all[3].Legacy {
		t.Fatalf("legacy flags %v %v %v %v", all[0].Legacy, all[1].Legacy, all[2].Legacy, all[3].Legacy)
	}
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			a, b := perKey(all[i]), perKey(all[j])
			for field, v := range a {
				if v == b[field] {
					t.Errorf("%s and %s share %s %q", sockets[i], sockets[j], field, v)
				}
			}
			// no per-key directory of one server lies inside the other's state
			for _, d := range []string{"State", "Snapshots", "Launch", "Pending", "Panes"} {
				if strings.HasPrefix(b[d]+"/", a["State"]+"/") || strings.HasPrefix(a[d]+"/", b["State"]+"/") {
					t.Errorf("%s of one server is inside the other's state dir (%q, %q)", d, a[d], b[d])
				}
			}
			if shared(all[i]) != shared(all[j]) {
				t.Errorf("the shared locations differ: %v vs %v", shared(all[i]), shared(all[j]))
			}
		}
	}
}

func TestKey_MacPrivateTmpIsDefault(t *testing.T) {
	check := func(t *testing.T, link, real string) {
		t.Helper()
		for _, spelling := range []string{
			filepath.Join(link, uidDir(), "default"),
			filepath.Join(real, uidDir(), "default"),
			filepath.Join(link, uidDir(), ".", "default"),
		} {
			if !Legacy(spelling) {
				t.Errorf("Legacy(%q) = false", spelling)
			}
			if k := Key(spelling); k != LegacyKey {
				t.Errorf("Key(%q) = %q, want default", spelling, k)
			}
		}
		if Legacy(filepath.Join(link, uidDir(), "work")) {
			t.Error("a non-default name in tmux's dir is legacy")
		}
	}

	t.Run("tmux dir absent", func(t *testing.T) {
		link, real := scratchTmp(t)
		if _, err := os.Lstat(filepath.Join(real, uidDir())); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("precondition: %s exists", uidDir())
		}
		check(t, link, real)
	})
	t.Run("tmux dir present", func(t *testing.T) {
		link, real := scratchTmp(t)
		if err := os.Mkdir(filepath.Join(real, uidDir()), 0o700); err != nil {
			t.Fatal(err)
		}
		check(t, link, real)
	})
	t.Run("real /tmp, read only", func(t *testing.T) {
		// Whatever state the real /tmp/tmux-<uid> is in (present on a machine
		// with a server, absent on fresh CI), both spellings agree. Only
		// lstat/readlink happen here.
		realTmp, err := filepath.EvalSymlinks("/tmp")
		if err != nil {
			t.Skip("no /tmp")
		}
		a := filepath.Join("/tmp", uidDir(), "default")
		b := filepath.Join(realTmp, uidDir(), "default")
		if !Legacy(a) || !Legacy(b) || Key(a) != LegacyKey || Key(b) != LegacyKey {
			t.Fatalf("Legacy(%s)=%v Legacy(%s)=%v", a, Legacy(a), b, Legacy(b))
		}
	})
}

var keyShape = regexp.MustCompile(`^[^@/]+@[0-9a-f]{8}$`)

func TestKey_SameBasenameDifferentDir(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a", "work")
	b := filepath.Join(root, "b", "work")
	ka, kb := Key(a), Key(b)
	if ka == kb {
		t.Fatalf("two sockets named work in different dirs share key %q", ka)
	}
	for _, k := range []string{ka, kb} {
		if !keyShape.MatchString(k) || !strings.HasPrefix(k, "work@") {
			t.Errorf("key %q is not work@hex8", k)
		}
	}
	if Key(a) != ka {
		t.Fatal("Key is not deterministic")
	}
}

func TestKey_TmuxTmpdirScratchNamedDefaultIsNotLegacy(t *testing.T) {
	link, _ := scratchTmp(t)
	_ = link
	scratch := t.TempDir()
	for _, s := range []string{
		filepath.Join(scratch, uidDir(), "default"), // what -L default gives under TMUX_TMPDIR=<scratch>
		filepath.Join(scratch, "default"),           // -S <scratch>/default
	} {
		if Legacy(s) {
			t.Errorf("Legacy(%q) = true", s)
		}
		if k := Key(s); k == LegacyKey || !strings.HasPrefix(k, "default@") {
			t.Errorf("Key(%q) = %q, want default@hex8", s, k)
		}
	}
}

func TestKey_SymlinkSpellingsAgree(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	cases := [][2]string{
		{filepath.Join(real, "s"), filepath.Join(alias, "s")},
		// a not-yet-existing directory below the symlink: deepest existing
		// ancestor is resolved, the remainder appended (R2-M7)
		{filepath.Join(real, "later", "tmux-1", "s"), filepath.Join(alias, "later", "tmux-1", "s")},
	}
	for _, c := range cases {
		ca, err := Canonical(c[0])
		if err != nil {
			t.Fatal(err)
		}
		cb, err := Canonical(c[1])
		if err != nil {
			t.Fatal(err)
		}
		if ca != cb || ca != c[0] {
			t.Errorf("Canonical(%q)=%q Canonical(%q)=%q", c[0], ca, c[1], cb)
		}
		if Key(c[0]) != Key(c[1]) {
			t.Errorf("keys differ for %q and %q", c[0], c[1])
		}
	}
}

func TestCanonical_KeepsSocketBasename(t *testing.T) {
	// A socket file that is itself a symlink is not followed: the key is the
	// name tmux was given.
	root, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.Symlink("/nonexistent", filepath.Join(root, "s")); err != nil {
		t.Fatal(err)
	}
	c, err := Canonical(filepath.Join(root, "s"))
	if err != nil || c != filepath.Join(root, "s") {
		t.Fatalf("Canonical = %q, %v", c, err)
	}
	if _, err := Canonical(""); err == nil {
		t.Fatal("empty path accepted")
	}
}

func TestMatch(t *testing.T) {
	cases := []struct {
		name, setting string
		want          bool
	}{
		{"default", "", true},
		{"default", "   ", true},
		{"work", "", false},
		{"default", "default", true},
		{"work", "default work", true},
		{"work", "default\twork\n", true},
		{"moox-val-a", "moox-val-*", true},
		{"moox-val-a", "moox-val-?", true},
		{"moox-val-ab", "moox-val-?", false},
		{"anything", "*", true},
		{"a", "[", false},              // malformed: matches nothing
		{"a", "[ a", true},             // a malformed word does not poison the others
		{"defaults", "default", false}, // whole-name match only
	}
	for _, c := range cases {
		if got := Match(c.name, c.setting); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.name, c.setting, got, c.want)
		}
	}
}

func TestLint(t *testing.T) {
	if errs := Lint("default work moox-* *"); len(errs) != 0 {
		t.Fatalf("valid setting linted: %v", errs)
	}
	if errs := Lint("ok [ also\\"); len(errs) != 2 {
		t.Fatalf("Lint found %d errors, want 2: %v", len(errs), errs)
	}
}

func TestName(t *testing.T) {
	if n := Name("/private/tmp/tmux-501/work"); n != "work" {
		t.Fatalf("Name = %q", n)
	}
}

type mapEnv map[string]string

func (m mapEnv) Getenv(k string) string { return m[k] }

func TestFor_LegacyAndKeyedLayouts(t *testing.T) {
	link, _ := scratchTmp(t)
	env := mapEnv{"HOME": "/h"}

	p, err := For(filepath.Join(link, uidDir(), "default"), env)
	if err != nil {
		t.Fatal(err)
	}
	want := Paths{
		Socket:      filepath.Join(link, uidDir(), "default"),
		Key:         "default",
		Legacy:      true,
		Snapshots:   "/h/.local/share/tmux/resurrect",
		Launch:      "/h/.config/tmux-claude/launch",
		Pending:     "/h/.config/tmux-claude/pending",
		Panes:       "/h/.config/tmux-claude/panes",
		ByPidDir:    "/h/.config/tmux-claude/panes/by-pid",
		FreezeDir:   "/h/.config/tmux-cc/frozen/default",
		State:       "/h/.local/state/moox/sockets/default",
		Log:         "/h/.local/state/moox/sockets/default/log",
		Lock:        "/h/.local/state/moox/sockets/default/save.lock",
		Marker:      "/h/.local/state/moox/sockets/default/active",
		TTState:     "/h/.local/state/tt",
		TTPanelConf: "/h/.config/tt/panel.conf",
		UserConfig:  "/h/.config/moox",
		Veto:        "/h/.config/tmux-claude/moox-excluded",
	}
	if p != want {
		t.Fatalf("legacy layout\n got  %+v\n want %+v", p, want)
	}

	env = mapEnv{"HOME": "/h", "XDG_STATE_HOME": "/xs", "XDG_DATA_HOME": "/xd", "XDG_CONFIG_HOME": "relative/ignored"}
	sock := filepath.Join(t.TempDir(), "work")
	p, err = For(sock, env)
	if err != nil {
		t.Fatal(err)
	}
	k := Key(sock)
	checks := map[string]string{
		p.Snapshots:   "/xs/moox/sockets/" + k + "/snapshots",
		p.Launch:      "/xs/moox/sockets/" + k + "/launch",
		p.Pending:     "/xs/moox/sockets/" + k + "/pending",
		p.Panes:       "/xs/moox/sockets/" + k + "/panes",
		p.ByPidDir:    "/h/.config/tmux-claude/panes/by-pid", // shared (R3-C2)
		p.FreezeDir:   "/h/.config/tmux-cc/frozen/" + k,
		p.State:       "/xs/moox/sockets/" + k,
		p.TTState:     "/xs/tt/sockets/" + k,
		p.TTPanelConf: "/h/.config/tt/panel.conf", // relative XDG_CONFIG_HOME ignored
		p.UserConfig:  "/h/.config/moox",
	}
	for got, want := range checks {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if p.Legacy || p.Key != k {
		t.Errorf("Key/Legacy = %q/%v", p.Key, p.Legacy)
	}
	if p.ByPid("") != p.ByPidDir || p.ByPid("/opt") != "/opt" {
		t.Error("ByPid does not apply the option rule")
	}

	if _, err := For(sock, mapEnv{}); !errors.Is(err, ErrNoHome) {
		t.Fatalf("no HOME: %v", err)
	}
	if _, err := For(sock, mapEnv{"HOME": "rel"}); !errors.Is(err, ErrNoHome) {
		t.Fatalf("relative HOME: %v", err)
	}
}
