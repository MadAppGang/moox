package observer

import "time"

// TimeFormat is the one timestamp representation this subsystem uses: RFC3339
// with milliseconds, UTC. The registry gives epoch millis; job records and
// transcripts give RFC3339 with millis already; the adapters convert once, on
// the way in, so nothing downstream has to know which source a timestamp came
// from.
//
// Millisecond precision is a domain rule, not just a wire rule: a timestamp
// carried with more precision than this would not survive serialisation, and
// the round-trip contract would be a lie. Adapters truncate on ingest.
const TimeFormat = "2006-01-02T15:04:05.000Z07:00"

// FormatTime renders t as RFC3339 with milliseconds in UTC.
func FormatTime(t time.Time) string {
	return t.UTC().Format(TimeFormat)
}

// ParseTime parses a timestamp written by FormatTime, or any RFC3339 value,
// and truncates it to millisecond precision in UTC so that a parsed value
// re-formats to the string it came from.
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC().Truncate(time.Millisecond), nil
}

// TruncateTime normalises t to the precision the domain carries: millisecond,
// UTC. Adapters call this on every timestamp they ingest.
func TruncateTime(t time.Time) time.Time {
	return t.UTC().Truncate(time.Millisecond)
}

// Clock is the source of "now". It is injected everywhere rather than read from
// time.Now(), which is what makes the age-derivation and staleness tests
// deterministic instead of flaky.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production Clock.
type SystemClock struct{}

// Now returns the current time at the domain's millisecond precision.
func (SystemClock) Now() time.Time { return TruncateTime(time.Now()) }

// DurationMs returns d in whole milliseconds.
func DurationMs(d time.Duration) int64 { return d.Milliseconds() }

// AgeMs returns now - t in whole milliseconds, as a pointer so that "no
// timestamp" stays distinguishable from "zero age". A nil t gives a nil age.
func AgeMs(now time.Time, t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	ms := now.Sub(*t).Milliseconds()
	return &ms
}
