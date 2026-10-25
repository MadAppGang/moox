package panelfmt

import (
	"strings"
	"testing"
)

// A file's window rules come first, in file order, then the base's; the
// first glob that matches the model id wins; no match (or no model) is "not
// known".
func TestWindowRulesMatchFirstFileBeforeBase(t *testing.T) {
	base := Defaults([]byte(defaultConf+"\nwindow claude-opus-5* = 1M\nwindow claude-* = 200k\n"), testReg, Options{})
	if len(base.Windows) != 2 {
		t.Fatalf("built-in rules = %+v, want 2", base.Windows)
	}
	s := ParseConfig([]byte(strings.Join([]string{
		"window claude-opus-5-5 = 500k   # a comment",
		"window\tclaude-opus-5-5=  300k",
		"window my-local-* = 32k",
	}, "\n")), testReg, Options{}, base)
	if len(s.Problems) != 0 {
		t.Fatalf("problems: %v", s.Problems)
	}
	for model, want := range map[string]int64{
		"claude-opus-5-5":          500_000, // the file's first rule, not its second
		"claude-opus-5-20260901":   1_000_000,
		"claude-sonnet-4-20250514": 200_000,
		"my-local-llama":           32_000,
	} {
		if got, ok := s.Window(model); !ok || got != want {
			t.Errorf("Window(%q) = %d, %v; want %d", model, got, ok, want)
		}
	}
	for _, model := range []string{"", "gpt-9", "Claude-opus-5"} {
		if got, ok := s.Window(model); ok {
			t.Errorf("Window(%q) = %d; want no window", model, got)
		}
	}
	// The base Set is not changed by the file laid over it.
	if len(base.Windows) != 2 {
		t.Errorf("the base's rules changed: %+v", base.Windows)
	}
	var nilSet *Set
	if _, ok := nilSet.Window("claude-opus-5"); ok {
		t.Error("a nil Set knows a window")
	}
}

// A malformed rule is fatal, positioned, and leaves the base's rules in place.
func TestAMalformedWindowRuleIsFatal(t *testing.T) {
	base := Defaults([]byte(defaultConf+"\nwindow claude-* = 200k\n"), testReg, Options{})
	cases := []struct {
		line string
		col  int
		msg  string
	}{
		{"window = 1M", 8, "expected a model pattern after window, like window claude-opus-5* = 1M"},
		{"window claude-[ = 1M", 8, `bad model pattern "claude-[" (syntax error in pattern)`},
		{"window claude-* 1M", 17, `expected "=" after window claude-*`},
		{"window claude-* = lots", 19, `bad size "lots" (write it like 200k, 1M or 1000000)`},
		{"window claude-* = 999", 19, "a window must be between 1k and 100M, not 999"},
		{"window claude-* = 1M extra", 22, "unexpected text after the value"},
	}
	for _, c := range cases {
		s := ParseConfig([]byte(c.line), testReg, Options{}, base)
		if len(s.Problems) != 1 {
			t.Errorf("%q: problems %v, want one", c.line, s.Problems)
			continue
		}
		p := s.Problems[0]
		if !p.Fatal || p.Line != 1 || p.Col != c.col || p.Msg != c.msg {
			t.Errorf("%q: got %+v, want fatal 1:%d %q", c.line, p, c.col, c.msg)
		}
		if len(s.Windows) != 1 || s.Windows[0].Glob != "claude-*" {
			t.Errorf("%q: windows %+v, want the base's", c.line, s.Windows)
		}
	}
}
