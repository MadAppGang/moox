package transcriptfs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/MadAppGang/moox/internal/observer"
)

// timestampKey is the only key the timing pass reads out of a transcript
// record. Nothing else crosses this boundary — no message, no content, no
// prompt text.
const timestampKey = "timestamp"

// ReadTiming reads the bounded timing tail and scans BACKWARDS for the newest
// record carrying a timestamp.
//
// Why backwards-from-the-end rather than "the last line", which is what every
// naive implementation does and what revision 1 specified: eight record types
// carry no `timestamp` key at all (`ai-title`, `agent-name`, `mode`,
// `permission-mode`, `atis-latch`, `file-history-snapshot`, `last-prompt`,
// `cost-state`), and on 4 of the 5 real job transcripts the FINAL LINE IS ONE
// OF THEM. Reading the last line returns nothing on 80% of the measured
// population. The newest timestamped record sits 1 to 3 records back, 747 to
// 1 880 bytes from EOF.
//
// Three results, all distinguishable, because the difference decides whether
// the core publishes a bound or an advisory:
//
//	LastActivityAt set                      -> an exact, gap-free measurement.
//	nil, ReachedStart false                 -> the tail held no timestamped
//	                                           record and did not reach offset
//	                                           0: activity-since-truncated.
//	nil, ReachedStart true                  -> the whole file was read and it
//	                                           genuinely has no timestamps.
//
// A tailBytes of 0 or less means "the production size", so a caller that has no
// opinion does not have to hold the constant.
func (s *Source) ReadTiming(ctx context.Context, loc observer.TranscriptLocation, tailBytes int) (observer.TimingFacts, error) {
	if err := ctx.Err(); err != nil {
		return observer.TimingFacts{}, err
	}
	if !loc.Found {
		// Nothing to read, and this is not a failure: the caller already knows
		// the target is absent and has its own advisory for it.
		return observer.TimingFacts{}, nil
	}
	if tailBytes <= 0 {
		tailBytes = TimingTailBytes
	}

	rel, ok := s.relOf(loc.Path)
	if !ok {
		return observer.TimingFacts{}, ErrOutsideProjectsRoot
	}

	tail, reachedStart, err := s.readTail(rel, tailBytes)
	if err != nil {
		return observer.TimingFacts{}, err
	}

	facts := observer.TimingFacts{TailBytes: len(tail), ReachedStart: reachedStart}
	facts.LastActivityAt = newestTimestamp(tail, reachedStart)
	return facts, nil
}

// readTail returns the last n bytes of a file, and whether that reached offset
// 0 (i.e. the whole file was read).
//
// It seeks when the file can seek and falls back to reading the whole file
// otherwise. The fallback is not a shortcut around the bound — it exists
// because fs.FS makes no promise about Seek, and an fstest.MapFS file in a test
// is exactly such a file. In production os.DirFS yields an *os.File, which
// seeks, so the 64 KB bound is real where it matters.
func (s *Source) readTail(rel string, n int) ([]byte, bool, error) {
	f, err := s.fsys.Open(rel)
	if err != nil {
		return nil, false, fmt.Errorf("transcriptfs: opening %s: %w", rel, err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("transcriptfs: stat %s: %w", rel, err)
	}
	size := info.Size()
	if size <= int64(n) {
		data, err := io.ReadAll(f)
		if err != nil {
			return nil, false, fmt.Errorf("transcriptfs: reading %s: %w", rel, err)
		}
		return data, true, nil
	}

	offset := size - int64(n)
	if seeker, ok := f.(io.Seeker); ok {
		if _, err := seeker.Seek(offset, io.SeekStart); err != nil {
			return nil, false, fmt.Errorf("transcriptfs: seeking %s: %w", rel, err)
		}
		data, err := io.ReadAll(io.LimitReader(f, int64(n)))
		if err != nil {
			return nil, false, fmt.Errorf("transcriptfs: reading %s: %w", rel, err)
		}
		return data, false, nil
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, false, fmt.Errorf("transcriptfs: reading %s: %w", rel, err)
	}
	if int64(len(data)) <= int64(n) {
		return data, true, nil
	}
	return data[int64(len(data))-int64(n):], false, nil
}

// newestTimestamp scans the tail's lines from the END backwards and returns the
// first parseable timestamp it finds.
//
// The FIRST line is discarded unless the tail reached offset 0, because a tail
// taken mid-file almost always begins in the middle of a record. Keeping it
// would feed half a JSON object to the decoder — harmless here, since a decode
// failure is skipped, but the discipline is what stops a partial line's
// accidentally-valid fragment from ever being read as a record.
func newestTimestamp(tail []byte, reachedStart bool) *time.Time {
	lines := bytes.Split(tail, []byte{'\n'})
	if !reachedStart && len(lines) > 0 {
		lines = lines[1:]
	}

	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rec map[string]json.RawMessage
		if err := json.Unmarshal(line, &rec); err != nil {
			// A record we cannot decode is skipped, not fatal. One malformed
			// line must never blank a field.
			continue
		}
		raw, ok := rec[timestampKey]
		if !ok {
			// One of the eight untimestamped record types. Keep walking back.
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err != nil || s == "" {
			continue
		}
		t, err := observer.ParseTime(s)
		if err != nil {
			continue
		}
		return &t
	}
	return nil
}
