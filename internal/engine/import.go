package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/engine/internal/importer"
	"github.com/MadAppGang/moox/internal/engine/internal/store"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/statedir"
)

// The importer half of the facade (§6.6, D12).
type (
	// ImportSource says what to import.
	ImportSource = importer.Source
	// ImportReport is what the import found.
	ImportReport = importer.Report
	// Imported is one import: snapshot, contents entries, report.
	Imported = importer.Result
	// ImportRunner runs the read-only list-panes of --attach-live.
	ImportRunner = importer.Runner
)

var (
	// ErrNotResurrect: the input is not a resurrect TSV (exit 4).
	ErrNotResurrect = importer.ErrNotResurrect
	// ErrNeedsReplaceLive: publishing onto the legacy default key needs
	// --replace-live (PR-H23).
	ErrNeedsReplaceLive = errors.New("engine: publishing an import on the default server needs --replace-live")
	// ErrNewerSave: a valid `source: "save"` snapshot newer than the import's
	// source file exists; publishing would supersede it (PR-H23).
	ErrNewerSave = errors.New("engine: a newer saved snapshot exists; the import would supersede it")
	// ErrProtectedOut: --out names a file that belongs to resurrect.
	ErrProtectedOut = errors.New("engine: --out may not name resurrect's own files")
)

// Import reads a resurrect snapshot (file or directory). live is read only
// with src.AttachLive; pass nil otherwise.
func Import(ctx context.Context, src ImportSource, live ImportRunner) (*Imported, error) {
	if src.AttachLive && live == nil {
		return nil, errors.New("engine: --attach-live needs a live server")
	}
	return importer.Import(ctx, src, live)
}

// IsResurrectFile reports a basename that belongs to resurrect (last,
// tmux_resurrect_*, pane_contents.tar.gz); moox never writes one (§12.4).
func IsResurrectFile(base string) bool {
	return base == "last" || base == "pane_contents.tar.gz" || strings.HasPrefix(base, "tmux_resurrect_")
}

// contentsName is the contents file written beside an --out snapshot.
func contentsName(out string) string {
	return strings.TrimSuffix(filepath.Base(out), ".json") + ".contents.gz"
}

// nameContents encodes the import's contents file and names it in the
// snapshot (file + sha256, RR14). An import has no server life: the header
// carries 0 0 and the snapshot's contents.server is null.
func nameContents(im *Imported, file string) ([]byte, error) {
	if len(im.Contents) == 0 {
		im.Snapshot.Contents = nil
		return nil, nil
	}
	data, err := snapshot.EncodeContents(snapshot.Server{}, im.Contents)
	if err != nil {
		return nil, err
	}
	c := im.Snapshot.Contents
	if c == nil {
		c = &snapshot.Contents{Panes: len(im.Contents)}
	}
	c.File, c.SHA256, c.Server = file, snapshot.SHA256(data), nil
	im.Snapshot.Contents = c
	return data, nil
}

// WriteImport writes the import to out (anywhere: V4 uses a scratch copy),
// with its contents file beside it, contents first, each temp + fsync +
// rename through statedir. It returns the paths written. claudeRoot is the
// Claude tree statedir refuses to write in.
func WriteImport(im *Imported, out, claudeRoot string) ([]string, error) {
	if im == nil || im.Snapshot == nil {
		return nil, errors.New("engine: nothing to write")
	}
	abs, err := filepath.Abs(out)
	if err != nil {
		return nil, err
	}
	base := filepath.Base(abs)
	if IsResurrectFile(base) {
		return nil, fmt.Errorf("%w: %s", ErrProtectedOut, base)
	}
	cname := contentsName(abs)
	if cname == base {
		cname = base + ".contents.gz"
	}
	contents, err := nameContents(im, cname)
	if err != nil {
		return nil, err
	}
	if err := snapshot.Validate(im.Snapshot); err != nil {
		return nil, err
	}
	body, err := snapshot.Encode(im.Snapshot)
	if err != nil {
		return nil, err
	}
	d, err := statedir.Open(filepath.Dir(abs), claudeRoot)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	var wrote []string
	if contents != nil {
		if err := d.WriteFileSync(cname, contents, 0o600); err != nil {
			return nil, err
		}
		wrote = append(wrote, d.Join(cname))
	}
	if err := d.WriteFileSync(base, body, 0o600); err != nil {
		return wrote, err
	}
	return append(wrote, d.Join(base)), nil
}

// PublishImportOptions guard a publish (PR-H23).
type PublishImportOptions struct {
	// ReplaceLive: required on the legacy default key, and to supersede a
	// newer `source: "save"` snapshot.
	ReplaceLive bool
}

// PublishImportResult names what a publish wrote and what it superseded.
type PublishImportResult struct {
	File       string // the published moox-<ts>.json basename
	Superseded string // the moox-last it replaced, or ""
}

// Superseded is the snapshot a publish would supersede (moox-last if it
// validates), for the confirmation `moox import --publish` asks.
func (e *Engine) Superseded() (name string, snap *snapshot.Snapshot) {
	st, err := store.Open(e.paths, e.claudeRoot)
	if err != nil {
		return "", nil
	}
	defer func() { _ = st.Close() }()
	last, err := st.Last()
	if err != nil {
		return "", nil
	}
	return last.Name, last.Snapshot
}

// PublishImport publishes the import as this server's moox-last through
// store.Publish under save.lock (§6.6). On the legacy default key it needs
// ReplaceLive; without ReplaceLive it refuses whenever the current valid
// snapshot is a `source: "save"` one newer than the import's source file
// (heal never prefers an import over a newer save either, §6.2).
func (e *Engine) PublishImport(ctx context.Context, im *Imported, o PublishImportOptions) (PublishImportResult, error) {
	if im == nil || im.Snapshot == nil {
		return PublishImportResult{}, errors.New("engine: nothing to publish")
	}
	if e.paths.Legacy && !o.ReplaceLive {
		return PublishImportResult{}, ErrNeedsReplaceLive
	}
	if err := ctx.Err(); err != nil {
		return PublishImportResult{}, err
	}
	st, err := store.Open(e.paths, e.claudeRoot)
	if err != nil {
		return PublishImportResult{}, err
	}
	defer func() { _ = st.Close() }()
	l, err := st.TryLock(e.record())
	if err != nil {
		return PublishImportResult{}, err
	}
	defer l.Release()

	var res PublishImportResult
	last, err := st.Last()
	switch {
	case err == nil:
		res.Superseded = last.Name
		if !o.ReplaceLive && newerSave(last.Snapshot, im.Report.SourceMtime) {
			return PublishImportResult{}, fmt.Errorf("%w: %s", ErrNewerSave, last.Name)
		}
	case errors.Is(err, ErrNewerFormat) && !o.ReplaceLive:
		return PublishImportResult{}, err
	}
	im.Snapshot.Socket = snapshot.Socket{Name: sockets.Name(e.paths.Socket), Key: e.paths.Key}
	jsonName, cname := st.NewNames(e.now())
	contents, err := nameContents(im, cname)
	if err != nil {
		return PublishImportResult{}, err
	}
	if err := st.Publish(l, im.Snapshot, jsonName, cname, contents); err != nil {
		return PublishImportResult{}, err
	}
	res.File = jsonName
	e.logf("IMPORT-PUBLISHED %s from %s (%d panes, %d windows; superseded %q)", jsonName,
		filepath.Base(im.Report.SourceFile), im.Report.Panes, im.Report.Windows, res.Superseded)
	return res, nil
}

// newerSave: snap is a saved (not imported) snapshot created after t.
func newerSave(snap *snapshot.Snapshot, t time.Time) bool {
	if snap == nil || snap.Source != snapshot.SourceSave {
		return false
	}
	c, err := time.Parse(time.RFC3339, snap.CreatedAt)
	if err != nil {
		return true // unknown age: fail safe, keep the save
	}
	return c.After(t)
}
