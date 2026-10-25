// Package store is where snapshots live on disk (architecture §6.2 steps 1,
// 10–12, §5.6): the save lock, publish (temp + fsync + renameat + fsync(dir),
// contents first, then the snapshot, then the pointer), the moox-last pointer
// and its heal, retention, the pins set and restoring-from, and the clobber
// guard. Every write goes through statedir, the module's one writer.
//
// Names this package ranks, keeps or deletes match
// ^moox-\d{8}T\d{6}(\.\d+)?\.(json|contents\.gz)$ and nothing else: on the
// legacy default key the directory is resurrect's, and resurrect's `last`,
// tmux_resurrect_*.txt and pane_contents.tar.gz are never candidates for
// anything (A6).
package store

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/statedir"
)

// File names in the snapshot and state directories.
const (
	PointerName       = "moox-last"
	LockName          = "save.lock"
	PinsName          = "pins"
	RestoringFromName = "restoring-from"
	ClobberLogName    = "clobber-guard.log"
)

// HealCandidates is B19's bound: heal looks at no more than 12 snapshots.
const HealCandidates = 12

var (
	// ErrNoSnapshot means no moox snapshot validates.
	ErrNoSnapshot = errors.New("store: no valid snapshot")
	// ErrBusy is statedir's lock-held error: another save holds save.lock.
	ErrBusy = statedir.ErrBusy
)

var nameRe = regexp.MustCompile(`^moox-(\d{8}T\d{6})(?:\.(\d+))?\.(json|contents\.gz)$`)

// stampLayout is the timestamp in a snapshot name.
const stampLayout = "20060102T150405"

// Name is a parsed snapshot or contents file name.
type Name struct {
	Base     string
	Stamp    time.Time // local time
	Seq      int       // the ".N" collision suffix; 0 for none
	Contents bool
}

// ParseName parses a moox name; ok is false for anything else (resurrect's
// files above all).
func ParseName(name string) (Name, bool) {
	m := nameRe.FindStringSubmatch(name)
	if m == nil {
		return Name{}, false
	}
	ts, err := time.ParseInLocation(stampLayout, m[1], time.Local)
	if err != nil {
		return Name{}, false
	}
	seq := 0
	if m[2] != "" {
		seq, _ = strconv.Atoi(m[2])
		seq++ // ".0" sorts after the bare name
	}
	return Name{Base: name, Stamp: ts, Seq: seq, Contents: m[3] == "contents.gz"}, true
}

// newer orders names newest first.
func newer(a, b Name) bool {
	if !a.Stamp.Equal(b.Stamp) {
		return a.Stamp.After(b.Stamp)
	}
	return a.Seq > b.Seq
}

// Store is one socket's snapshot directory and state directory.
type Store struct {
	snaps      *statedir.Dir
	state      *statedir.Dir
	paths      sockets.Paths
	claudeRoot string

	// onWrite is a test hook called before each durable write of publish.
	onWrite func(name string)
}

// Open opens (creating 0700 if missing) the socket's snapshot and state
// directories. claudeRoot is the Claude tree statedir refuses to write in.
func Open(p sockets.Paths, claudeRoot string) (*Store, error) {
	snaps, err := statedir.Open(p.Snapshots, claudeRoot)
	if err != nil {
		return nil, fmt.Errorf("store: snapshots dir: %w", err)
	}
	state, err := statedir.Open(p.State, claudeRoot)
	if err != nil {
		_ = snaps.Close()
		return nil, fmt.Errorf("store: state dir: %w", err)
	}
	return &Store{snaps: snaps, state: state, paths: p, claudeRoot: claudeRoot}, nil
}

// Close releases the directory handles.
func (s *Store) Close() error {
	return errors.Join(s.snaps.Close(), s.state.Close())
}

// SnapshotDir is the snapshot directory's path.
func (s *Store) SnapshotDir() string { return s.snaps.Path() }

// Path is the absolute path of a snapshot-directory entry.
func (s *Store) Path(name string) string { return s.snaps.Join(name) }

// names lists the moox names in the snapshot directory, newest first.
func (s *Store) names() ([]Name, error) {
	all, err := s.snaps.ReadDirNames()
	if err != nil {
		return nil, err
	}
	var out []Name
	for _, n := range all {
		if p, ok := ParseName(n); ok {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return newer(out[i], out[j]) })
	return out, nil
}

// NewNames picks the names of a new snapshot and its contents file, stamped
// now, with a ".N" suffix if a save in the same second already took them.
func (s *Store) NewNames(now time.Time) (jsonName, contentsName string) {
	base := "moox-" + now.Format(stampLayout)
	for i := -1; ; i++ {
		b := base
		if i >= 0 {
			b = base + "." + strconv.Itoa(i)
		}
		_, e1 := s.snaps.Lstat(b + ".json")
		_, e2 := s.snaps.Lstat(b + ".contents.gz")
		if errors.Is(e1, fs.ErrNotExist) && errors.Is(e2, fs.ErrNotExist) {
			return b + ".json", b + ".contents.gz"
		}
	}
}

// ---------------------------------------------------------------------------
// read side
// ---------------------------------------------------------------------------

// Load reads, decodes and validates one snapshot by name.
func (s *Store) Load(name string) (*snapshot.Snapshot, error) {
	if p, ok := ParseName(name); !ok || p.Contents {
		return nil, fmt.Errorf("%w: %q is not a moox snapshot name", snapshot.ErrInvalid, name)
	}
	b, _, err := s.snaps.ReadFile(name)
	if err != nil {
		return nil, err
	}
	snap, err := snapshot.Decode(b)
	if err != nil {
		return nil, err
	}
	if err := snapshot.Validate(snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// Pointer is moox-last's basename, or "" if it is missing or malformed.
func (s *Store) Pointer() string {
	b, _, err := s.snaps.ReadFile(PointerName)
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(b))
	if p, ok := ParseName(name); !ok || p.Contents {
		return ""
	}
	return name
}

// Last is the read side's answer.
type Last struct {
	Name     string
	Snapshot *snapshot.Snapshot
	// Healed: moox-last did not name a valid snapshot and Name was found by
	// the heal scan instead; the caller rewrites the pointer under the lock
	// (Heal) and logs HEALED.
	Healed bool
	// Pointer is what moox-last said.
	Pointer string
}

// Last reads moox-last, then the file it names, then Decode, then Validate.
// On any failure it scans moox-*.json newest first (at most 12 candidates,
// B19; resurrect files are never candidates) and takes the first that
// validates (H1–H3, RR11). It writes nothing.
//
// A snapshot written by a newer moox (snapshot.ErrNewerFormat) is never
// read past: when moox-last names one, or it is the newest candidate of the
// scan, Last returns that error, so a downgraded binary refuses instead of
// treating the workspace as absent and publishing over it (G1-H4).
//
// Among candidates a `source: "save"` snapshot beats an older
// `source: "import-resurrect"` one (§6.2, PR-H23): an import is named by its
// publish time but carries the resurrect file's mtime as created_at, so the
// scan goes on past an import for a save created after it (G1-H8).
func (s *Store) Last() (Last, error) {
	ptr := s.Pointer()
	if ptr != "" {
		snap, err := s.Load(ptr)
		if err == nil {
			return Last{Name: ptr, Snapshot: snap, Pointer: ptr}, nil
		}
		if errors.Is(err, snapshot.ErrNewerFormat) {
			return Last{Pointer: ptr}, fmt.Errorf("store: moox-last %s: %w", ptr, err)
		}
	}
	names, err := s.names()
	if err != nil {
		return Last{Pointer: ptr}, err
	}
	var pick *Last
	tried := 0
	for _, n := range names {
		if n.Contents {
			continue
		}
		if tried == HealCandidates {
			break
		}
		tried++
		snap, err := s.Load(n.Base)
		if err != nil {
			if pick == nil && errors.Is(err, snapshot.ErrNewerFormat) {
				return Last{Pointer: ptr}, fmt.Errorf("store: %s: %w", n.Base, err)
			}
			continue
		}
		if pick == nil {
			pick = &Last{Name: n.Base, Snapshot: snap, Healed: true, Pointer: ptr}
			if snap.Source != snapshot.SourceImport {
				break
			}
			continue
		}
		if snap.Source == snapshot.SourceSave {
			// The first save under an import decides: older saves are older
			// still.
			if createdAfter(snap, pick.Snapshot) {
				pick = &Last{Name: n.Base, Snapshot: snap, Healed: true, Pointer: ptr}
			}
			break
		}
	}
	if pick == nil {
		return Last{Pointer: ptr}, ErrNoSnapshot
	}
	return *pick, nil
}

// createdAfter reports a.created_at > b.created_at; an unparsable a is
// treated as newer (fail safe: keep the save).
func createdAfter(a, b *snapshot.Snapshot) bool {
	ta, err := time.Parse(time.RFC3339, a.CreatedAt)
	if err != nil {
		return true
	}
	tb, err := time.Parse(time.RFC3339, b.CreatedAt)
	if err != nil {
		return true
	}
	return ta.After(tb)
}

// Heal rewrites moox-last to name (a heal found by Last).
func (s *Store) Heal(l *Lock, name string) error {
	if err := l.check(); err != nil {
		return err
	}
	return s.snaps.WriteFileSync(PointerName, []byte(name+"\n"), 0o600)
}

// ---------------------------------------------------------------------------
// publish
// ---------------------------------------------------------------------------

// Publish writes one snapshot (I2, R21, RR2/RR3): the contents file first (if
// this save captured one), then the snapshot, then moox-last — each temp +
// fsync + renameat + fsync(dir) — so the pointer is always a complete pointer
// to a complete, already-validated file, and a snapshot never names a
// contents file that is not on disk. An invalid snapshot is never written
// (B19). Immediately before each rename the lock's same-inode test is
// repeated, so even an external `rm save.lock` cannot let a second publisher
// through (§5.6).
func (s *Store) Publish(l *Lock, snap *snapshot.Snapshot, jsonName string, contentsName string, contents []byte) error {
	if err := snapshot.Validate(snap); err != nil {
		return err
	}
	if p, ok := ParseName(jsonName); !ok || p.Contents {
		return fmt.Errorf("store: %q is not a snapshot name", jsonName)
	}
	body, err := snapshot.Encode(snap)
	if err != nil {
		return err
	}
	if contents != nil {
		if snap.Contents == nil || snap.Contents.File != contentsName || snap.Contents.SHA256 != snapshot.SHA256(contents) {
			return errors.New("store: the snapshot does not name the contents file it is published with")
		}
		if err := s.write(l, contentsName, contents); err != nil {
			return err
		}
	}
	if err := s.write(l, jsonName, body); err != nil {
		return err
	}
	return s.write(l, PointerName, []byte(jsonName+"\n"))
}

func (s *Store) write(l *Lock, name string, data []byte) error {
	if err := l.check(); err != nil {
		return err
	}
	if s.onWrite != nil {
		s.onWrite(name)
	}
	return s.snaps.WriteFileSync(name, data, 0o600)
}

// ---------------------------------------------------------------------------
// contents
// ---------------------------------------------------------------------------

// ContentsLimit bounds one contents file read, at save (reuse) and at
// restore alike (P3: a file holds every pane's full history, R2-C7, and is
// far larger than statedir's 8 MiB default for small documents). A save that
// read through the default limit published every structural save after a
// large capture with no scrollback at all (G1-H1).
const ContentsLimit = 512 << 20

// NewestContents is the newest moox-*.contents.gz and its bytes, for a
// structural save's reuse (§6.1); found=false if there is none. err is set
// when the newest file exists but cannot be read, so the caller can say why
// a save carries no contents.
func (s *Store) NewestContents() (name string, data []byte, found bool, err error) {
	names, err := s.names()
	if err != nil {
		return "", nil, false, err
	}
	for _, n := range names {
		if !n.Contents {
			continue
		}
		b, _, err := s.snaps.ReadFileLimit(n.Base, ContentsLimit)
		if err != nil {
			return n.Base, nil, false, err
		}
		return n.Base, b, true, nil
	}
	return "", nil, false, nil
}

// ErrContentsMismatch: the contents file a snapshot names is missing or is
// not the file the snapshot was built with (its sha256 differs). Restore then
// drops contents only (RR14).
var ErrContentsMismatch = errors.New("store: contents file does not match the snapshot")

// LoadContents reads the contents a snapshot names, verified by sha256.
func (s *Store) LoadContents(snap *snapshot.Snapshot) (map[string][]byte, error) {
	if snap.Contents == nil || snap.Contents.File == "" {
		return nil, fmt.Errorf("%w: the snapshot names no contents file", ErrContentsMismatch)
	}
	if p, ok := ParseName(snap.Contents.File); !ok || !p.Contents {
		return nil, fmt.Errorf("%w: %q is not a contents name", ErrContentsMismatch, snap.Contents.File)
	}
	b, _, err := s.snaps.ReadFileLimit(snap.Contents.File, ContentsLimit)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrContentsMismatch, err)
	}
	if snapshot.SHA256(b) != snap.Contents.SHA256 {
		return nil, fmt.Errorf("%w: sha256 of %s", ErrContentsMismatch, snap.Contents.File)
	}
	srv, entries, err := snapshot.DecodeContents(b)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrContentsMismatch, err)
	}
	if snap.Contents.Server != nil && !snap.Contents.Server.Same(&srv) {
		return nil, fmt.Errorf("%w: header names another server life", ErrContentsMismatch)
	}
	return entries, nil
}

// ---------------------------------------------------------------------------
// logs
// ---------------------------------------------------------------------------

// AppendLog appends one line to log/<name> (created 0600, dir 0700).
func (s *Store) AppendLog(name, line string) error {
	logs, err := s.state.OpenSub("log")
	if err != nil {
		return err
	}
	defer func() { _ = logs.Close() }()
	f, err := logs.OpenAppend(name)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(line, "\n") {
		line += "\n"
	}
	_, werr := f.WriteString(line)
	return errors.Join(werr, f.Close())
}
