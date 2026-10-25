package importer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

// ContentsSkew is how much older than the snapshot the shared archive may be
// and still be used (RR14: the one archive serves every snapshot and its
// write is not atomic; the live 00:25 archive vs 00:41 `last` case).
const ContentsSkew = 5 * time.Minute

// maxArchive and maxMember bound what is read into memory.
const (
	maxArchive = 1 << 30
	maxMember  = 256 << 20
)

const archiveName = "pane_contents.tar.gz"

// attachContents maps the archive's `./pane_contents/pane-S:W.P` entries to
// panes ONCE, here, and re-keys them by each pane's saved_id, so nothing
// positional survives the import (§6.6). The archive is read with
// archive/tar + compress/gzip in memory; nothing is extracted to disk.
//
// data is the snapshot file's bytes: an explicit archive in another
// directory is linked to the snapshot only through a byte-identical copy of
// it there (G1-2-H3).
func attachContents(src Source, dir string, snapMtime time.Time, data []byte, res *Result) {
	rep := &res.Report
	file, explicit := src.Contents, true
	if file == "" {
		file, explicit = filepath.Join(dir, archiveName), false
	}
	st, err := os.Stat(file)
	if err != nil {
		if explicit || !errors.Is(err, fs.ErrNotExist) {
			rep.warnf("CONTENTS-UNREADABLE: %v", err)
		}
		return
	}
	if !pairs(src, file, explicit, dir, snapMtime, data, st.ModTime(), rep) {
		return
	}
	if st.Size() > maxArchive {
		rep.warnf("CONTENTS-TOO-LARGE: %d bytes; dropped", st.Size())
		return
	}
	archive, err := os.ReadFile(file)
	if err != nil {
		rep.warnf("CONTENTS-UNREADABLE: %v", err)
		return
	}
	entries, err := readArchive(archive)
	if err != nil {
		rep.warnf("CONTENTS-MALFORMED: %v; dropped", err)
		return
	}
	for i := range res.Snapshot.Panes {
		p := &res.Snapshot.Panes[i]
		if p.Role == snapshot.RoleTTObserver || p.Role == snapshot.RoleFrozen {
			continue // parity with save: their banner/sidebar is not contents (§6.1)
		}
		b, ok := entries[p.Key]
		if !ok {
			continue
		}
		res.Contents[p.SavedID] = b
		p.Contents = true
	}
	rep.ContentsUsed = len(res.Contents) > 0
	rep.ContentsPanes = len(res.Contents)
	if rep.ContentsUsed {
		res.Snapshot.Contents = &snapshot.Contents{Panes: len(res.Contents),
			CapturedAt: st.ModTime().Format(time.RFC3339)}
	}
}

// pairs decides whether the archive file (mtime at) belongs to the snapshot
// (dir, snapMtime, data), and warns when it does not.
//
// The archive is rewritten on EVERY resurrect save, while a snapshot file is
// written only when the layout changed (an identical one is deleted and
// `last` kept). So an archive newer than the snapshot is that snapshot's own
// while no newer snapshot file exists beside it — and once one does, an
// archive written after it is keyed by THAT layout's S:W.P and would give a
// pane another pane's scrollback (G1-H5).
//
// That reasoning holds only in the directory resurrect writes both into. An
// explicit archive elsewhere (`--from /backup/old.txt --contents
// /resurrect/pane_contents.tar.gz`) says nothing about the snapshot's own
// directory, so it is checked against ITS directory, through the copy of
// this snapshot found there; with no such copy nothing links the two and it
// is dropped (G1-2-H3). TrustContents skips every pairing check, with a
// warning: the caller vouches for the pairing (G1-2-L1).
func pairs(src Source, file string, explicit bool, dir string, snapMtime time.Time, data []byte, at time.Time, rep *Report) bool {
	if src.TrustContents {
		rep.warnf("CONTENTS-TRUSTED: %s attached without the pairing checks (--trust-contents)", file)
		return true
	}
	if adir := filepath.Dir(file); explicit && !sameDir(adir, dir) {
		tm, ok := twinIn(adir, data)
		if !ok {
			rep.warnf("CONTENTS-UNLINKED: the archive's directory holds no copy of this snapshot, so its S:W.P positions may name other panes; dropped (--trust-contents attaches it anyway)")
			return false
		}
		dir, snapMtime = adir, tm
	}
	if at.Before(snapMtime.Add(-ContentsSkew)) {
		rep.warnf("CONTENTS-STALE: archive %s older than the snapshot (more than %s); dropped (RR14)",
			snapMtime.Sub(at).Round(time.Second), ContentsSkew)
		return false
	}
	if n, ok := newerSnapshot(dir, snapMtime); ok && !at.Before(n.Add(-ContentsSkew)) {
		rep.warnf("CONTENTS-NEWER: archive written after a newer snapshot (%s); its positions belong to that layout; dropped (RR14)",
			n.Format(time.RFC3339))
		return false
	}
	return true
}

// sameDir reports whether a and b are the same directory (by identity, so a
// symlinked or differently spelled path still matches).
func sameDir(a, b string) bool {
	sa, err := os.Stat(a)
	if err != nil {
		return false
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(sa, sb)
}

// twinIn is the mtime of the newest resurrect snapshot file in dir whose
// bytes are data, if any.
func twinIn(dir string, data []byte) (time.Time, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, false
	}
	var best time.Time
	found := false
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "tmux_resurrect_") || !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil || fi.Size() != int64(len(data)) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || !bytes.Equal(b, data) {
			continue
		}
		if !found || fi.ModTime().After(best) {
			best, found = fi.ModTime(), true
		}
	}
	return best, found
}

// newerSnapshot is the mtime of the oldest resurrect snapshot file in dir
// that is newer than t (by more than a second), if any.
func newerSnapshot(dir string, t time.Time) (time.Time, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, false
	}
	var best time.Time
	found := false
	for _, e := range ents {
		if !strings.HasPrefix(e.Name(), "tmux_resurrect_") || !e.Type().IsRegular() {
			continue
		}
		fi, err := e.Info()
		if err != nil || !fi.ModTime().After(t.Add(time.Second)) {
			continue
		}
		if !found || fi.ModTime().Before(best) {
			best, found = fi.ModTime(), true
		}
	}
	return best, found
}

// readArchive returns the regular members under pane_contents/ keyed by the
// S:W.P after "pane-".
func readArchive(data []byte) (map[string][]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	tr := tar.NewReader(zr)
	out := map[string][]byte{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		name := strings.TrimPrefix(h.Name, "./")
		key, ok := strings.CutPrefix(name, "pane_contents/pane-")
		if !ok || key == "" || strings.Contains(key, "/") {
			continue
		}
		if h.Size > maxMember {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(tr, maxMember))
		if err != nil {
			return nil, err
		}
		out[key] = b
	}
}

// attachLive reads the live server's panes (read-only) and, for each row
// with no recording, the launch/<N> of the live pane at the same S:W.P. An
// empty recording is absent (B15). First line only (B1).
func attachLive(ctx context.Context, src Source, live Runner, res *Result) error {
	out, err := live.Run(ctx, "list-panes", "-a", "-F",
		"#{session_name}\x1f#{window_index}\x1f#{pane_index}\x1f#{pane_id}")
	if err != nil {
		return err
	}
	byKey := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, "\x1f")
		if len(f) != 4 {
			continue
		}
		byKey[f[0]+":"+f[1]+"."+f[2]] = f[3]
	}
	if src.Recordings == "" {
		res.Report.warnf("RECORDINGS-NO-DIR: no launch directory; nothing attached")
		return nil
	}
	if st, err := os.Stat(src.Recordings); err != nil || !st.IsDir() {
		res.Report.warnf("RECORDINGS-UNREADABLE: %s is not a directory", src.Recordings)
		return nil
	}
	for i := range res.Snapshot.Panes {
		p := &res.Snapshot.Panes[i]
		if p.Launch != "" {
			continue
		}
		id, ok := byKey[p.Key]
		if !ok || !strings.HasPrefix(id, "%") {
			continue
		}
		n, ok := atoi(id[1:])
		if !ok {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src.Recordings, fmt.Sprint(n)))
		if err != nil || len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		p.Launch = firstLine(string(b))
		res.Report.Recordings++
	}
	return nil
}
