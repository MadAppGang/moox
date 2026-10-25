package importer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/layout"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

// Fixtures are synthetic: every row is built from the shapes
// research/resurrect-behaviours.md §1 found in the live `last` (field counts
// and sentinels), never copied from it.

const home = "/home/user"

func row(f ...string) string { return strings.Join(f, "\t") }

// v2 layouts: one pane, and two panes side by side (ids %7, %9).
const (
	lay1 = `{"V":2,"L":{"t":"p","w":167,"h":51,"x":0,"y":0,"a":true,"i":1,"I":"%3"}}`
	lay2 = `{"V":2,"L":{"t":"h","w":203,"h":56,"x":0,"y":0,"c":[{"t":"p","w":101,"h":56,"x":0,"y":0,"a":true,"i":1,"I":"%7"},{"t":"p","w":101,"h":56,"x":102,"y":0,"l":0,"i":2,"I":"%9"}]}}`
)

// v1 is a classic layout with a correct checksum.
func v1(body string) string { return fmt.Sprintf("%04x,%s", layout.Checksum(body), body) }

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func lines(rows ...string) string { return strings.Join(rows, "\n") + "\n" }

// basic is a normal two-session workspace.
func basic() string {
	return lines(
		row("pane", "api", "1", "1", ":*", "1", "✳ Claude Code", ":/home/user/dev/api", "1", "2.1.309",
			":claude --dangerously-skip-permissions ", ";CLAUDE_SID=0b9f-uuid", ";CLAUDE_CMD="+b64("c --worktree foo")),
		row("pane", "api", "2", "0", ":-", "1", "server", ":/home/user/dev/api", "0", "zsh", ":"),
		row("pane", "api", "2", "0", ":-", "2", "logs", ":/home/user/dev/api/logs", "1", "tail", ":tail -f x.log",
			";CLAUDE_CMD="+b64("tail -f x.log")),
		row("pane", "notes", "1", "1", ":*", "1", "host", ":~/notes", "1", "zsh", ":"),
		row("window", "api", "1", ":claude", "1", ":*", lay1, "off"),
		row("window", "api", "2", ":dev", "0", ":-", lay2, ":"),
		row("window", "notes", "1", ":zsh", "1", ":*", v1("80x24,0,0,12"), "on"),
		row("state", "api", "notes"),
	)
}

func mustImport(t *testing.T, src Source) *Result {
	t.Helper()
	res, err := Import(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	return res
}

func paneBy(t *testing.T, s *snapshot.Snapshot, key string) snapshot.Pane {
	t.Helper()
	for _, p := range s.Panes {
		if p.Key == key {
			return p
		}
	}
	t.Fatalf("no pane %s", key)
	return snapshot.Pane{}
}

func TestImport_FromFile(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "tmux_resurrect_20261024T004046.txt", basic())
	res := mustImport(t, Source{From: f, Home: home, Producer: "moox test"})
	s, rep := res.Snapshot, res.Report

	if s.Source != snapshot.SourceImport || s.Format != snapshot.FormatName || s.Producer != "moox test" {
		t.Fatalf("header: %+v", s)
	}
	if err := snapshot.Validate(s); err != nil {
		t.Fatal(err)
	}
	if rep.Rows != 8 || rep.Panes != 4 || rep.Windows != 3 || rep.Sessions != 2 || rep.Sids != 1 || rep.Continuations != 0 {
		t.Fatalf("report: %+v", rep)
	}
	if rep.SourceFile != f || rep.SourceMtime.IsZero() {
		t.Fatalf("source: %q %v", rep.SourceFile, rep.SourceMtime)
	}
	if *s.Client != (snapshot.Client{Session: "api", LastSession: "notes"}) {
		t.Fatalf("client %+v", *s.Client)
	}

	c := paneBy(t, s, "api:1.1")
	if c.Role != snapshot.RoleClaude || c.SID() != "0b9f-uuid" || c.Claude.Launcher != "claude" ||
		c.Launch != "c --worktree foo" || c.FullCommand != "claude --dangerously-skip-permissions" ||
		c.Cwd != "/home/user/dev/api" || c.Title != "✳ Claude Code" || !c.Active || c.CurrentCommand != "2.1.309" {
		t.Fatalf("claude pane: %+v", c)
	}
	if c.SavedID != "%3" {
		t.Errorf("saved_id from the layout leaf: %q", c.SavedID)
	}
	if p := paneBy(t, s, "api:2.1"); p.Role != snapshot.RoleShell || p.FullCommand != "" || p.SavedID != "%7" || p.Active {
		t.Errorf("shell pane: %+v", p)
	}
	if p := paneBy(t, s, "api:2.2"); p.Role != snapshot.RoleProgram || p.Launch != "tail -f x.log" || p.Claude != nil || p.SavedID != "%9" {
		t.Errorf("program pane with a bare ;CLAUDE_CMD= recording: %+v", p)
	}
	if p := paneBy(t, s, "notes:1.1"); p.Cwd != "/home/user/notes" || p.SavedID != "%12" {
		t.Errorf("~ path / v1 id: %+v", p)
	}

	w := s.Windows
	if w[0].Width != 167 || w[0].Height != 51 || w[0].LayoutFormat != snapshot.LayoutV2 || w[0].AutomaticRename != "off" ||
		!w[0].Active || w[0].Name != "claude" || w[0].Layout != lay1 {
		t.Errorf("window 1: %+v", w[0])
	}
	if w[1].Width != 203 || w[1].Height != 56 || w[1].AutomaticRename != snapshot.AutoRenameUnset || !w[1].Last {
		t.Errorf("window 2: %+v", w[1])
	}
	if w[2].Width != 80 || w[2].Height != 24 || w[2].LayoutFormat != snapshot.LayoutV1 || w[2].AutomaticRename != "on" {
		t.Errorf("window 3 (v1): %+v", w[2])
	}
	if se := s.Sessions[0]; se.Name != "api" || se.ActiveWindow != 1 || se.LastWindow != 2 {
		t.Errorf("session: %+v", se)
	}
}

func TestImport_RefusesMooxJSON(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "moox-20261024T004046.json", `{"format":"moox-snapshot","version":1}`+"\n")
	if _, err := Import(context.Background(), Source{From: f}, nil); !errors.Is(err, ErrNotResurrect) {
		t.Fatalf("moox JSON: got %v, want ErrNotResurrect", err)
	}
	g := writeFile(t, dir, "other.txt", "\n\nhello\tworld\n")
	if _, err := Import(context.Background(), Source{From: g}, nil); !errors.Is(err, ErrNotResurrect) {
		t.Fatalf("other TSV: got %v", err)
	}
	// A directory with only moox files (and a last that names one).
	if err := os.Symlink("moox-20261024T004046.json", filepath.Join(dir, "last")); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(context.Background(), Source{From: dir}, nil); !errors.Is(err, ErrNotResurrect) {
		t.Fatalf("dir of moox files: got %v", err)
	}
}

func TestImport_DirResolvesLastNeverMoox(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "tmux_resurrect_20261024T000000.txt", basic())
		writeFile(t, dir, "tmux_resurrect_20261025T000000.txt", lines(row("pane", "x", "1", "1", ":*", "1", "t", ":/", "1", "zsh", ":")))
		if err := os.Symlink("tmux_resurrect_20261024T000000.txt", filepath.Join(dir, "last")); err != nil {
			t.Fatal(err)
		}
		res := mustImport(t, Source{From: dir})
		if filepath.Base(res.Report.SourceFile) != "tmux_resurrect_20261024T000000.txt" {
			t.Fatalf("resolved %s", res.Report.SourceFile)
		}
	})
	t.Run("regular-file-last", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "last", basic())
		res := mustImport(t, Source{From: dir})
		if filepath.Base(res.Report.SourceFile) != "last" || res.Report.Panes != 4 {
			t.Fatalf("resolved %s %+v", res.Report.SourceFile, res.Report)
		}
	})
	t.Run("dangling-newest-valid-never-moox", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, dir, "tmux_resurrect_20261023T000000.txt", basic())
		// Newer but truncated (0 windows, 0 state: R2) — not valid.
		writeFile(t, dir, "tmux_resurrect_20261024T000000.txt", lines(row("pane", "x", "1", "1", ":*", "1", "t", ":/", "1", "zsh", ":")))
		// Newest of all, a moox file: never considered.
		writeFile(t, dir, "moox-20991231T000000.json", `{"format":"moox-snapshot"}`)
		if err := os.Symlink("tmux_resurrect_20261099T000000.txt", filepath.Join(dir, "last")); err != nil {
			t.Fatal(err)
		}
		res := mustImport(t, Source{From: dir})
		if filepath.Base(res.Report.SourceFile) != "tmux_resurrect_20261023T000000.txt" {
			t.Fatalf("resolved %s", res.Report.SourceFile)
		}
		if !hasWarning(res.Report, "LAST-DANGLING") {
			t.Errorf("warnings %v", res.Report.Warnings)
		}
	})
}

func hasWarning(r Report, kind string) bool {
	for _, w := range r.Warnings {
		if strings.HasPrefix(w, kind) {
			return true
		}
	}
	return false
}

func TestImport_ContinuationsNotJudgedDoubled(t *testing.T) {
	// A full_command spanning lines: pgrep lists every child, one per line,
	// and an argv with newlines spans lines too. 30 continuations of the
	// `-zsh`/`<defunct>` kind and some with tabs.
	rows := []string{row("pane", "s", "1", "1", ":*", "1", "t", ":/home/user", "1", "2.1.309",
		":claude --resume abc", ";CLAUDE_SID=abc")}
	for i := 0; i < 30; i++ {
		switch i % 3 {
		case 0:
			rows = append(rows, "-zsh")
		case 1:
			rows = append(rows, "<defunct>")
		default:
			rows = append(rows, row("  some prompt text", "more"))
		}
	}
	rows = append(rows,
		row("pane", "s", "1", "1", ":*", "2", "t", ":/home/user", "0", "zsh", ":"),
		row("window", "s", "1", ":w", "1", ":*", lay2, "off"),
		row("state", "s"))
	res := mustImport(t, Source{From: writeFile(t, t.TempDir(), "f.txt", lines(rows...))})
	if res.Report.Continuations != 30 || res.Report.Panes != 2 || res.Report.Rows != 4 {
		t.Fatalf("report %+v", res.Report)
	}
	p := paneBy(t, res.Snapshot, "s:1.1")
	if p.SID() != "abc" || !strings.HasPrefix(p.FullCommand, "claude --resume abc\n-zsh\n<defunct>\n  some prompt text\tmore\n") {
		t.Fatalf("continuations not attached in order: %+v", p)
	}
	if n := strings.Count(p.FullCommand, "\n"); n != 30 {
		t.Fatalf("full_command holds %d continuation lines, want 30: %q", n, p.FullCommand)
	}
	if q := paneBy(t, res.Snapshot, "s:1.2"); q.FullCommand != "" {
		t.Fatalf("a continuation leaked into the next pane: %+v", q)
	}
}

// TestImport_ContinuationAttachesToPrecedingPane (I-2): a continuation line
// belongs to the pane record before it (research/resurrect-behaviours.md:
// "Attach continuation lines to the preceding pane record"), kept verbatim and
// in order after a newline; one that follows no pane row is counted only.
func TestImport_ContinuationAttachesToPrecedingPane(t *testing.T) {
	f := writeFile(t, t.TempDir(), "f.txt", lines(
		row("pane", "s", "1", "1", ":*", "1", "t", ":/home/user", "1", "sleep", ":sleep 1000"),
		"helper --flag=two",
		row("helper", "with\ttab"),
		row("pane", "s", "1", "1", ":*", "2", "t", ":/home/user", "0", "zsh", ":"),
		"second-pane child",
		row("window", "s", "1", ":w", "1", ":*", lay2, "off"),
		"after a window row",
		row("state", "s"),
	))
	res := mustImport(t, Source{From: f})
	if res.Report.Continuations != 4 || res.Report.Panes != 2 {
		t.Fatalf("report %+v", res.Report)
	}
	if p := paneBy(t, res.Snapshot, "s:1.1"); p.FullCommand != "sleep 1000\nhelper --flag=two\nhelper\twith\ttab" {
		t.Fatalf("s:1.1 full_command %q", p.FullCommand)
	}
	if p := paneBy(t, res.Snapshot, "s:1.2"); p.FullCommand != "\nsecond-pane child" {
		t.Fatalf("s:1.2 full_command %q", p.FullCommand)
	}
}

func TestImport_ContinuationLooksLikeRecord(t *testing.T) {
	// RR15: a continuation that STARTS with a record type but fails that
	// type's field shape is a continuation, not a record.
	f := writeFile(t, t.TempDir(), "f.txt", lines(
		row("pane", "s", "1", "1", ":*", "1", "t", ":/home/user", "1", "zsh", ":sh -c x"),
		row("pane", "and then the prompt says"),
		row("window", "is a word", "too"),
		row("state", "of", "the", "art"),
		"pane",
		row("window", "s", "1", ":w", "1", ":*", lay1, "off"),
		row("state", "s", ""),
	))
	res := mustImport(t, Source{From: f})
	if res.Report.Continuations != 4 || res.Report.Panes != 1 || res.Report.Windows != 1 {
		t.Fatalf("report %+v", res.Report)
	}
	if res.Snapshot.Client.Session != "s" || res.Snapshot.Client.LastSession != "" {
		t.Fatalf("state %+v", res.Snapshot.Client)
	}
}

func TestImport_ClaudeCmdFullBase64WithPadding(t *testing.T) {
	// "wt foo red" → d3QgZm9vIHJlZA== (two '=' of padding): decoding must
	// not cut at the first '='. A multi-line recording keeps its first line.
	cases := map[string]string{
		"wt foo red":          "wt foo red",
		"wt a":                "wt a", // one '=' of padding
		"vim x\nsecond line":  "vim x",
		"echo a=b ==c":        "echo a=b ==c",
		"claudish --model x ": "claudish --model x ",
	}
	for in, want := range cases {
		enc := b64(in)
		f := writeFile(t, t.TempDir(), "f.txt", lines(
			row("pane", "s", "1", "1", ":*", "1", "t", ":/", "1", "2.1.1", ":claude", ";CLAUDE_SID=sid-1", ";CLAUDE_CMD="+enc),
			row("window", "s", "1", ":w", "1", ":*", lay1, "off"),
			row("state", "s"),
		))
		res := mustImport(t, Source{From: f})
		if got := res.Snapshot.Panes[0].Launch; got != want {
			t.Errorf("%q (%s): launch %q, want %q", in, enc, got, want)
		}
	}
	// Unpadded base64 is accepted too.
	f := writeFile(t, t.TempDir(), "f.txt", lines(
		row("pane", "s", "1", "1", ":*", "1", "t", ":/", "1", "zsh", ":", ";CLAUDE_CMD="+strings.TrimRight(b64("wt foo red"), "=")),
		row("window", "s", "1", ":w", "1", ":*", lay1, "off"),
		row("state", "s"),
	))
	if got := mustImport(t, Source{From: f}).Snapshot.Panes[0].Launch; got != "wt foo red" {
		t.Errorf("unpadded: %q", got)
	}
}

func TestImport_ClaudishReplayVerbatim(t *testing.T) {
	f := writeFile(t, t.TempDir(), "f.txt", lines(
		row("pane", "s", "1", "1", ":*", "1", "", ":/home/user", "1", "2.1.1", "4242", ":claudish --model x",
			";CLAUDE_SID=sid-2", ";CLAUDE_CMD="+b64("cl"), ";CLAUDISH_REPLAY=--model x -d  --y"),
		row("window", "s", "1", ":w", "1", ":*", lay1, "off"),
		row("state", "s"),
	))
	res := mustImport(t, Source{From: f})
	p := res.Snapshot.Panes[0]
	if p.Claude == nil || p.Claude.Launcher != "claudish" || p.Claude.Replay != "--model x -d  --y" || p.Launch != "cl" {
		t.Fatalf("claudish row: %+v %+v", p, p.Claude)
	}
	if res.Report.Realigned != 1 || p.Cwd != "/home/user" || p.FullCommand != "claudish --model x" {
		t.Fatalf("realigned with three tags: %+v %+v", p, res.Report)
	}
}

func TestImport_SidRejected(t *testing.T) {
	f := writeFile(t, t.TempDir(), "f.txt", lines(
		row("pane", "s", "1", "1", ":*", "1", "t", ":/", "1", "2.1.1", ":claude", ";CLAUDE_SID=DUP=abc"),
		row("pane", "s", "1", "1", ":*", "2", "t", ":/", "0", "2.1.1", ":claude", ";CLAUDE_SID=$(rm -rf x)"),
		row("window", "s", "1", ":w", "1", ":*", lay2, "off"),
		row("state", "s"),
	))
	res := mustImport(t, Source{From: f})
	for _, p := range res.Snapshot.Panes {
		if p.Claude != nil || p.Role == snapshot.RoleClaude {
			t.Errorf("%s kept a rejected sid: %+v", p.Key, p.Claude)
		}
	}
	if res.Report.Sids != 0 || !hasWarning(res.Report, "SID-REJECTED") {
		t.Fatalf("report %+v", res.Report)
	}
}

func TestImport_RealignedEmptyTitle(t *testing.T) {
	// 12 fields: [7]="" [8]=:path [9]=active [10]=cmd [11]=pid [12]=:full.
	f := writeFile(t, t.TempDir(), "f.txt", lines(
		row("pane", "s", "1", "1", ":*", "1", "", ":/home/user/a", "1", "nvim", "81234", ":nvim -S"),
		row("window", "s", "1", ":w", "1", ":*", lay1, "off"),
		row("state", "s"),
	))
	res := mustImport(t, Source{From: f})
	p := res.Snapshot.Panes[0]
	if p.Title != "" || p.Cwd != "/home/user/a" || !p.Active || p.CurrentCommand != "nvim" || p.FullCommand != "nvim -S" ||
		res.Report.Realigned != 1 || res.Report.Collapsed != 0 {
		t.Fatalf("%+v %+v", p, res.Report)
	}
}

func TestImport_CollapsedNeverRealigned(t *testing.T) {
	// 11 fields: the path shifted into f7, f8 is active, f9 the cmd, f10 the
	// pid, and f11 ":" (resurrect ran pgrep on history_size).
	f := writeFile(t, t.TempDir(), "f.txt", lines(
		row("pane", "s", "1", "1", ":*", "1", ":/home/user/b", "1", "zsh", "81235", ":"),
		row("window", "s", "1", ":w", "1", ":*", lay1, "off"),
		row("state", "s"),
	))
	res := mustImport(t, Source{From: f})
	p := res.Snapshot.Panes[0]
	if p.Title != "" || p.Cwd != "/home/user/b" || !p.Active || p.CurrentCommand != "zsh" || p.FullCommand != "" ||
		res.Report.Collapsed != 1 {
		t.Fatalf("%+v %+v", p, res.Report)
	}
}

func TestImport_SpaceInPath(t *testing.T) {
	// R36/C4: resurrect writes the first space as `\ `; the importer
	// unescapes every one.
	f := writeFile(t, t.TempDir(), "f.txt", lines(
		row("pane", "s", "1", "1", ":*", "1", "t", `:/home/user/my\ dir/a\ b`, "1", "zsh", ":"),
		row("pane", "s", "1", "1", ":*", "2", "", `:~/x\ y`, "0", "zsh", "123", ":"),
		row("window", "s", "1", ":w", "1", ":*", lay2, "off"),
		row("state", "s"),
	))
	res := mustImport(t, Source{From: f, Home: home})
	if got := res.Snapshot.Panes[0].Cwd; got != "/home/user/my dir/a b" {
		t.Errorf("cwd %q", got)
	}
	if got := res.Snapshot.Panes[1].Cwd; got != "/home/user/x y" {
		t.Errorf("realigned ~ cwd %q", got)
	}
}

func TestImport_HealScarredTitle(t *testing.T) {
	// B40/R14: a shell titled ":/old/path" (the old re-collapse) whose cwd is
	// $HOME and whose path exists comes back in that path, title cleared.
	h := t.TempDir()
	real := filepath.Join(h, "proj")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	f := writeFile(t, t.TempDir(), "f.txt", lines(
		row("pane", "s", "1", "1", ":*", "1", ":"+real, ":"+h, "1", "zsh", ":"),
		row("pane", "s", "1", "1", ":*", "2", ":"+filepath.Join(h, "gone"), ":"+h, "0", "zsh", ":"),
		row("pane", "s", "2", "0", ":", "1", ":"+real, ":/elsewhere", "1", "vim", ":vim x"),
		row("window", "s", "1", ":w", "1", ":*", lay2, "off"),
		row("window", "s", "2", ":v", "0", ":", lay1, "off"),
		row("state", "s"),
	))
	res := mustImport(t, Source{From: f, Home: h})
	if p := paneBy(t, res.Snapshot, "s:1.1"); p.Cwd != real || p.Title != "" {
		t.Errorf("healed: %+v", p)
	}
	if p := paneBy(t, res.Snapshot, "s:1.2"); p.Cwd != h || p.Title != "" {
		t.Errorf("missing path: title cleared, cwd kept: %+v", p)
	}
	if p := paneBy(t, res.Snapshot, "s:2.1"); p.Title != ":"+real || p.Cwd != "/elsewhere" {
		t.Errorf("a program row is not a shell row: %+v", p)
	}
	if res.Report.HealedTitles != 1 {
		t.Errorf("HealedTitles %d", res.Report.HealedTitles)
	}
}

func TestImport_Roles(t *testing.T) {
	f := writeFile(t, t.TempDir(), "f.txt", lines(
		row("pane", "s", "1", "1", ":*", "1", "t", ":/", "1", "bash", ":tt observe --pane --auto"),
		row("pane", "s", "1", "1", ":*", "2", "❄ FROZEN k3y banner", ":/", "0", "zsh", ":"),
		row("window", "s", "1", ":w", "1", ":*", lay2, "off"),
		row("state", "s"),
	))
	res := mustImport(t, Source{From: f})
	if p := res.Snapshot.Panes[0]; p.Role != snapshot.RoleTTObserver {
		t.Errorf("observer: %+v", p)
	}
	if p := res.Snapshot.Panes[1]; p.Role != snapshot.RoleFrozen || p.FrozenKey != "k3y" {
		t.Errorf("frozen: %+v", p)
	}
}

func TestImport_StateRowTwoOrThreeFields(t *testing.T) {
	for _, st := range []string{row("state", "s"), row("state", "s", "o")} {
		f := writeFile(t, t.TempDir(), "f.txt", lines(
			row("pane", "s", "1", "1", ":*", "1", "t", ":/", "1", "zsh", ":"),
			row("window", "s", "1", ":w", "1", ":*", lay1, "off"), st))
		mustImport(t, Source{From: f})
	}
	for name, body := range map[string]string{
		"none": lines(row("pane", "s", "1", "1", ":*", "1", "t", ":/", "1", "zsh", ":"), row("window", "s", "1", ":w", "1", ":*", lay1, "off")),
		"two": lines(row("pane", "s", "1", "1", ":*", "1", "t", ":/", "1", "zsh", ":"), row("window", "s", "1", ":w", "1", ":*", lay1, "off"),
			row("state", "s"), row("state", "s")),
	} {
		if _, err := Import(context.Background(), Source{From: writeFile(t, t.TempDir(), "f.txt", body)}, nil); !errors.Is(err, snapshot.ErrInvalid) {
			t.Errorf("%s state rows: %v", name, err)
		}
	}
}

func TestImport_DuplicateRowsInvalid(t *testing.T) {
	r := row("pane", "s", "1", "1", ":*", "1", "t", ":/", "1", "zsh", ":")
	f := writeFile(t, t.TempDir(), "f.txt", lines(r, r, row("window", "s", "1", ":w", "1", ":*", lay1, "off"), row("state", "s")))
	if _, err := Import(context.Background(), Source{From: f}, nil); !errors.Is(err, snapshot.ErrInvalid) {
		t.Fatalf("doubled pane rows: %v", err)
	}
}

func TestImport_LayoutV1ChecksumRepaired(t *testing.T) {
	bad := "ffff,80x24,0,0,4"
	f := writeFile(t, t.TempDir(), "f.txt", lines(
		row("pane", "s", "1", "1", ":*", "1", "t", ":/", "1", "zsh", ":"),
		row("window", "s", "1", ":w", "1", ":*", bad, "off"), row("state", "s")))
	res := mustImport(t, Source{From: f})
	w := res.Snapshot.Windows[0]
	if w.Layout != v1("80x24,0,0,4") || w.Width != 80 || w.Height != 24 || !hasWarning(res.Report, "LAYOUT-CHECKSUM") {
		t.Fatalf("%+v %v", w, res.Report.Warnings)
	}
	if _, _, err := layout.Parse(w.Layout); err != nil {
		t.Fatal(err)
	}
}

func TestImport_SyntheticIDsWhenLeavesDiffer(t *testing.T) {
	// One leaf, two panes (a floating pane, or a stale layout): the ids
	// cannot be matched, so both panes get %imp<n>.
	f := writeFile(t, t.TempDir(), "f.txt", lines(
		row("pane", "s", "1", "1", ":*", "1", "t", ":/", "1", "zsh", ":"),
		row("pane", "s", "1", "1", ":*", "2", "t", ":/", "0", "zsh", ":"),
		row("window", "s", "1", ":w", "1", ":*", lay1, "off"), row("state", "s")))
	res := mustImport(t, Source{From: f})
	if a, b := res.Snapshot.Panes[0].SavedID, res.Snapshot.Panes[1].SavedID; a != "%imp1" || b != "%imp2" {
		t.Fatalf("ids %q %q", a, b)
	}
	if !hasWarning(res.Report, "LAYOUT-LEAVES") {
		t.Errorf("warnings %v", res.Report.Warnings)
	}
}

func TestImport_GroupedSession(t *testing.T) {
	f := writeFile(t, t.TempDir(), "f.txt", lines(
		row("grouped_session", "mirror", "main", ":2", ":1"),
		row("pane", "main", "1", "1", ":*", "1", "t", ":/", "1", "zsh", ":"),
		row("window", "main", "1", ":w", "1", ":*", lay1, "off"), row("state", "mirror", "main")))
	res := mustImport(t, Source{From: f})
	s := res.Snapshot
	if len(s.Sessions) != 2 || s.Sessions[1] != (snapshot.Session{Name: "mirror", Group: "main", GroupedWith: "main", ActiveWindow: 1, LastWindow: 2}) ||
		s.Sessions[0].Group != "main" {
		t.Fatalf("%+v", s.Sessions)
	}
	if err := snapshot.Validate(s); err != nil {
		t.Fatal(err)
	}
}

func TestImport_PaneWithoutWindowDropped(t *testing.T) {
	f := writeFile(t, t.TempDir(), "f.txt", lines(
		row("pane", "s", "1", "1", ":*", "1", "t", ":/", "1", "zsh", ":"),
		row("pane", "s", "9", "1", ":*", "1", "t", ":/", "1", "zsh", ":"),
		row("window", "s", "1", ":w", "1", ":*", lay1, "off"),
		row("window", "s", "5", ":empty", "0", ":", lay1, "off"),
		row("state", "s")))
	res := mustImport(t, Source{From: f})
	if res.Report.Dropped != 2 || res.Report.Panes != 1 || res.Report.Windows != 1 {
		t.Fatalf("%+v", res.Report)
	}
}

// tarGz builds a resurrect-style archive.
func tarGz(t *testing.T, members map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: "./pane_contents/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, body := range members {
		if err := tw.WriteHeader(&tar.Header{Name: "./pane_contents/pane-" + name, Typeflag: tar.TypeReg, Mode: 0o644,
			Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

func TestImport_StaleContentsDropped(t *testing.T) {
	members := map[string]string{"api:1.1": "claude scrollback", "api:2.1": "server log", "notes:1.1": "notes",
		"gone:1.1": "no such pane"}
	setup := func(t *testing.T, archiveAge time.Duration) (string, string) {
		dir := t.TempDir()
		f := writeFile(t, dir, "tmux_resurrect_20261024T004046.txt", basic())
		a := filepath.Join(dir, "pane_contents.tar.gz")
		if err := os.WriteFile(a, tarGz(t, members), 0o600); err != nil {
			t.Fatal(err)
		}
		snapT := time.Now().Add(-time.Hour)
		_ = os.Chtimes(f, snapT, snapT)
		at := snapT.Add(-archiveAge)
		_ = os.Chtimes(a, at, at)
		return dir, f
	}
	t.Run("fresh", func(t *testing.T) {
		_, f := setup(t, 4*time.Minute) // within the 5-minute skew
		res := mustImport(t, Source{From: f})
		if !res.Report.ContentsUsed || res.Report.ContentsPanes != 3 {
			t.Fatalf("%+v", res.Report)
		}
		// keyed by saved_id once, never positionally
		if string(res.Contents["%3"]) != "claude scrollback" || string(res.Contents["%7"]) != "server log" {
			t.Fatalf("entries %v", res.Contents)
		}
		for _, p := range res.Snapshot.Panes {
			if want := p.Key != "api:2.2"; p.Contents != want {
				t.Errorf("%s contents=%v", p.Key, p.Contents)
			}
		}
		if res.Snapshot.Contents == nil || res.Snapshot.Contents.Panes != 3 {
			t.Errorf("contents object %+v", res.Snapshot.Contents)
		}
	})
	t.Run("stale", func(t *testing.T) {
		_, f := setup(t, 10*time.Minute)
		res := mustImport(t, Source{From: f})
		if res.Report.ContentsUsed || len(res.Contents) != 0 || res.Snapshot.Contents != nil || !hasWarning(res.Report, "CONTENTS-STALE") {
			t.Fatalf("%+v", res.Report)
		}
		for _, p := range res.Snapshot.Panes {
			if p.Contents {
				t.Errorf("%s marked contents", p.Key)
			}
		}
	})
	t.Run("explicit-missing", func(t *testing.T) {
		dir, f := setup(t, 0)
		res := mustImport(t, Source{From: f, Contents: filepath.Join(dir, "nope.tar.gz")})
		if res.Report.ContentsUsed || !hasWarning(res.Report, "CONTENTS-UNREADABLE") {
			t.Fatalf("%+v", res.Report)
		}
	})
}

// TestImport_NewerLayoutArchiveDropped (G1-H5): importing an OLDER
// snapshot file while the shared archive was written after a NEWER one
// would key that newer layout's S:W.P scrollback onto the old panes; it is
// dropped. The newest file keeps an archive written long after it (resurrect
// rewrites the archive on every save but keeps an identical snapshot).
func TestImport_NewerLayoutArchiveDropped(t *testing.T) {
	members := map[string]string{"api:1.1": "claude scrollback", "api:2.1": "server log", "notes:1.1": "notes"}
	dir := t.TempDir()
	old := writeFile(t, dir, "tmux_resurrect_20261024T004046.txt", basic())
	newer := writeFile(t, dir, "tmux_resurrect_20261024T104046.txt", basic())
	a := filepath.Join(dir, "pane_contents.tar.gz")
	if err := os.WriteFile(a, tarGz(t, members), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = os.Chtimes(old, now.Add(-10*time.Hour), now.Add(-10*time.Hour))
	_ = os.Chtimes(newer, now.Add(-3*time.Hour), now.Add(-3*time.Hour))
	_ = os.Chtimes(a, now.Add(-time.Minute), now.Add(-time.Minute)) // 3 h of unchanged-layout saves since
	res := mustImport(t, Source{From: old})
	if res.Report.ContentsUsed || len(res.Contents) != 0 || !hasWarning(res.Report, "CONTENTS-NEWER") {
		t.Fatalf("old snapshot with a newer layout's archive: %+v", res.Report)
	}
	res = mustImport(t, Source{From: newer})
	if !res.Report.ContentsUsed || res.Report.ContentsPanes != 3 {
		t.Fatalf("newest snapshot lost its own (later-rewritten) archive: %+v", res.Report)
	}
}

// TestImport_ExplicitArchiveElsewhereNeedsLink (G1-2-H3): an explicit
// --contents archive in ANOTHER directory than the snapshot is matched only
// by positional S:W.P keys, which a later layout reuses for other panes. The
// newer-snapshot scan of the snapshot's own directory proves nothing about
// it, so it is used only when the archive's directory holds the same
// snapshot (a byte-identical resurrect file) and no newer one; otherwise it
// is dropped (CONTENTS-UNLINKED), unless TrustContents says the pairing is
// deliberate (also the way past CONTENTS-NEWER/STALE, G1-2-L1).
func TestImport_ExplicitArchiveElsewhereNeedsLink(t *testing.T) {
	members := map[string]string{"api:1.1": "another pane's scrollback", "api:2.1": "server log", "notes:1.1": "notes"}
	now := time.Now()
	setup := func(t *testing.T) (snap, resDir, archive string) {
		backup := t.TempDir()
		snap = writeFile(t, backup, "old.txt", basic()) // a standalone older copy
		_ = os.Chtimes(snap, now.Add(-10*time.Hour), now.Add(-10*time.Hour))
		resDir = t.TempDir()
		archive = filepath.Join(resDir, "pane_contents.tar.gz")
		if err := os.WriteFile(archive, tarGz(t, members), 0o600); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(archive, now.Add(-time.Minute), now.Add(-time.Minute))
		return snap, resDir, archive
	}
	t.Run("no link: dropped", func(t *testing.T) {
		snap, resDir, archive := setup(t)
		// the archive's own dir holds a different, newer layout
		other := writeFile(t, resDir, "tmux_resurrect_20261024T104046.txt", lines(
			row("pane", "api", "1", "1", ":*", "1", "t", ":/", "1", "zsh", ":"),
			row("window", "api", "1", ":w", "1", ":*", lay1, "off"), row("state", "api")))
		_ = os.Chtimes(other, now.Add(-2*time.Hour), now.Add(-2*time.Hour))
		res := mustImport(t, Source{From: snap, Contents: archive})
		if res.Report.ContentsUsed || len(res.Contents) != 0 || !hasWarning(res.Report, "CONTENTS-UNLINKED") {
			t.Fatalf("an unrelated newer archive was attached to a standalone snapshot: %+v", res.Report)
		}
	})
	t.Run("no resurrect files beside it: dropped", func(t *testing.T) {
		snap, _, archive := setup(t)
		res := mustImport(t, Source{From: snap, Contents: archive})
		if res.Report.ContentsUsed || !hasWarning(res.Report, "CONTENTS-UNLINKED") {
			t.Fatalf("%+v", res.Report)
		}
	})
	t.Run("the same snapshot beside it: used", func(t *testing.T) {
		snap, resDir, archive := setup(t)
		twin := writeFile(t, resDir, "tmux_resurrect_20261024T004046.txt", basic())
		_ = os.Chtimes(twin, now.Add(-3*time.Minute), now.Add(-3*time.Minute))
		res := mustImport(t, Source{From: snap, Contents: archive})
		if !res.Report.ContentsUsed || res.Report.ContentsPanes != 3 {
			t.Fatalf("the archive beside the snapshot's own twin was dropped: %+v", res.Report)
		}
	})
	t.Run("the twin is superseded beside it: dropped", func(t *testing.T) {
		snap, resDir, archive := setup(t)
		twin := writeFile(t, resDir, "tmux_resurrect_20261024T004046.txt", basic())
		_ = os.Chtimes(twin, now.Add(-5*time.Hour), now.Add(-5*time.Hour))
		newer := writeFile(t, resDir, "tmux_resurrect_20261024T104046.txt", lines(
			row("pane", "api", "1", "1", ":*", "1", "t", ":/", "1", "zsh", ":"),
			row("window", "api", "1", ":w", "1", ":*", lay1, "off"), row("state", "api")))
		_ = os.Chtimes(newer, now.Add(-2*time.Hour), now.Add(-2*time.Hour))
		res := mustImport(t, Source{From: snap, Contents: archive})
		if res.Report.ContentsUsed || !hasWarning(res.Report, "CONTENTS-NEWER") {
			t.Fatalf("%+v", res.Report)
		}
	})
	t.Run("TrustContents: used, with a warning", func(t *testing.T) {
		snap, _, archive := setup(t)
		res := mustImport(t, Source{From: snap, Contents: archive, TrustContents: true})
		if !res.Report.ContentsUsed || res.Report.ContentsPanes != 3 || !hasWarning(res.Report, "CONTENTS-TRUSTED") {
			t.Fatalf("%+v", res.Report)
		}
	})
}

type fakeLive struct {
	out   string
	calls [][]string
}

func (f *fakeLive) Run(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	return []byte(f.out), nil
}

func TestImport_AttachLiveRecordings(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "f.txt", basic())
	rec := t.TempDir()
	writeFile(t, rec, "21", "npm run dev\nsecond\n") // api:2.1
	writeFile(t, rec, "22", "should not replace")    // api:2.2 has a ;CLAUDE_CMD=
	writeFile(t, rec, "40", "")                      // notes:1.1: empty = absent
	live := &fakeLive{out: strings.Join([]string{
		"api\x1f1\x1f1\x1f%20", "api\x1f2\x1f1\x1f%21", "api\x1f2\x1f2\x1f%22", "notes\x1f1\x1f1\x1f%40"}, "\n") + "\n"}
	res, err := Import(context.Background(), Source{From: f, AttachLive: true, Recordings: rec}, live)
	if err != nil {
		t.Fatal(err)
	}
	if len(live.calls) != 1 || live.calls[0][0] != "list-panes" {
		t.Fatalf("live calls %v", live.calls)
	}
	if p := paneBy(t, res.Snapshot, "api:2.1"); p.Launch != "npm run dev" {
		t.Errorf("attached %+v", p)
	}
	if p := paneBy(t, res.Snapshot, "api:2.2"); p.Launch != "tail -f x.log" {
		t.Errorf("tag recording kept %+v", p)
	}
	if p := paneBy(t, res.Snapshot, "notes:1.1"); p.Launch != "" {
		t.Errorf("empty recording attached %+v", p)
	}
	if res.Report.Recordings != 1 {
		t.Errorf("report %+v", res.Report)
	}
	if _, err := Import(context.Background(), Source{From: f, AttachLive: true}, nil); err == nil {
		t.Error("attach-live without a live server must fail")
	}
}

func TestImport_NeverWritesSource(t *testing.T) {
	dir := t.TempDir()
	f := writeFile(t, dir, "tmux_resurrect_20261024T004046.txt", basic())
	if err := os.Symlink(filepath.Base(f), filepath.Join(dir, "last")); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadDir(dir)
	st0, _ := os.Stat(f)
	mustImport(t, Source{From: dir})
	after, _ := os.ReadDir(dir)
	st1, _ := os.Stat(f)
	if len(before) != len(after) || !st0.ModTime().Equal(st1.ModTime()) || st0.Size() != st1.Size() {
		t.Fatal("the import touched the resurrect directory")
	}
}
