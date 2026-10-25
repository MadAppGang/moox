package panelfmt

import (
	"strings"
	"testing"
)

// TestIconKeysSetTheIconsTheFileNames: a file may set any of the eight icon
// keys; the Set carries exactly those, keyed by status, and a file that names
// none leaves Icons nil.
func TestIconKeysSetTheIconsTheFileNames(t *testing.T) {
	src := strings.Join([]string{
		`icon_busy    = "🔄"`,
		`icon_work    = '*'`,
		`icon_waiting = "⏳"  # a comment`,
		`icon_blocked = "⛔"`,
		`icon_idle    = "💤"`,
		`icon_shell   = "🐚"`,
		`icon_unclear = "?"`,
		`icon_ended   = "·"`,
	}, "\n")
	s := ParseConfig([]byte(src), testReg, Options{}, defaults(t))
	if len(s.Problems) != 0 {
		t.Fatalf("problems: %v", s.Problems)
	}
	want := map[string]string{
		"busy": "🔄", "work": "*", "waiting": "⏳", "blocked": "⛔",
		"idle": "💤", "shell": "🐚", "unclear": "?", "ended": "·",
	}
	if len(s.Icons) != len(want) {
		t.Fatalf("icons = %v, want %v", s.Icons, want)
	}
	for k, v := range want {
		if s.Icons[k] != v {
			t.Errorf("icon %s = %q, want %q", k, s.Icons[k], v)
		}
	}
	if got := IconNames(); len(got) != len(want) {
		t.Errorf("IconNames() = %v, want the %d keys above", got, len(want))
	}
	if d := defaults(t); d.Icons != nil {
		t.Errorf("the built-in conf sets no icon, yet Icons = %v", d.Icons)
	}
	if s := ParseConfig([]byte(`meta = "{a}"`), testReg, Options{}, defaults(t)); s.Icons != nil {
		t.Errorf("a file without icon keys gave Icons = %v", s.Icons)
	}
}

// TestAnUnusableIconIsSoftPositionedAndIgnored: each case is one soft problem
// at the value's line and column, the rest of the file applies, and the icon
// is absent so the panel keeps its built-in glyph.
func TestAnUnusableIconIsSoftPositionedAndIgnored(t *testing.T) {
	cases := []struct {
		name, line string
		col        int
		msg        string
	}{
		{"too wide", `icon_idle = "zz!"`, 13, "icon_idle must be 1 or 2 columns wide, not 3; the built-in icon is used"},
		{"empty", `icon_idle = ""`, 13, "icon_idle must be 1 or 2 columns wide, not 0; the built-in icon is used"},
		{"a tab", `icon_idle = "\t"`, 13, "icon_idle must be 1 or 2 columns wide, not 0; the built-in icon is used"},
		{"two emoji", `icon_idle = "💤💤"`, 13, "icon_idle must be 1 or 2 columns wide, not 4; the built-in icon is used"},
		{"variation selector", "icon_ended = \"✔️\"", 14, "icon_ended contains U+FE0F (a variation selector): terminals disagree on its width, so drop it; the built-in icon is used"},
		{"zwj sequence", "icon_busy = \"👩‍💻\"", 13, "icon_busy contains U+200D (a zero-width joiner): terminals disagree on the width of a joined emoji, so use a single one; the built-in icon is used"},
		{"unquoted", `icon_idle = z`, 13, `icon_idle takes a quoted glyph, like icon_idle = "*": expected a quoted string; the built-in icon is used`},
		{"unterminated", `icon_idle = "z`, 13, `icon_idle takes a quoted glyph, like icon_idle = "*": unterminated string; the built-in icon is used`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "meta = \"{b}\"\n" + c.line
			s := ParseConfig([]byte(src), testReg, Options{}, defaults(t))
			want := Problem{Line: 2, Col: c.col, Msg: c.msg}
			if len(s.Problems) != 1 || s.Problems[0] != want {
				t.Fatalf("problems = %+v, want [%+v]", s.Problems, want)
			}
			if len(s.Icons) != 0 {
				t.Errorf("an unusable icon was kept: %v", s.Icons)
			}
			if got, _ := s.Meta.Line(80, texts(map[string]string{"b": "B"})); got != "B" {
				t.Errorf("the rest of the file did not apply: meta = %q", got)
			}
		})
	}
}

// TestADuplicateIconKeyIsSoftAndTheLaterLineWins, even when the later line is
// the unusable one: the built-in glyph then comes back, never the earlier one.
func TestADuplicateIconKeyIsSoftAndTheLaterLineWins(t *testing.T) {
	s := ParseConfig([]byte("icon_idle = \"a\"\nicon_idle = \"b\""), testReg, Options{}, defaults(t))
	if len(s.Problems) != 1 || s.Problems[0] != (Problem{Line: 2, Col: 1, Msg: "duplicate key icon_idle; the later one wins"}) {
		t.Fatalf("problems = %+v", s.Problems)
	}
	if s.Icons["idle"] != "b" {
		t.Errorf("idle = %q, want the later b", s.Icons["idle"])
	}
	s = ParseConfig([]byte("icon_idle = \"a\"\nicon_idle = \"abc\""), testReg, Options{}, defaults(t))
	if len(s.Problems) != 2 {
		t.Fatalf("problems = %+v, want the duplicate and the width", s.Problems)
	}
	if _, ok := s.Icons["idle"]; ok {
		t.Errorf("the earlier icon survived a rejected later one: %v", s.Icons)
	}
}

// TestAnUnknownIconKeyIsAnUnknownKey: only the eight statuses exist.
func TestAnUnknownIconKeyIsAnUnknownKey(t *testing.T) {
	s := ParseConfig([]byte(`icon_working = "*"`), testReg, Options{}, defaults(t))
	if len(s.Problems) != 1 || !s.Problems[0].Fatal || s.Problems[0].Msg != `unknown key "icon_working"` {
		t.Fatalf("problems = %+v", s.Problems)
	}
}

// TestIconsLayOverTheDefaultsSet: a Set's own icons carry into a file laid
// over it, the file's win, and the two Sets never share one map.
func TestIconsLayOverTheDefaultsSet(t *testing.T) {
	base := ParseConfig([]byte("icon_idle = \"i\"\nicon_busy = \"b\""), testReg, Options{}, defaults(t))
	s := ParseConfig([]byte(`icon_busy = "B"`), testReg, Options{}, base)
	if s.Icons["idle"] != "i" || s.Icons["busy"] != "B" {
		t.Errorf("icons = %v, want idle from the base and busy from the file", s.Icons)
	}
	if base.Icons["busy"] != "b" {
		t.Errorf("laying a file over a Set changed that Set: %v", base.Icons)
	}
	fatal := ParseConfig([]byte("icon_busy = \"X\"\nrows = 1"), testReg, Options{}, base)
	if fatal.Icons["busy"] != "b" {
		t.Errorf("a fatal file applied its icon: %v", fatal.Icons)
	}
}
