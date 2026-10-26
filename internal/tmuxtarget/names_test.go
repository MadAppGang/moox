package tmuxtarget

import "testing"

func mustVersion(t *testing.T, s string) Version {
	t.Helper()
	v, err := ParseVersion(s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestNames_StoredFormAndRoundTrip: the model of how tmux stores names
// (session_check_name/window_set_name on 3.6, clean_name from 3.7), with the
// vectors PV14 measured on a real 3.6a in CI run 40244726496 ("a.b" stored
// "a_b", `a\b` stored `a\\b`) and on 3.8 ("a.b" kept, `a\b` stored `a\\b`);
// and NameArg making a name tmux printed store unchanged again — no
// backslash doubling per restore, on any version.
func TestNames_StoredFormAndRoundTrip(t *testing.T) {
	v36, v37, v38 := mustVersion(t, "tmux 3.6a"), mustVersion(t, "3.7"), mustVersion(t, "tmux 3.8")
	if !v36.MapsSessionSeparators() || v37.MapsSessionSeparators() || v38.MapsSessionSeparators() ||
		v36.EncodesTitles() || !v37.EncodesTitles() || !v38.EncodesTitles() {
		t.Fatal("version predicates are wrong for 3.6a/3.7/3.8")
	}
	for arg, want := range map[string]string{
		"a.b": "a_b", "a:b": "a_b", `a\b`: `a\\b`, "api.v2": "api_v2", "main": "main",
		"tab\there": `tab\there`, "nl\nx": `nl\nx`, "sp ace": "sp ace", "ünï ✓": "ünï ✓",
		"bel\a": `bel\a`, "del\x7f": `del\177`, "bad\xffutf": `bad\377utf`, "esc\x1b[": `esc\033[`,
	} {
		if got := StoredSessionName(v36, arg); got != want {
			t.Errorf("3.6 StoredSessionName(%q) = %q, want %q", arg, got, want)
		}
	}
	for arg, want := range map[string]string{"a.b": "a.b", "a:b": "a:b", `a\b`: `a\\b`, "tab\tx": `tab\tx`} {
		if got := StoredSessionName(v38, arg); got != want {
			t.Errorf("3.8 StoredSessionName(%q) = %q, want %q", arg, got, want)
		}
	}
	for _, x := range []string{`a\b`, `a\\b`, "tab\there", "nl\nx", `\101`, `x\q`, "del\x7f", "bad\xff", "ü\\", `trailing\`, `\000x`} {
		stored := VisName(x) // what a 3.6 server holds, and a save reads back
		if again := VisName(NameArg(v36, stored)); again != stored {
			t.Errorf("3.6 round trip of %q: stored %q, created again %q", x, stored, again)
		}
	}
	// 3.7+ refuses control characters and invalid UTF-8 in a name: only the
	// backslash pairs are decoded there, so the name is never refused.
	for stored, want := range map[string]string{`a\\b`: `a\b`, `x\\y\\`: `x\y\`, `tab\there`: `tab\there`,
		`a\\b\001`: `a\b\001`, `bad\377`: `bad\377`, `\101`: "A", `ü\\`: `ü\`} {
		if got := NameArg(v38, stored); got != want {
			t.Errorf("3.8 NameArg(%q) = %q, want %q", stored, got, want)
		}
	}
	if got := UnvisName(`\000x`); got != `\000x` {
		t.Errorf(`UnvisName decoded a NUL: %q`, got)
	}
	// Titles: verbatim on 3.6 (screen_set_title), halved pairs from 3.7.
	if got := TitleArg(v36, `a\\b #x`).String(); got != `a\\b ##x` {
		t.Errorf("3.6 TitleArg = %q", got)
	}
	if got := TitleArg(v38, `a\\b #x`).String(); got != `a\b ##x` {
		t.Errorf("3.8 TitleArg = %q", got)
	}
}
