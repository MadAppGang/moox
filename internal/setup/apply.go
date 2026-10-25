package setup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/MadAppGang/moox/internal/setup/manifest"
	"github.com/MadAppGang/moox/internal/statedir"
)

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// dirs hands out one verified directory handle per parent directory.
type dirs struct {
	claude string
	open   map[string]*statedir.Dir
}

func newDirs(env Env) *dirs { return &dirs{claude: env.claudeRoot(), open: map[string]*statedir.Dir{}} }

func (d *dirs) get(dir string) (*statedir.Dir, error) {
	if h, ok := d.open[dir]; ok {
		return h, nil
	}
	var h *statedir.Dir
	var err error
	if statedir.Within(dir, d.claude) {
		// ~/.claude itself: the one write into the Claude tree, with the
		// user's explicit consent (F5, §2.4 RestrictedCalls).
		h, err = statedir.OpenUserConfig(dir)
	} else {
		h, err = statedir.Open(dir, d.claude)
	}
	if err != nil {
		return nil, err
	}
	d.open[dir] = h
	return h, nil
}

func (d *dirs) close() {
	for _, h := range d.open {
		_ = h.Close()
	}
}

// Apply carries out every Add action of p, in order, and writes the
// manifest (§8.3 step 5–6). It returns one line per action. A machine that
// is already set up gets "already" for everything and no new manifest.
func Apply(env Env, f Facts, p Plan) ([]string, error) {
	var report []string
	if !p.Pending() {
		for _, a := range p.Actions {
			report = append(report, a.Describe())
		}
		return report, nil
	}
	ds := newDirs(env)
	defer ds.close()

	m := f.Manifest
	if m == nil {
		m = &manifest.Manifest{Version: manifest.Version, CreatedAt: env.now().UTC().Format("2006-01-02T15:04:05Z"),
			MooxVersion: env.MooxVersion}
	}
	m.Mode = string(p.Mode)
	m.MooxVersion = env.MooxVersion

	if f.Manifest == nil {
		for _, d := range missingDirs(env.mooxConfig(), env.Home) {
			m.Actions = append(m.Actions, manifest.Action{Kind: manifest.KindMkdir, Path: d})
		}
	}
	var backupDir *statedir.Dir
	backups := func() (*statedir.Dir, error) {
		if backupDir != nil {
			return backupDir, nil
		}
		cfg, err := ds.get(env.mooxConfig())
		if err != nil {
			return nil, err
		}
		root, err := cfg.OpenSub("backups")
		if err != nil {
			return nil, err
		}
		defer func() { _ = root.Close() }()
		stamp := env.now().Format("20060102T150405")
		name := stamp
		for i := 1; ; i++ {
			if _, err := root.Lstat(name); errors.Is(err, fs.ErrNotExist) {
				break
			}
			name = fmt.Sprintf("%s.%d", stamp, i)
		}
		backupDir, err = root.OpenSub(name)
		if err != nil {
			return nil, err
		}
		ds.open[backupDir.Path()] = backupDir
		return backupDir, nil
	}
	// writeManifest is called after every step, so a failure part-way still
	// leaves a manifest that uninstall can walk back.
	writeManifest := func() error {
		cfg, err := ds.get(env.mooxConfig())
		if err != nil {
			return err
		}
		b, err := manifest.Encode(m)
		if err != nil {
			return err
		}
		return cfg.WriteFileSync(manifest.FileName, b, 0o600)
	}

	for _, a := range p.Actions {
		if a.State != Add || a.Kind == "note" {
			report = append(report, a.Describe())
			continue
		}
		for _, d := range missingDirs(filepath.Dir(a.Path), env.Home) {
			m.Actions = append(m.Actions, manifest.Action{Kind: manifest.KindMkdir, Path: d})
		}
		rec, err := applyOne(ds, a, backups)
		if err != nil {
			report = append(report, "FAILED: "+a.Describe()+": "+err.Error())
			_ = writeManifest()
			return report, fmt.Errorf("setup: %s: %w", a.Path, err)
		}
		m.Actions = append(m.Actions, rec)
		report = append(report, "done: "+a.Describe())
		if err := writeManifest(); err != nil {
			return report, fmt.Errorf("setup: writing the manifest: %w", err)
		}
	}
	return report, nil
}

func applyOne(ds *dirs, a Action, backups func() (*statedir.Dir, error)) (manifest.Action, error) {
	dir, base := filepath.Split(a.Path)
	dir = filepath.Clean(dir)
	rec := manifest.Action{Kind: a.Kind, Path: a.Path}
	switch a.Kind {
	case manifest.KindBackup:
		src, err := ds.get(dir)
		if err != nil {
			return rec, err
		}
		b, err := backups()
		if err != nil {
			return rec, err
		}
		// The ENTRY moves — a symlink as a symlink, its target untouched.
		if err := statedir.RenameAt(src, base, b, base); err != nil {
			return rec, err
		}
		rec.Backup = filepath.Join(b.Path(), base)
	case manifest.KindWrite:
		d, err := ds.get(dir)
		if err != nil {
			return rec, err
		}
		if _, err := d.Lstat(base); err == nil {
			return rec, fmt.Errorf("%s appeared since it was checked; not overwritten", a.Path)
		}
		if err := d.WriteFileSync(base, a.Content, 0o600); err != nil {
			return rec, err
		}
		rec.SHA256Written = sum(a.Content)
	case manifest.KindAppend:
		d, err := ds.get(dir)
		if err != nil {
			return rec, err
		}
		cur, _, err := d.ReadFile(base)
		if err != nil {
			return rec, err
		}
		text := a.Line
		if len(cur) > 0 && !bytes.HasSuffix(cur, []byte("\n")) {
			text = "\n" + text
		}
		w, err := d.OpenAppend(base)
		if err != nil {
			return rec, err
		}
		_, werr := w.WriteString(text + "\n")
		cerr := w.Close()
		if werr != nil {
			return rec, werr
		}
		if cerr != nil {
			return rec, cerr
		}
		rec.Line = text
	case manifest.KindClaudeMerge:
		c := a.claude
		if c == nil {
			return rec, errors.New("no merge planned")
		}
		d, err := ds.get(c.dir)
		if err != nil {
			return rec, err
		}
		cur, _, rerr := d.ReadFile(c.name)
		switch {
		case errors.Is(rerr, fs.ErrNotExist):
			cur = nil
		case rerr != nil:
			return rec, rerr
		}
		if !bytes.Equal(cur, c.before) {
			return rec, errors.New("settings.json changed since it was read; run moox setup again")
		}
		if c.before != nil {
			b, err := backups()
			if err != nil {
				return rec, err
			}
			if err := b.WriteFileSync(c.name, c.before, 0o600); err != nil {
				return rec, err
			}
			rec.Backup = filepath.Join(b.Path(), c.name)
			if err := d.ReplaceKeepMode(c.name, c.after); err != nil {
				return rec, err
			}
		} else if err := d.WriteFileSync(c.name, c.after, 0o600); err != nil {
			return rec, err
		}
		rec.SHA256Written = sum(c.after)
	default:
		return rec, fmt.Errorf("unknown action %q", a.Kind)
	}
	return rec, nil
}

// missingDirs are the components of dir (below an existing ancestor) that do
// not exist yet, outermost first: the directories a write will create.
func missingDirs(dir, home string) []string {
	var out []string
	for d := filepath.Clean(dir); d != "/" && d != "." && d != filepath.Dir(d); d = filepath.Dir(d) {
		if _, err := os.Lstat(d); err == nil {
			break
		}
		out = append([]string{d}, out...)
	}
	return out
}

// UninstallOptions configure Uninstall.
type UninstallOptions struct {
	DryRun bool
	// Purge also removes XS/moox and XC/moox (never the legacy default paths).
	Purge     bool
	StateHome string // XS
}

// ErrNotSetUp: there is no manifest to walk.
var ErrNotSetUp = errors.New("setup: no manifest (moox setup has not run here)")

// Uninstall walks the manifest in reverse (§8.3): backups renamed back,
// written files removed only while their sha256 still matches, appended
// lines removed exactly, settings.json restored from its backup only if
// unchanged since the merge (else moox's entries are removed by value).
func Uninstall(env Env, o UninstallOptions) ([]string, error) {
	b, err := os.ReadFile(env.ManifestPath())
	if errors.Is(err, fs.ErrNotExist) {
		if o.Purge {
			return purge(env, o)
		}
		return nil, ErrNotSetUp
	}
	if err != nil {
		return nil, err
	}
	m, err := manifest.Decode(b)
	if err != nil {
		return nil, err
	}
	ds := newDirs(env)
	defer ds.close()
	var report []string
	failed := false
	say := func(format string, a ...any) { report = append(report, fmt.Sprintf(format, a...)) }
	u := &undoer{ds: ds, env: env, dry: o.DryRun, gone: map[string]bool{}}
	// The manifest goes before the directories it sits in are removed.
	removeManifest := func() {
		if cfg, err := ds.get(env.mooxConfig()); err == nil {
			if err := cfg.Remove(manifest.FileName, statedir.TypeRegular, nil); err == nil {
				say("removed %s", env.ManifestPath())
			}
		}
	}
	manifestGone := false
	for i := len(m.Actions) - 1; i >= 0; i-- {
		a := m.Actions[i]
		if a.Kind == manifest.KindMkdir && statedir.Within(env.mooxConfig(), a.Path) && !manifestGone && !o.DryRun && !failed {
			removeEmptyDirs(ds, env, filepath.Join(env.mooxConfig(), "backups"))
			removeManifest()
			manifestGone = true
		}
		line, err := u.undo(a)
		if err != nil {
			failed = true
			say("FAILED: %s %s: %v", a.Kind, a.Path, err)
			continue
		}
		say("%s", line)
	}
	if o.DryRun {
		return report, nil
	}
	if !failed && !manifestGone {
		removeEmptyDirs(ds, env, filepath.Join(env.mooxConfig(), "backups"))
		removeManifest()
	} else if failed && !manifestGone {
		say("kept %s: some steps failed (see above)", env.ManifestPath())
	}
	if o.Purge {
		more, err := purge(env, o)
		report = append(report, more...)
		if err != nil {
			return report, err
		}
	}
	if failed {
		return report, errors.New("setup: uninstall left some steps undone")
	}
	return report, nil
}

type undoer struct {
	ds   *dirs
	env  Env
	dry  bool
	gone map[string]bool // dry run: paths an earlier step would have removed
}

func (u *undoer) exists(p string) bool {
	if u.gone[p] {
		return false
	}
	_, err := os.Lstat(p)
	return err == nil
}

func (u *undoer) undo(a manifest.Action) (string, error) {
	ds, env, dry := u.ds, u.env, u.dry
	dir, base := filepath.Split(a.Path)
	dir = filepath.Clean(dir)
	switch a.Kind {
	case manifest.KindMkdir:
		names, err := os.ReadDir(a.Path)
		if errors.Is(err, fs.ErrNotExist) {
			return "already gone: " + a.Path, nil
		}
		if err != nil {
			return "", err
		}
		left := 0
		for _, n := range names {
			if !u.gone[filepath.Join(a.Path, n.Name())] {
				left++
			}
		}
		if left > 0 {
			return "kept " + a.Path + ": it is not empty", nil
		}
		if dry {
			u.gone[a.Path] = true
			return "would remove the directory " + a.Path, nil
		}
		parent, err := statedir.OpenExisting(dir, env.claudeRoot())
		if err != nil {
			return "", err
		}
		defer func() { _ = parent.Close() }()
		delete(ds.open, a.Path)
		if err := parent.RemoveTree(base); err != nil {
			return "", err
		}
		return "removed the directory " + a.Path, nil
	case manifest.KindWrite:
		cur, err := os.ReadFile(a.Path)
		if errors.Is(err, fs.ErrNotExist) {
			return "already gone: " + a.Path, nil
		}
		if err != nil {
			return "", err
		}
		if sum(cur) != a.SHA256Written {
			return "kept " + a.Path + ": it changed since moox wrote it", nil
		}
		if dry {
			u.gone[a.Path] = true
			return "would remove " + a.Path, nil
		}
		d, err := ds.get(dir)
		if err != nil {
			return "", err
		}
		if err := d.Remove(base, statedir.TypeRegular, nil); err != nil {
			return "", err
		}
		return "removed " + a.Path, nil
	case manifest.KindBackup:
		if a.Backup == "" {
			return "", errors.New("no backup path recorded")
		}
		if _, err := os.Lstat(a.Backup); err != nil {
			return "", fmt.Errorf("backup %s: %w", a.Backup, err)
		}
		if u.exists(a.Path) {
			return "kept the backup " + a.Backup + ": " + a.Path + " exists again", nil
		}
		if dry {
			u.gone[a.Backup] = true
			return "would restore " + a.Path + " from " + a.Backup, nil
		}
		bdir, bbase := filepath.Split(a.Backup)
		src, err := ds.get(filepath.Clean(bdir))
		if err != nil {
			return "", err
		}
		dst, err := ds.get(dir)
		if err != nil {
			return "", err
		}
		if err := statedir.RenameAt(src, bbase, dst, base); err != nil {
			return "", err
		}
		return "restored " + a.Path, nil
	case manifest.KindAppend:
		cur, err := os.ReadFile(a.Path)
		if errors.Is(err, fs.ErrNotExist) {
			return "already gone: " + a.Path, nil
		}
		if err != nil {
			return "", err
		}
		needle := a.Line + "\n"
		i := strings.LastIndex(string(cur), needle)
		if i < 0 {
			return "nothing to remove from " + a.Path + ": the moox line is not there", nil
		}
		if dry {
			return "would remove the moox line from " + a.Path, nil
		}
		out := append(append([]byte{}, cur[:i]...), cur[i+len(needle):]...)
		d, err := ds.get(dir)
		if err != nil {
			return "", err
		}
		if err := d.ReplaceKeepMode(base, out); err != nil {
			return "", err
		}
		return "removed the moox line from " + a.Path, nil
	case manifest.KindClaudeMerge:
		cur, err := os.ReadFile(a.Path)
		if errors.Is(err, fs.ErrNotExist) {
			return "already gone: " + a.Path, nil
		}
		if err != nil {
			return "", err
		}
		d, err := ds.get(dir)
		if err != nil {
			return "", err
		}
		if sum(cur) == a.SHA256Written {
			if a.Backup == "" { // setup created the file
				if dry {
					u.gone[a.Path] = true
					return "would remove " + a.Path, nil
				}
				return "removed " + a.Path, d.Remove(base, statedir.TypeRegular, nil)
			}
			orig, err := os.ReadFile(a.Backup)
			if err != nil {
				return "", fmt.Errorf("backup %s: %w", a.Backup, err)
			}
			if dry {
				return "would restore " + a.Path + " from " + a.Backup, nil
			}
			if err := d.ReplaceKeepMode(base, orig); err != nil {
				return "", err
			}
			bdir, bbase := filepath.Split(a.Backup)
			if bd, err := ds.get(filepath.Clean(bdir)); err == nil {
				_ = bd.Remove(bbase, statedir.TypeRegular, nil)
			}
			return "restored " + a.Path, nil
		}
		out, removed, err := RemoveClaudeHooks(cur, env.Root)
		if err != nil {
			return "", err
		}
		if !removed {
			return "nothing to remove from " + a.Path + ": the moox hooks are not there", nil
		}
		if dry {
			return "would remove the moox hooks from " + a.Path + " (it changed since the merge)", nil
		}
		if err := d.ReplaceKeepMode(base, out); err != nil {
			return "", err
		}
		return "removed the moox hooks from " + a.Path + " (it changed since the merge; the rest is kept)", nil
	}
	return "", fmt.Errorf("unknown action %q", a.Kind)
}

// removeEmptyDirs removes backups/<ts> directories the restore emptied, then
// backups/ itself if empty.
func removeEmptyDirs(ds *dirs, env Env, root string) {
	parent, err := ds.get(env.mooxConfig())
	if err != nil {
		return
	}
	b, err := statedir.OpenExisting(root, env.claudeRoot())
	if err != nil {
		return
	}
	names, _ := b.ReadDirNames()
	left := 0
	for _, n := range names {
		sub, err := b.OpenSub(n)
		if err != nil {
			left++
			continue
		}
		inner, _ := sub.ReadDirNames()
		_ = sub.Close()
		if len(inner) == 0 {
			_ = b.RemoveTree(n)
		} else {
			left++
		}
	}
	_ = b.Close()
	if left == 0 {
		_ = parent.RemoveTree("backups")
	}
}

func purge(env Env, o UninstallOptions) ([]string, error) {
	var report []string
	for _, p := range []string{filepath.Join(o.StateHome, "moox"), env.mooxConfig()} {
		if o.StateHome == "" && strings.HasPrefix(p, "moox") {
			continue
		}
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		if o.DryRun {
			report = append(report, "would remove "+p)
			continue
		}
		parent, err := statedir.OpenExisting(filepath.Dir(p), env.claudeRoot())
		if err != nil {
			return report, err
		}
		err = parent.RemoveTree(filepath.Base(p))
		_ = parent.Close()
		if err != nil {
			return report, err
		}
		report = append(report, "removed "+p)
	}
	return report, nil
}
