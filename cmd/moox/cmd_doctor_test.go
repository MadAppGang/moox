package main

// moox doctor (§8.4, D9) on isolated servers (tmuxtest.Start: -f /dev/null
// -L moox-test-*, scratch HOME/XDG) and fake HOME trees. The verb runs in
// process with the server's scratch environment (TMUX_TMPDIR empty scratch).

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/tmuxtest"
)

func runIn(t *testing.T, environ []string, args ...string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	m := newMeta(context.Background(), environ)
	m.stdin, m.stdout, m.stderr = strings.NewReader(""), &out, &out
	m.stdinTTY, m.stdoutTTY = false, false
	code := run(m, args)
	return code, out.String()
}

func TestDoctor_LegacyHooksKeysOptions(t *testing.T) {
	s := tmuxtest.Start(t)
	s.Run(t, "set-hook", "-g", "session-created[3]", `run-shell -b "~/.tmux/plugins/tmux-resurrect/scripts/save.sh"`)
	s.Run(t, "set-hook", "-g", "client-attached[7]", `run-shell -b "/x/tmux-continuum/scripts/continuum_save.sh"`)
	s.Run(t, "set-hook", "-g", "session-created[101]", `wait-for -S moox-dirty`)
	s.Run(t, "bind-key", "C-r", "run-shell /x/tmux-resurrect/scripts/restore.sh")
	s.Run(t, "bind-key", "-n", "M-y", "run-shell /x/tmux-continuum/x.sh")
	s.Run(t, "bind-key", "C-g", "display-message", "keep me")
	s.Run(t, "set-option", "-g", "@resurrect-dir", "/tmp/x")
	s.Run(t, "set-option", "-g", "@continuum-restore", "on")
	s.Run(t, "set-option", "-g", "@keep-me", "1")
	env := s.Env()

	_, out := runIn(t, env, "doctor", "-S", s.T.Socket)
	for _, want := range []string{"legacy-hook", "session-created[3]", "client-attached[7]", "legacy-key", "-T prefix C-r",
		"-T root M-y", "legacy-option", "@resurrect-dir", "@continuum-restore", "moox doctor --fix legacy-hooks"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q", want)
		}
	}
	if strings.Contains(out, "session-created[101]") || strings.Contains(out, "C-g") || strings.Contains(out, "@keep-me") {
		t.Errorf("doctor flags non-legacy entries:\n%s", out)
	}
	if t.Failed() {
		t.Fatalf("output:\n%s", out)
	}

	code, out := runIn(t, env, "doctor", "-S", s.T.Socket, "--fix", "legacy-hooks", "--dry-run")
	if code != 0 || strings.Count(out, "would ") != 6 {
		t.Fatalf("dry run: exit %d\n%s", code, out)
	}
	if !strings.Contains(s.Run(t, "show-hooks", "-g"), "session-created[3]") {
		t.Fatal("--dry-run unset a hook")
	}
	code, out = runIn(t, env, "doctor", "-S", s.T.Socket, "--fix", "legacy-hooks")
	if code != 0 {
		t.Fatalf("fix: exit %d\n%s", code, out)
	}
	hooks := s.Run(t, "show-hooks", "-g")
	keys := s.Run(t, "list-keys")
	if strings.Contains(hooks, "resurrect") || strings.Contains(hooks, "continuum") || !strings.Contains(hooks, "session-created[101]") {
		t.Fatalf("hooks after the fix:\n%s", hooks)
	}
	if strings.Contains(keys, "resurrect") || strings.Contains(keys, "continuum") || !strings.Contains(keys, "keep me") {
		t.Fatalf("keys after the fix:\n%s", keys)
	}
	if s.Opt(t, "@resurrect-dir") != "" || s.Opt(t, "@continuum-restore") != "" || s.Opt(t, "@keep-me") != "1" {
		t.Fatal("options after the fix are wrong")
	}
	_, out = runIn(t, env, "doctor", "-S", s.T.Socket)
	if !strings.Contains(out, "no resurrect/continuum hooks, keys or options") {
		t.Fatalf("still legacy after the fix:\n%s", out)
	}
}

func TestDoctor_StaleSocketDirs(t *testing.T) {
	s := tmuxtest.Start(t)
	env := s.Env()
	old := time.Now().Add(-30 * 24 * time.Hour)
	mk := func(rel string, mtime time.Time) string {
		p := filepath.Join(s.XS, rel)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		f := filepath.Join(p, "daemon.log")
		if err := os.WriteFile(f, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{f, p} {
			if err := os.Chtimes(q, mtime, mtime); err != nil {
				t.Fatal(err)
			}
		}
		return p
	}
	stale1 := mk("moox/sockets/gone@0000aaaa", old)
	stale2 := mk("tt/sockets/gone@0000aaaa", old)
	fresh := mk("moox/sockets/fresh@0000bbbb", time.Now())
	legacy := mk("moox/sockets/default", old)
	live := mk("moox/sockets/"+s.Paths(t).Key, old)

	_, out := runIn(t, env, "doctor", "-S", s.T.Socket)
	for _, p := range []string{stale1, stale2} {
		if !strings.Contains(out, p+": no live socket") {
			t.Errorf("not reported stale: %s\n%s", p, out)
		}
	}
	for _, p := range []string{fresh, legacy, live} {
		if strings.Contains(out, p+":") {
			t.Errorf("reported stale but is not: %s\n%s", p, out)
		}
	}
	code, out := runIn(t, env, "doctor", "-S", s.T.Socket, "--fix", "stale")
	if code != 0 {
		t.Fatalf("--fix stale: %d\n%s", code, out)
	}
	for _, p := range []string{stale1, stale2} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s survived --fix stale", p)
		}
	}
	for _, p := range []string{fresh, legacy, live} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("--fix stale removed %s", p)
		}
	}
}

func TestDoctor_Restoring(t *testing.T) {
	s := tmuxtest.Start(t)
	env := s.Env()
	s.Run(t, "set-option", "-g", "@moox-restoring", "garbage")
	code, out := runIn(t, env, "doctor", "-S", s.T.Socket)
	if code != exitError || !strings.Contains(out, `@moox-restoring "garbage" does not parse`) ||
		!strings.Contains(out, "moox doctor --fix restoring") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if code, out = runIn(t, env, "doctor", "-S", s.T.Socket, "--fix", "restoring"); code != 0 || s.Opt(t, "@moox-restoring") != "" {
		t.Fatalf("--fix restoring: exit %d, option %q\n%s", code, s.Opt(t, "@moox-restoring"), out)
	}
	// A live owner is left alone.
	live := "restore:" + strconv.Itoa(os.Getpid()) + ":1"
	s.Run(t, "set-option", "-g", "@moox-restoring", live)
	_, out = runIn(t, env, "doctor", "-S", s.T.Socket, "--fix", "restoring")
	if s.Opt(t, "@moox-restoring") != live || !strings.Contains(out, "left alone") {
		t.Fatalf("a live @moox-restoring was cleared:\n%s", out)
	}
	// A dead owner is a warning, and fixable.
	s.Run(t, "set-option", "-g", "@moox-restoring", "boot:999999:1")
	if _, out = runIn(t, env, "doctor", "-S", s.T.Socket); !strings.Contains(out, "pid 999999, which is gone") {
		t.Fatalf("dead owner not reported:\n%s", out)
	}
}

func TestDoctor_GateDaemonAndPins(t *testing.T) {
	s := tmuxtest.Start(t)
	env := s.Env()
	_, out := runIn(t, env, "doctor", "-S", s.T.Socket)
	if !strings.Contains(out, "@moox-active is unset: the gate") {
		t.Fatalf("gate-never-ran not reported:\n%s", out)
	}
	s.Run(t, "set-option", "-g", "@moox-active", "1")
	p := s.Paths(t)
	if err := os.MkdirAll(p.State, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.State, "pins"), []byte("moox-20260101T000000.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.State, "restoring-from"), []byte("moox-20260102T000000.json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runIn(t, env, "doctor", "-S", s.T.Socket)
	pin := filepath.Join(p.Snapshots, "moox-20260101T000000.json")
	for _, want := range []string{"no saver.state: the daemon is not running", "pinned after a non-PASS restore: " + pin,
		"moox restart --snapshot " + pin, "moox snapshot unpin moox-20260101T000000.json", "leftover restoring-from",
		"@moox-active is 1 but"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor lacks %q", want)
		}
	}
	if code != exitError {
		t.Errorf("exit %d with a FAIL result", code)
	}
	if t.Failed() {
		t.Logf("output:\n%s", out)
	}
}

func TestDoctor_PathTools(t *testing.T) {
	s := tmuxtest.Start(t)
	bin := filepath.Join(s.Root, "tools")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	script := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	env := func() []string {
		e := s.Env()
		for i, kv := range e {
			if strings.HasPrefix(kv, "PATH=") {
				e[i] = "PATH=" + bin + ":/bin" // no /usr/bin: macOS ships jq there
			}
		}
		return e
	}
	script("tt", `echo "tt version 2.4.0, darwin/arm64"`) // Tarantool's tt
	_, out := runIn(t, env(), "doctor", "-S", s.T.Socket)
	if !strings.Contains(out, "jq: not on PATH") || !strings.Contains(out, "is not moox's tt") {
		t.Fatalf("jq / Tarantool tt not reported:\n%s", out)
	}
	script("jq", "exit 0")
	script("tt", `echo "tt 0.0.0-dev (moox)"`)
	local := filepath.Join(s.Home, ".local", "bin")
	if err := os.MkdirAll(local, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "tt"), []byte("#!/bin/sh\necho 'tt 0.0.9 (moox)'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	_, out = runIn(t, env(), "doctor", "-S", s.T.Socket)
	if strings.Contains(out, "jq: not on PATH") || strings.Contains(out, "is not moox's tt") {
		t.Fatalf("false positives:\n%s", out)
	}
	if !strings.Contains(out, filepath.Join(local, "tt")+" is a different tt build") {
		t.Fatalf("~/.local/bin/tt skew not reported:\n%s", out)
	}
}

// TestDoctor_NoResurrectChecks (I11): doctor does not read resurrect's
// directory or its `last`. A broken resurrect tree (a dangling `last`, a
// garbage snapshot) under both the XDG and the literal ~/.local/share paths
// changes no result; resurrect is named only by the legacy check, and only
// for a configured @resurrect-* option.
func TestDoctor_NoResurrectChecks(t *testing.T) {
	s := tmuxtest.Start(t)
	env := s.Env()
	results := func() []map[string]string {
		t.Helper()
		_, out := runIn(t, env, "doctor", "-S", s.T.Socket, "--json")
		var doc struct{ Results []map[string]string }
		if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Results) == 0 {
			t.Fatalf("--json does not parse (%v):\n%s", err, out)
		}
		return doc.Results
	}
	summary := func(rs []map[string]string) string {
		var rows []string
		for _, r := range rs {
			rows = append(rows, r["check"]+" "+r["level"]+" "+r["message"])
		}
		return strings.Join(rows, "\n")
	}
	before := results()

	broken := filepath.Join(s.Root, "resurrect-broken")
	for _, dir := range []string{filepath.Join(s.XD, "tmux", "resurrect"), filepath.Join(s.Home, ".local", "share", "tmux", "resurrect"), broken} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "tmux_resurrect_20260101T000000.txt"), []byte("garbage\tnot\ta\tsnapshot\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("tmux_resurrect_20991231T235959.txt", filepath.Join(dir, "last")); err != nil {
			t.Fatal(err)
		}
	}
	after := results()
	if summary(after) != summary(before) {
		t.Fatalf("a broken resurrect tree changed doctor's results\nbefore:\n%s\nafter:\n%s", summary(before), summary(after))
	}
	// the legacy checks look for resurrect's hooks, keys and options in the
	// server's own config; nothing else may name it
	isLegacy := func(r map[string]string) bool {
		return r["check"] == "legacy" || strings.HasPrefix(r["check"], "legacy-")
	}
	for _, r := range after {
		if strings.Contains(strings.ToLower(r["message"]+r["fix"]), "resurrect") && !isLegacy(r) {
			t.Errorf("doctor reports on resurrect: %v", r)
		}
	}

	s.Run(t, "set-option", "-g", "@resurrect-dir", broken)
	flagged := false
	for _, r := range results() {
		named := strings.Contains(strings.ToLower(r["message"]+r["fix"]), "resurrect")
		if named && !isLegacy(r) {
			t.Errorf("only the legacy checks may name resurrect: %v", r)
		}
		flagged = flagged || (r["check"] == "legacy-option" && strings.Contains(r["message"], "@resurrect-dir"))
	}
	if !flagged {
		t.Error("the configured @resurrect-dir is not flagged as a legacy option")
	}
}

func TestDoctor_JSON(t *testing.T) {
	s := tmuxtest.Start(t)
	_, out := runIn(t, s.Env(), "doctor", "-S", s.T.Socket, "--json")
	var doc struct {
		Version string
		Results []map[string]string
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Results) == 0 {
		t.Fatalf("--json does not parse (%v):\n%s", err, out)
	}
	for _, r := range doc.Results {
		for _, k := range []string{"check", "level", "message", "fix", "server"} {
			if _, ok := r[k]; !ok {
				t.Fatalf("result lacks %q: %v", k, r)
			}
		}
	}
}
