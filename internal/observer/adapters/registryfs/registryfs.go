package registryfs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// DirName is the registry directory relative to the Claude state root.
const DirName = "sessions"

// recordSuffix is the extension of a session record. keySuffix is the extension
// of the sibling file the registry keeps beside each record; it is counted and
// NEVER opened. It is not ours, it is not JSON, and reading it would be a read
// of key material for no reason at all.
const (
	recordSuffix = ".json"
	keySuffix    = ".key"
)

// tornRetryDelay is §6.5's one named exception to the time.Sleep ban: a parse
// failure on an in-place-rewritten file means TORN, not dead, and the cheapest
// correct response is to wait out the writer and read again.
//
// maxRetriesPerRebuild bounds it. Beyond the bound, files keep their previous
// state and are re-read on the next coalesced event, which the design already
// expects — so the worst case of a directory being rewritten wholesale is
// latency, not a blocked read.
const (
	tornRetryDelay       = 10 * time.Millisecond
	maxRetriesPerRebuild = 4
)

// Source is a read-only reader of the interactive session registry. It
// implements observer.RegistrySource.
type Source struct {
	fsys  fs.FS
	root  string
	clock observer.Clock
	// sleep is the torn-read retry delay, injectable so the retry path is
	// testable without spending real milliseconds. Production is time.Sleep.
	sleep func(time.Duration)
	// maxRetries bounds the retries in one Read.
	maxRetries int
}

// New returns a Source reading the registry directory at dir.
//
// The directory is wrapped in os.DirFS, which yields a read-only value: the
// adapter has no way to write to ~/.claude/ even by mistake, and the static
// write audit has nothing to find here because there is nothing to find.
func New(dir string, clock observer.Clock) *Source {
	return NewFS(os.DirFS(dir), dir, clock)
}

// NewFS returns a Source reading from fsys, with root used only to name files
// in advisories and in the source report. Tests pass an fstest.MapFS; the
// containment test passes an os.DirFS over a chmod 0500 tree.
func NewFS(fsys fs.FS, root string, clock observer.Clock) *Source {
	return newSource(fsys, root, clock)
}

// newSource is the one construction site, so the time.Sleep reference has
// exactly one home and the §9.3a timer allowlist can name it precisely.
func newSource(fsys fs.FS, root string, clock observer.Clock) *Source {
	if clock == nil {
		clock = observer.SystemClock{}
	}
	return &Source{
		fsys:       fsys,
		root:       root,
		clock:      clock,
		sleep:      time.Sleep,
		maxRetries: maxRetriesPerRebuild,
	}
}

// Root is the directory this source reads, for the source report.
func (s *Source) Root() string { return s.root }

// Read returns every readable registry record, sorted by pid ascending.
//
// The sort is not cosmetic: §3.2's duplicate rule is "the FIRST in a fixed
// order wins", and "first" is only deterministic if the order is. Directory
// enumeration order is not a fixed order.
//
// A missing registry directory is a FACT, not an error — it is what a machine
// where Claude has never run looks like. Records is empty, Absent is true, and
// the error is nil, so a caller reports "0 sessions" and exits 0.
func (s *Source) Read(ctx context.Context) (observer.RegistryResult, error) {
	res := observer.RegistryResult{ObservedAt: s.clock.Now()}

	entries, err := fs.ReadDir(s.fsys, ".")
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			res.Absent = true
			return res, nil
		}
		res.Err = err
		return res, err
	}

	// Enumerate in name order first so the retry budget is spent
	// deterministically rather than on whichever files the filesystem happened
	// to list first.
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		switch {
		case e.IsDir():
			// The registry is flat. A directory here is not ours.
		case strings.HasSuffix(name, keySuffix):
			res.IgnoredKeyFiles++
		case strings.HasSuffix(name, recordSuffix):
			names = append(names, name)
		default:
			// Anything else in this directory belongs to someone else.
		}
	}
	sort.Strings(names)

	retries := 0
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			res.Err = err
			return res, err
		}

		full := s.pathOf(name)
		rec, n, err := s.readOne(name, full)
		if err == nil {
			res.Records = append(res.Records, rec)
			continue
		}

		// §6.5: a parse failure on a file written IN PLACE means the writer was
		// mid-write, not that the session is gone. Wait once and read again.
		if retries >= s.maxRetries {
			res.RetriesDeferred++
			res.ParseErrors++
			res.Failures = append(res.Failures, observer.RecordFailure{
				Path: full, Bytes: n, Error: "retry budget spent: " + err.Error(),
			})
			continue
		}
		retries++
		res.ParseErrors++
		s.sleep(tornRetryDelay)

		rec, n, err = s.readOne(name, full)
		if err == nil {
			// Succeeded on the retry: use it, and the incremented parseErrors
			// counter is the whole trace. No advisory — a torn read during a
			// status transition is normal, not a defect.
			res.Records = append(res.Records, rec)
			continue
		}
		res.Failures = append(res.Failures, observer.RecordFailure{
			Path: full, Bytes: n, Error: err.Error(),
		})
	}

	sort.SliceStable(res.Records, func(i, j int) bool {
		return res.Records[i].PID < res.Records[j].PID
	})
	return res, nil
}

// readOne reads and parses one record, returning the byte count so a failure
// can report how much it got.
func (s *Source) readOne(name, full string) (observer.RegistryRecord, int, error) {
	data, err := fs.ReadFile(s.fsys, name)
	if err != nil {
		return observer.RegistryRecord{}, len(data), err
	}
	rec, err := parseRecord(full, data)
	return rec, len(data), err
}

// pathOf builds the display path for a record. It is used for advisories and
// for the core's keep-previous lookup, never to open anything — every read goes
// through the fs.FS with the bare entry name.
func (s *Source) pathOf(name string) string {
	if s.root == "" {
		return name
	}
	if strings.Contains(s.root, string(filepath.Separator)) || filepath.IsAbs(s.root) {
		return filepath.Join(s.root, name)
	}
	return path.Join(s.root, name)
}

var _ observer.RegistrySource = (*Source)(nil)
