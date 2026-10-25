//go:build realdata

package engine

// V4 (architecture §12.3): import the live resurrect snapshot — from a COPY.
//
//	MOOX_V4_REPORT=<file> go test -tags realdata -run TestRealData_V4 -v ./internal/engine/
//
// The real files are only READ, once, to copy them: resurrect's `last`
// target and pane_contents.tar.gz (MOOX_V4_SOURCE, default
// ~/.local/share/tmux/resurrect) and the launch recordings the snapshot's
// panes need (MOOX_V4_LAUNCH, default ~/.config/tmux-claude/launch). Every
// import, write and publish works on the copy in t.TempDir(), under a
// scratch HOME/XDG tree. No tmux command is run: the live server is the
// user's default server, so --attach-live is exercised with a runner that
// answers list-panes from the pane ids the snapshot's own layouts recorded
// at save time.
//
// The report holds counts and redacted shapes only: no session, window or
// pane names, titles, paths, commands or contents.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(to, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
}

type layoutLive struct{ out string }

func (l layoutLive) Run(_ context.Context, args ...string) ([]byte, error) {
	if len(args) == 0 || args[0] != "list-panes" {
		return nil, fmt.Errorf("V4 fake live: unexpected %v", args)
	}
	return []byte(l.out), nil
}

var paneIDRe = regexp.MustCompile(`^%[0-9]+$`)

// sourceCounts are counted from the copy without the importer: the rows a
// heal verdict (H2) would count, keyed on the first field.
type sourceCounts struct {
	panes, windows, sessions, continuations int
	fieldCounts                             map[string]int // "pane:13" → n
}

func countSource(data []byte) sourceCounts {
	c := sourceCounts{fieldCounts: map[string]int{}}
	sess := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		switch f[0] {
		case "pane":
			if len(f) >= 11 {
				c.panes++
				c.fieldCounts[fmt.Sprintf("pane:%d", len(f))]++
				continue
			}
		case "window":
			if len(f) == 8 {
				c.windows++
				sess[f[1]] = true
				c.fieldCounts["window:8"]++
				continue
			}
		case "state":
			c.fieldCounts[fmt.Sprintf("state:%d", len(f))]++
			continue
		case "grouped_session":
			c.fieldCounts["grouped_session"]++
			continue
		}
		c.continuations++
	}
	c.sessions = len(sess)
	return c
}

// expectedCwd is the cwd a row carries, by the simplest independent rule:
// the first of fields 7–8 (1-based) that is a ":/" or ":~" path.
func expectedCwds(data []byte, home string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Split(line, "\t")
		if f[0] != "pane" || len(f) < 11 {
			continue
		}
		key := f[1] + ":" + f[2] + "." + f[5]
		for _, v := range f[6:8] {
			if strings.HasPrefix(v, ":/") || strings.HasPrefix(v, ":~") {
				p := strings.ReplaceAll(v[1:], `\ `, " ")
				if p == "~" {
					p = home
				} else if strings.HasPrefix(p, "~/") {
					p = home + p[1:]
				}
				out[key] = p
				break
			}
		}
	}
	return out
}

func kind(w string) string {
	k, _, _ := strings.Cut(w, " ")
	return strings.TrimSuffix(k, ":")
}

func TestRealData_V4_ImportLiveSnapshot(t *testing.T) {
	realHome, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	srcDir := os.Getenv("MOOX_V4_SOURCE")
	if srcDir == "" {
		srcDir = filepath.Join(realHome, ".local", "share", "tmux", "resurrect")
	}
	launchDir := os.Getenv("MOOX_V4_LAUNCH")
	if launchDir == "" {
		launchDir = filepath.Join(realHome, ".config", "tmux-claude", "launch")
	}
	ln, err := os.Readlink(filepath.Join(srcDir, "last"))
	if err != nil {
		t.Skipf("no resurrect last to copy: %v", err)
	}
	target := ln
	if !filepath.IsAbs(target) {
		target = filepath.Join(srcDir, target)
	}

	// ---- copy (never move) ------------------------------------------------
	work := t.TempDir()
	cp := filepath.Join(work, "resurrect")
	rec := filepath.Join(work, "launch")
	for _, d := range []string{cp, rec} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	base := filepath.Base(target)
	copyFile(t, target, filepath.Join(cp, base))
	if err := os.Symlink(base, filepath.Join(cp, "last")); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(srcDir, "pane_contents.tar.gz")
	haveArchive := false
	if _, err := os.Stat(archive); err == nil {
		copyFile(t, archive, filepath.Join(cp, "pane_contents.tar.gz"))
		haveArchive = true
	}
	srcData, err := os.ReadFile(filepath.Join(cp, base))
	if err != nil {
		t.Fatal(err)
	}
	src0, err := os.Stat(filepath.Join(cp, base))
	if err != nil {
		t.Fatal(err)
	}

	// ---- import 1: the file alone (the pane ids come from its layouts) ----
	ctx := context.Background()
	first, err := Import(ctx, ImportSource{From: cp, Home: realHome, Producer: "moox v4"}, nil)
	if err != nil {
		t.Fatalf("import of the copy: %v", err)
	}
	var liveRows []string
	layoutIDs := 0
	for _, p := range first.Snapshot.Panes {
		if !paneIDRe.MatchString(p.SavedID) {
			continue
		}
		layoutIDs++
		liveRows = append(liveRows, fmt.Sprintf("%s\x1f%d\x1f%d\x1f%s", p.Session, p.Window, p.Index, p.SavedID))
		n := p.SavedID[1:]
		if from := filepath.Join(launchDir, n); fileExists(from) {
			copyFile(t, from, filepath.Join(rec, n))
		}
	}
	copied, _ := os.ReadDir(rec)

	// ---- import 2: with the recordings attached ---------------------------
	im, err := Import(ctx, ImportSource{From: cp, Home: realHome, Producer: "moox v4", AttachLive: true, Recordings: rec},
		layoutLive{out: strings.Join(liveRows, "\n") + "\n"})
	if err != nil {
		t.Fatalf("import with recordings: %v", err)
	}
	s, rep := im.Snapshot, im.Report
	if err := snapshot.Validate(s); err != nil {
		t.Fatal(err)
	}

	// ---- compare with the source -------------------------------------------
	want := countSource(srcData)
	var fails []string
	check := func(name string, got, exp int) {
		if got != exp {
			fails = append(fails, fmt.Sprintf("%s: imported %d, source %d", name, got, exp))
		}
	}
	check("panes", rep.Panes+rep.Dropped, want.panes)
	check("windows", rep.Windows, want.windows)
	check("sessions", rep.Sessions, want.sessions)
	check("continuations", rep.Continuations, want.continuations)
	check("snapshot panes", len(s.Panes), rep.Panes)
	cwds := expectedCwds(srcData, realHome)
	cwdMismatch := 0
	for _, p := range s.Panes {
		if exp, ok := cwds[p.Key]; ok && exp != p.Cwd {
			cwdMismatch++
		}
	}
	if cwdMismatch > rep.HealedTitles {
		fails = append(fails, fmt.Sprintf("cwd: %d panes differ from the source (healed titles %d)", cwdMismatch, rep.HealedTitles))
	}

	// ---- write + publish on scratch ----------------------------------------
	out := filepath.Join(work, "out", "imported.json")
	wrote, err := WriteImport(im, out, filepath.Join(work, "home", ".claude"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(out)
	back, err := snapshot.Decode(b)
	if err != nil || snapshot.Validate(back) != nil || len(back.Panes) != len(s.Panes) {
		t.Fatalf("written snapshot does not read back: %v", err)
	}
	root := filepath.Join(work, "scratch")
	env := tmuxtarget.MapEnv{"HOME": filepath.Join(root, "home"), "XDG_STATE_HOME": filepath.Join(root, "xs"),
		"XDG_DATA_HOME": filepath.Join(root, "xd"), "XDG_CONFIG_HOME": filepath.Join(root, "xc")}
	e, err := New(Options{Target: tmuxtarget.Target{Bin: "/nonexistent/tmux", Socket: filepath.Join(root, "moox-val-import")},
		Env: env, Producer: "moox v4"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e.Paths().Snapshots, root) {
		t.Fatalf("publish target escapes the scratch tree: %s", e.Paths().Snapshots)
	}
	pub, err := e.PublishImport(ctx, im, PublishImportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	last, err := e.Last()
	if err != nil || last.Name != pub.File || len(last.Snapshot.Panes) != len(s.Panes) {
		t.Fatalf("Last after publish: %v", err)
	}
	if haveArchive && rep.ContentsUsed {
		entries, err := func() (map[string][]byte, error) {
			cb, err := os.ReadFile(filepath.Join(e.Paths().Snapshots, last.Snapshot.Contents.File))
			if err != nil {
				return nil, err
			}
			if snapshot.SHA256(cb) != last.Snapshot.Contents.SHA256 {
				return nil, fmt.Errorf("sha256 mismatch")
			}
			_, m, err := snapshot.DecodeContents(cb)
			return m, err
		}()
		if err != nil || len(entries) != rep.ContentsPanes {
			fails = append(fails, fmt.Sprintf("published contents: %d entries, %v", len(entries), err))
		}
	}

	// ---- the copy and the real source are untouched -----------------------
	src1, _ := os.Stat(filepath.Join(cp, base))
	if !src1.ModTime().Equal(src0.ModTime()) || src1.Size() != src0.Size() {
		fails = append(fails, "the copied source changed")
	}
	if ln2, _ := os.Readlink(filepath.Join(srcDir, "last")); ln2 == "" {
		fails = append(fails, "resurrect's real last vanished during the run")
	}

	// ---- report ------------------------------------------------------------
	roles := map[string]int{}
	launches, claudish := 0, 0
	for _, p := range s.Panes {
		roles[p.Role]++
		if p.Launch != "" {
			launches++
		}
		if p.Claude != nil && p.Claude.Launcher == "claudish" {
			claudish++
		}
	}
	formats := map[string]int{}
	sized := 0
	for _, w := range s.Windows {
		formats[w.LayoutFormat]++
		if w.Width > 0 && w.Height > 0 {
			sized++
		}
	}
	kinds := map[string]int{}
	for _, w := range rep.Warnings {
		kinds[kind(w)]++
	}
	verdict := "PASS"
	if len(fails) > 0 {
		verdict = "FAIL"
	}

	var r strings.Builder
	fmt.Fprintf(&r, "# V4 — import of the live resurrect snapshot (real-data copy)\n\n")
	fmt.Fprintf(&r, "Run: %s · `go test -tags realdata -run TestRealData_V4_ImportLiveSnapshot ./internal/engine/`\n\n",
		time.Now().UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&r, "**Verdict: %s**\n\n", verdict)
	fmt.Fprintf(&r, "Method: resurrect's `last` target, `pane_contents.tar.gz` and the %d launch recordings present for its panes were "+
		"COPIED (read once, never moved, mtimes preserved) into a temp dir; the import, `--out` write and publish ran on the copy "+
		"under a scratch HOME/XDG tree (socket `moox-val-import`). No tmux command ran: `--attach-live` used a runner answering "+
		"list-panes from the pane ids the snapshot's own layouts recorded. Restore (stubbed, on an isolated server) is P4b's half of V4.\n\n",
		len(copied))
	fmt.Fprintf(&r, "Source: `%s` (%d bytes, mtime %s); contents archive copied: %v\n\n", redactName(base), len(srcData),
		src0.ModTime().UTC().Format("2006-01-02 15:04 UTC"), haveArchive)
	fmt.Fprintf(&r, "| Count | Source (independent count) | Imported |\n|---|---|---|\n")
	fmt.Fprintf(&r, "| pane rows | %d | %d (+%d dropped) |\n", want.panes, rep.Panes, rep.Dropped)
	fmt.Fprintf(&r, "| windows | %d | %d |\n", want.windows, rep.Windows)
	fmt.Fprintf(&r, "| sessions | %d | %d |\n", want.sessions, rep.Sessions)
	fmt.Fprintf(&r, "| continuation lines | %d | %d (attached to the preceding pane's full_command) |\n", want.continuations, rep.Continuations)
	fmt.Fprintf(&r, "| cwd equal to the source row | %d | %d (%d differ, %d healed titles) |\n\n", len(cwds),
		len(s.Panes)-cwdMismatch, cwdMismatch, rep.HealedTitles)
	fmt.Fprintf(&r, "Row shapes (field counts): %s\n\n", histogram(want.fieldCounts))
	fmt.Fprintf(&r, "| Report field | Value |\n|---|---|\n")
	for _, kv := range []struct {
		k string
		v any
	}{{"rows", rep.Rows}, {"sids", rep.Sids}, {"realigned (empty title, 12 fields)", rep.Realigned},
		{"collapsed (never realigned)", rep.Collapsed}, {"healed titles", rep.HealedTitles},
		{"launch lines (;CLAUDE_CMD= + recordings)", launches}, {"recordings attached live (rows without ;CLAUDE_CMD=)", rep.Recordings},
		{"claudish launchers", claudish}, {"contents used", rep.ContentsUsed}, {"contents panes", rep.ContentsPanes},
		{"saved_id from layout leaves", layoutIDs}, {"saved_id synthetic", len(s.Panes) - layoutIDs},
		{"windows sized from layout", fmt.Sprintf("%d/%d", sized, len(s.Windows))},
		{"layout formats", histogram(formats)}, {"roles", histogram(roles)}, {"warnings", len(rep.Warnings)}} {
		fmt.Fprintf(&r, "| %s | %v |\n", kv.k, kv.v)
	}
	fmt.Fprintf(&r, "\nWarnings by kind (details omitted: they name sessions and paths): %s\n\n", histogram(kinds))
	fmt.Fprintf(&r, "Written: `--out` %d files, read back with Decode + Validate; published `moox-<ts>.json` on scratch, "+
		"`Last()` returned it with %d panes; contents sha256 verified.\n", len(wrote), len(last.Snapshot.Panes))
	if len(fails) > 0 {
		fmt.Fprintf(&r, "\nFailures:\n")
		for _, f := range fails {
			fmt.Fprintf(&r, "- %s\n", f)
		}
	}
	text := r.String()
	t.Log("\n" + text)
	if p := os.Getenv("MOOX_V4_REPORT"); p != "" {
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range fails {
		t.Error(f)
	}
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// redactName keeps a resurrect file name's shape (its timestamp is not
// personal) and nothing else.
func redactName(n string) string {
	if regexp.MustCompile(`^tmux_resurrect_\d{8}T\d{6}\.txt$`).MatchString(n) {
		return "tmux_resurrect_<ts>.txt"
	}
	return "<file>"
}

func histogram(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
