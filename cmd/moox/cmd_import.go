package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/MadAppGang/moox/internal/engine"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/mooxroot"
	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/statedir"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

const importUsage = `usage: moox import --from DIR|FILE [--contents FILE [--trust-contents]] [--attach-live]
                   [--out FILE | --publish (-L NAME|-S PATH) [--replace-live] [--yes]] [--report FILE]`

// cmdImport is `moox import` (§6.6, §8.1): a resurrect snapshot becomes a
// moox snapshot v1, written to --out (anywhere) or published as a server's
// moox-last. --publish is guarded (PR-H23): it needs an explicit -L/-S, and
// on the legacy default key also --replace-live plus a confirmation naming
// the snapshot it supersedes (or --yes).
func cmdImport(m *meta, args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(m.stderr)
	fs.Usage = func() { fmt.Fprintln(m.stderr, importUsage) }
	from := fs.String("from", "", "resurrect FILE, or DIR whose `last` is resolved")
	contents := fs.String("contents", "", "pane-contents archive (default <dir>/pane_contents.tar.gz)")
	trustContents := fs.Bool("trust-contents", false, "attach --contents without checking that it belongs to the snapshot")
	attachLive := fs.Bool("attach-live", false, "attach launch recordings of the matching live panes (read-only)")
	out := fs.String("out", "", "write the snapshot to FILE")
	publish := fs.Bool("publish", false, "publish as the server's moox-last")
	nameL := fs.String("L", "", "server socket NAME")
	pathS := fs.String("S", "", "server socket PATH")
	replaceLive := fs.Bool("replace-live", false, "allow superseding the live snapshot")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	reportFile := fs.String("report", "", "write the import report to FILE")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	bad := func(format string, a ...any) int {
		fmt.Fprintf(m.stderr, "moox import: "+format+"\n", a...)
		fmt.Fprintln(m.stderr, importUsage)
		return exitUsage
	}
	switch {
	case fs.NArg() != 0:
		return bad("unexpected argument %q", fs.Arg(0))
	case *from == "":
		return bad("--from is required")
	case *out == "" && !*publish:
		return bad("give --out FILE or --publish")
	case *out != "" && *publish:
		return bad("--out and --publish are exclusive")
	case *nameL != "" && *pathS != "":
		return bad("-L and -S are exclusive")
	case *publish && *nameL == "" && *pathS == "":
		return bad("--publish needs an explicit -L NAME or -S PATH")
	case !*publish && (*replaceLive || *yes):
		return bad("--replace-live and --yes apply to --publish only")
	case *trustContents && *contents == "":
		return bad("--trust-contents needs --contents FILE")
	}

	home := m.env.Getenv("HOME")
	claudeRoot := filepath.Join(home, ".claude")
	src := engine.ImportSource{From: *from, Contents: *contents, TrustContents: *trustContents, AttachLive: *attachLive, Home: home,
		Producer: "moox " + mooxroot.VersionString()}

	var target tmuxtarget.Target
	haveTarget := *nameL != "" || *pathS != "" || *attachLive
	if haveTarget {
		var err error
		if target, err = resolveTarget(m, *nameL, *pathS, *attachLive); err != nil {
			fmt.Fprintf(m.stderr, "moox import: %v\n", err)
			if *attachLive {
				return exitTmuxAbsent
			}
			return exitUsage
		}
	}
	var live engine.ImportRunner
	if *attachLive {
		paths, err := sockets.For(target.Socket, m.env)
		if err != nil {
			fmt.Fprintf(m.stderr, "moox import: %v\n", err)
			return exitError
		}
		src.Recordings, live = paths.Launch, target
	}

	im, err := engine.Import(m.ctx, src, live)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox import: %v\n", err)
		return exitFor(err)
	}
	if haveTarget {
		im.Snapshot.Socket = snapshot.Socket{Name: sockets.Name(target.Socket), Key: sockets.Key(target.Socket)}
	}

	code := exitOK
	if *publish {
		code = publishImport(m, im, target, *replaceLive, *yes)
	} else {
		wrote, err := engine.WriteImport(im, *out, claudeRoot)
		if err != nil {
			fmt.Fprintf(m.stderr, "moox import: %v\n", err)
			return exitFor(err)
		}
		for _, w := range wrote {
			fmt.Fprintf(m.stdout, "wrote %s\n", w)
		}
	}
	text := formatReport(im.Report)
	if *reportFile != "" {
		if err := writeReport(*reportFile, text, claudeRoot); err != nil {
			fmt.Fprintf(m.stderr, "moox import: report: %v\n", err)
			if code == exitOK {
				code = exitError
			}
		}
	} else {
		fmt.Fprint(m.stderr, text)
	}
	return code
}

// resolveTarget: -L NAME / -S PATH, else (attach-live only) the usual
// manifest-aware resolution (MOOX_TMUX_SOCKET > $TMUX > the manifest's mode
// > default, G2-H2). The tmux binary is needed only to run the read-only
// list-panes of --attach-live.
func resolveTarget(m *meta, name, path string, needBin bool) (tmuxtarget.Target, error) {
	sock := path
	if name != "" {
		if strings.Contains(name, "/") {
			return tmuxtarget.Target{}, fmt.Errorf("-L takes a NAME; use -S for a path")
		}
		sock = name
	}
	if needBin {
		return m.target(&sockFlags{path: sock})
	}
	env := m.env
	if !strings.Contains(sock, "/") {
		p, err := tmuxtarget.LabelPath(env, sock)
		return tmuxtarget.Target{Socket: p}, err
	}
	abs, err := filepath.Abs(sock)
	return tmuxtarget.Target{Socket: abs}, err
}

func publishImport(m *meta, im *engine.Imported, target tmuxtarget.Target, replaceLive, yes bool) int {
	e, err := engine.New(engine.Options{Target: target, Env: m.env, Producer: im.Snapshot.Producer})
	if err != nil {
		fmt.Fprintf(m.stderr, "moox import: %v\n", err)
		return exitError
	}
	if e.Paths().Legacy {
		if !replaceLive {
			fmt.Fprintln(m.stderr, "moox import: the default server's snapshot is the live one; --publish there needs --replace-live")
			return exitUsage
		}
		if !yes {
			name, snap := e.Superseded()
			what := "no current moox snapshot"
			if snap != nil {
				what = fmt.Sprintf("%s (%s, %d panes, %d windows, created %s)", name, snap.Source, len(snap.Panes),
					len(snap.Windows), snap.CreatedAt)
			}
			if !m.stdinTTY {
				fmt.Fprintf(m.stderr, "moox import: publishing would supersede %s; confirm with --yes (no terminal to ask)\n", what)
				return exitUsage
			}
			fmt.Fprintf(m.stderr, "Publish the import of %s (%d panes, %d windows) as the default server's snapshot,\nsuperseding %s? [y/N] ",
				filepath.Base(im.Report.SourceFile), im.Report.Panes, im.Report.Windows, what)
			if !confirmed(m.stdin) {
				fmt.Fprintln(m.stderr, "moox import: aborted; nothing written")
				return exitAborted
			}
		}
	}
	res, err := e.PublishImport(m.ctx, im, engine.PublishImportOptions{ReplaceLive: replaceLive})
	if err != nil {
		fmt.Fprintf(m.stderr, "moox import: %v\n", err)
		return exitFor(err)
	}
	fmt.Fprintf(m.stdout, "published %s\n", filepath.Join(e.Paths().Snapshots, res.File))
	if res.Superseded != "" {
		fmt.Fprintf(m.stdout, "superseded %s\n", res.Superseded)
	}
	return exitOK
}

func confirmed(r io.Reader) bool {
	line, _ := bufio.NewReader(r).ReadString('\n')
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

func exitFor(err error) int {
	var busy *engine.BusyError
	switch {
	case errors.Is(err, engine.ErrNotResurrect), errors.Is(err, snapshot.ErrInvalid), errors.Is(err, snapshot.ErrNewerFormat):
		return exitInvalid
	case errors.Is(err, engine.ErrNewerSave):
		return exitNotApplic
	case errors.Is(err, engine.ErrNeedsReplaceLive), errors.Is(err, engine.ErrProtectedOut):
		return exitUsage
	case errors.As(err, &busy), errors.Is(err, engine.ErrBusy):
		return exitBusy
	}
	return exitError
}

// formatReport renders the report, one "name value" per line, then the
// warnings.
func formatReport(r engine.ImportReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "source %s\n", r.SourceFile)
	if !r.SourceMtime.IsZero() {
		fmt.Fprintf(&b, "source_mtime %s\n", r.SourceMtime.Format("2006-01-02T15:04:05Z07:00"))
	}
	for _, kv := range []struct {
		k string
		v int
	}{{"rows", r.Rows}, {"panes", r.Panes}, {"windows", r.Windows}, {"sessions", r.Sessions}, {"sids", r.Sids},
		{"continuations", r.Continuations}, {"realigned", r.Realigned}, {"collapsed", r.Collapsed},
		{"healed_titles", r.HealedTitles}, {"dropped", r.Dropped}, {"recordings", r.Recordings},
		{"contents_panes", r.ContentsPanes}} {
		fmt.Fprintf(&b, "%s %d\n", kv.k, kv.v)
	}
	fmt.Fprintf(&b, "contents_used %v\n", r.ContentsUsed)
	fmt.Fprintf(&b, "warnings %d\n", len(r.Warnings))
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "  %s\n", w)
	}
	return b.String()
}

func writeReport(file, text, claudeRoot string) error {
	abs, err := filepath.Abs(file)
	if err != nil {
		return err
	}
	if engine.IsResurrectFile(filepath.Base(abs)) {
		return fmt.Errorf("%s belongs to resurrect", filepath.Base(abs))
	}
	d, err := statedir.Open(filepath.Dir(abs), claudeRoot)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.WriteFileSync(filepath.Base(abs), []byte(text), 0o600)
}
