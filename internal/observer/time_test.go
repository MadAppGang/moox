package observer

import (
	"testing"
	"time"
)

// TestFormatTimeIsMillisecondUTC pins the one timestamp representation. Getting
// this wrong in one place would be invisible until two surfaces disagreed.
func TestFormatTimeIsMillisecondUTC(t *testing.T) {
	cases := []struct {
		in   time.Time
		want string
	}{
		{time.Date(2026, 9, 18, 2, 40, 11, 204_000_000, time.UTC), "2026-09-18T02:40:11.204Z"},
		{time.Date(2026, 9, 18, 2, 40, 11, 0, time.UTC), "2026-09-18T02:40:11.000Z"},
		// A non-UTC input must be converted, not merely re-labelled.
		{time.Date(2026, 9, 18, 12, 40, 11, 204_000_000, time.FixedZone("AEST", 10*3600)),
			"2026-09-18T02:40:11.204Z"},
		// Sub-millisecond precision is truncated by the format, which is why
		// the domain carries millisecond precision as a rule.
		{time.Date(2026, 9, 18, 2, 40, 11, 204_999_999, time.UTC), "2026-09-18T02:40:11.204Z"},
	}
	for _, c := range cases {
		if got := FormatTime(c.in); got != c.want {
			t.Errorf("FormatTime(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestParseTimeRoundTripsFormatTime is the property the codec depends on: a
// parsed value must re-format to the string it came from, or every timestamp
// would drift by a fraction on each hop through a socket client.
func TestParseTimeRoundTripsFormatTime(t *testing.T) {
	originals := []time.Time{
		time.Date(2026, 9, 18, 2, 40, 11, 204_000_000, time.UTC),
		time.Date(2026, 8, 17, 8, 9, 36, 697_000_000, time.UTC),
		time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	for _, in := range originals {
		s := FormatTime(in)
		back, err := ParseTime(s)
		if err != nil {
			t.Fatalf("ParseTime(%q): %v", s, err)
		}
		if !back.Equal(in) {
			t.Errorf("%v -> %q -> %v", in, s, back)
		}
		if FormatTime(back) != s {
			t.Errorf("re-format of %q gave %q", s, FormatTime(back))
		}
	}
}

// TestParseTimeRejectsGarbage. A timestamp that will not parse must be an
// error, so the caller can leave the field nil rather than defaulting to the
// zero time — which would read as year 1 and compute an age of two millennia.
func TestParseTimeRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "not a time", "1758163211204", "2026-09-18"} {
		if _, err := ParseTime(s); err == nil {
			t.Errorf("ParseTime(%q) accepted garbage", s)
		}
	}
}

// TestParseTimeAcceptsAnyRFC3339Precision: job records and transcripts already
// write RFC3339 with millis, but a source that writes more or fewer digits must
// still be readable, truncated to our precision.
func TestParseTimeAcceptsAnyRFC3339Precision(t *testing.T) {
	cases := map[string]string{
		"2026-09-18T02:40:11Z":           "2026-09-18T02:40:11.000Z",
		"2026-09-18T02:40:11.2Z":         "2026-09-18T02:40:11.200Z",
		"2026-09-18T02:40:11.204999999Z": "2026-09-18T02:40:11.204Z",
		"2026-09-18T12:40:11.204+10:00":  "2026-09-18T02:40:11.204Z",
	}
	for in, want := range cases {
		got, err := ParseTime(in)
		if err != nil {
			t.Fatalf("ParseTime(%q): %v", in, err)
		}
		if FormatTime(got) != want {
			t.Errorf("ParseTime(%q) -> %q, want %q", in, FormatTime(got), want)
		}
	}
}

// TestAgeMsKeepsNilNil. "No timestamp" and "zero age" are different answers,
// and the whole nullable design rests on them staying different.
func TestAgeMsKeepsNilNil(t *testing.T) {
	now := time.Date(2026, 9, 18, 2, 40, 11, 204_000_000, time.UTC)
	if got := AgeMs(now, nil); got != nil {
		t.Errorf("AgeMs(now, nil) = %d, want nil", *got)
	}
	same := now
	if got := AgeMs(now, &same); got == nil || *got != 0 {
		t.Errorf("AgeMs of the same instant should be a pointer to 0, got %v", got)
	}
	earlier := now.Add(-279_119 * time.Millisecond)
	got := AgeMs(now, &earlier)
	if got == nil || *got != 279_119 {
		t.Errorf("AgeMs = %v, want 279119", got)
	}
}

// TestSystemClockTruncatesToMillisecond so that production and the fake clock
// carry the same precision and a test cannot pass only because its clock was
// tidier than the real one.
func TestSystemClockTruncatesToMillisecond(t *testing.T) {
	now := SystemClock{}.Now()
	if now.Nanosecond()%int(time.Millisecond) != 0 {
		t.Errorf("SystemClock.Now() = %v, which carries sub-millisecond precision", now)
	}
	if now.Location() != time.UTC {
		t.Errorf("SystemClock.Now() is in %v, want UTC", now.Location())
	}
}

// TestDurationMs is trivial and is here so the helper is not the one thing
// nobody checked.
func TestDurationMs(t *testing.T) {
	if got := DurationMs(1500 * time.Millisecond); got != 1500 {
		t.Errorf("DurationMs = %d, want 1500", got)
	}
	if got := DurationMs(0); got != 0 {
		t.Errorf("DurationMs(0) = %d", got)
	}
}
