package setup

import "testing"

// TestHasSocketsLine_MatchesTheValue (G2-H6): separate mode is set up only
// when local.conf's @moox-sockets VALUE lets the "moox" server take part.
// The old test matched the option name's text, which always contains
// "moox", so `"default"` passed and setup reported success for a server the
// gate then excluded.
func TestHasSocketsLine_MatchesTheValue(t *testing.T) {
	cases := []struct {
		conf string
		want bool
	}{
		{`set -g @moox-sockets "moox"  # moox`, true},
		{`set -g @moox-sockets "default"`, false},
		{`set -g @moox-sockets "default moox"`, true},
		{`set -g @moox-sockets '*'`, true},
		{`set-option -g @moox-sockets mo*`, true},
		{`set -g @moox-sockets "work"  # moox`, false},
		{`# set -g @moox-sockets "moox"`, false},
		{`set -g @moox-sockets-extra "moox"`, false},
		{"set -g @moox-sockets \"moox\"\nset -g @moox-sockets \"default\"", false},
		{"set -g @moox-sockets \"default\"\nset -ga @moox-sockets \" moox\"", true},
		{"set -g @moox-sockets \"moox\"\nset -gu @moox-sockets", false},
		{``, false},
	}
	for _, c := range cases {
		if got := hasSocketsLine([]byte(c.conf), SeparateSocket); got != c.want {
			t.Errorf("%q: %v, want %v", c.conf, got, c.want)
		}
	}
}
