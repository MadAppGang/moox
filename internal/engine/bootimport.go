package engine

// bootimport.go is D12's migration at boot (§6.2 "Import availability",
// PR-M4, R2-H10): the first moox boot on a socket whose snapshot directory
// holds no valid moox snapshot, but does hold resurrect's `last`, imports
// that file, PUBLISHES the import as moox-<ts>.json (source
// "import-resurrect") under the save.lock the restore already holds, points
// moox-last at it, and only then restores from that file. So a pin and the
// clobber guard always have a real moox predecessor, and ImportAvailable
// turns false because a moox file now exists.
//
// On the legacy default key the snapshot directory IS resurrect's
// (~/.local/share/tmux/resurrect); on any other key it is the per-socket
// moox dir, where a resurrect `last` exists only if someone put it there.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/MadAppGang/moox/internal/engine/internal/store"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// errNoImport: the snapshot directory has no resurrect `last` to import.
var errNoImport = errors.New("engine: no resurrect snapshot to import")

// resurrectLast is resurrect's pointer in the socket's snapshot directory.
func (e *Engine) resurrectLast() string { return filepath.Join(e.paths.Snapshots, "last") }

// bootImport imports resurrect's `last` in memory (read-only; nothing is
// written). errNoImport when there is no `last` at all.
func (e *Engine) bootImport(ctx context.Context) (*Imported, error) {
	if _, err := os.Lstat(e.resurrectLast()); errors.Is(err, fs.ErrNotExist) {
		return nil, errNoImport
	}
	env := e.opts.Env
	if env == nil {
		env = tmuxtarget.OSEnv{}
	}
	home := ""
	if r, err := sockets.RootsFrom(env); err == nil {
		home = r.Home
	}
	im, err := Import(ctx, ImportSource{From: e.paths.Snapshots, Home: home, Producer: e.opts.Producer}, nil)
	if err != nil {
		return nil, err
	}
	im.Snapshot.Socket = snapshot.Socket{Name: sockets.Name(e.paths.Socket), Key: e.paths.Key}
	if err := snapshot.Validate(im.Snapshot); err != nil {
		return nil, err
	}
	return im, nil
}

// importAvailable is the gate's check 5 alternative: no valid moox snapshot
// and an importable resurrect `last`.
func (e *Engine) importAvailable(ctx context.Context) (*Imported, bool) {
	im, err := e.bootImport(ctx)
	if err != nil {
		if !errors.Is(err, errNoImport) {
			e.logf("IMPORT-UNAVAILABLE %s: %v", e.resurrectLast(), err)
		}
		return nil, false
	}
	return im, true
}

// publishBootImport publishes the boot import under the held lock and
// returns the published basename.
func (e *Engine) publishBootImport(ctx context.Context, st *store.Store, l *store.Lock) (string, error) {
	im, err := e.bootImport(ctx)
	if err != nil {
		return "", err
	}
	jsonName, cname := st.NewNames(e.now())
	contents, err := nameContents(im, cname)
	if err != nil {
		return "", err
	}
	if contents == nil {
		cname = ""
	}
	if err := st.Publish(l, im.Snapshot, jsonName, cname, contents); err != nil {
		return "", fmt.Errorf("engine: publishing the boot import: %w", err)
	}
	e.logf("IMPORT-PUBLISHED %s from %s before the boot restore (%d panes, %d windows, %d sessions)", jsonName,
		filepath.Base(im.Report.SourceFile), im.Report.Panes, im.Report.Windows, im.Report.Sessions)
	_ = st.AppendLog("restore.log", e.now().Format("2006-01-02T15:04:05Z07:00")+" IMPORT-PUBLISHED "+jsonName+
		" from "+filepath.Base(im.Report.SourceFile))
	return jsonName, nil
}
