// Package tmuxtarget is the one way moox and tt talk to a tmux server.
//
// Every exec goes through a Target, and a Target always passes `-u -S <socket>`
// (moox architecture §4.5, web-findings 1.5): `-S` so nothing in the caller's
// environment ($TMUX, $TMUX_TMPDIR) can steer a command to another server, and
// `-u` so pane titles and names survive as UTF-8.
//
// Two further rules live here because they are properties of how argv is
// built, not of any one caller:
//
//   - Chain joins several tmux commands into ONE exec and escapes an argument
//     whose LAST byte is ';' (cmd-parse.y:1094-1110, E5). An inner ';' is data
//     and is passed unchanged (Q5).
//   - Lit marks a string that tmux will NOT format-expand. Titles, window and
//     session names and start directories are format-expanded by tmux and
//     `#(…)` in them runs a shell (E2); Literal doubles every '#', and the
//     builders take a Lit in exactly those positions, so passing a raw string
//     there does not compile.
//
// This package is a leaf: stdlib only (§2.3).
package tmuxtarget

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Env is the part of the process environment Resolve reads.
type Env interface{ Getenv(string) string }

// OSEnv reads the real process environment.
type OSEnv struct{}

// Getenv implements Env.
func (OSEnv) Getenv(k string) string { return os.Getenv(k) }

// MapEnv is an Env backed by a map, for tests and for explicit environments.
type MapEnv map[string]string

// Getenv implements Env.
func (m MapEnv) Getenv(k string) string { return m[k] }

// Target names one tmux server and the binary that talks to it.
type Target struct {
	Bin    string // absolute path of the tmux binary
	Socket string // absolute socket path; never empty after Resolve
}

// ErrNoSocket means a Target was used before it named a server.
var ErrNoSocket = errors.New("tmuxtarget: no socket")

// Resolve picks the server and binary for a command (§4.5):
//
//	socket = flagSocket > $<prefix>_TMUX_SOCKET > first field of $TMUX > -L "default"
//	bin    = flagBin    > $<prefix>_TMUX_BIN    > exec.LookPath("tmux")
//
// prefix is "TT" for tt and "MOOX" for moox. A socket given without a '/' is a
// NAME with -L semantics: realpath($TMUX_TMPDIR or /tmp)/tmux-<uid>/<name>,
// mirroring make_label (tmux.c:205-245). A path is kept as given — tmux keeps a
// -S path verbatim (E3) — and identity questions go through sockets.Canonical,
// never through string equality of two spellings.
func Resolve(env Env, prefix, flagSocket, flagBin string) (Target, error) {
	if env == nil {
		env = OSEnv{}
	}
	sock := flagSocket
	if sock == "" && prefix != "" {
		sock = env.Getenv(prefix + "_TMUX_SOCKET")
	}
	if sock == "" {
		if tm := env.Getenv("TMUX"); tm != "" {
			sock, _, _ = strings.Cut(tm, ",")
		}
	}
	if sock == "" {
		sock = "default"
	}
	var err error
	if !strings.Contains(sock, "/") {
		sock, err = LabelPath(env, sock)
		if err != nil {
			return Target{}, err
		}
	} else if !filepath.IsAbs(sock) {
		abs, aerr := filepath.Abs(sock)
		if aerr != nil {
			return Target{}, fmt.Errorf("tmuxtarget: socket %q: %w", sock, aerr)
		}
		sock = abs
	}

	bin := flagBin
	if bin == "" && prefix != "" {
		bin = env.Getenv(prefix + "_TMUX_BIN")
	}
	if bin == "" {
		bin, err = exec.LookPath("tmux")
		if err != nil {
			return Target{}, fmt.Errorf("tmuxtarget: tmux not found: %w", err)
		}
	}
	if !filepath.IsAbs(bin) {
		if strings.Contains(bin, "/") {
			bin, err = filepath.Abs(bin)
		} else {
			bin, err = exec.LookPath(bin)
		}
		if err != nil {
			return Target{}, fmt.Errorf("tmuxtarget: tmux binary: %w", err)
		}
	}
	return Target{Bin: bin, Socket: sock}, nil
}

// LabelPath is the socket path `tmux -L name` uses: the realpath of
// $TMUX_TMPDIR (or /tmp), then tmux-<uid>/<name>. Only the base directory is
// realpath'd, as tmux does; tmux-<uid> may not exist yet (it does not after a
// reboot, before the first server).
func LabelPath(env Env, name string) (string, error) {
	if name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("tmuxtarget: %q is not a socket name", name)
	}
	if env == nil {
		env = OSEnv{}
	}
	base := env.Getenv("TMUX_TMPDIR")
	if base == "" {
		base = "/tmp"
	}
	real, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("tmuxtarget: resolving %s: %w", base, err)
	}
	return filepath.Join(real, "tmux-"+strconv.Itoa(os.Getuid()), name), nil
}

// Args is the full argv after the binary: always -u -S <socket> first.
func (t Target) Args(args ...string) []string {
	out := make([]string, 0, len(args)+3)
	out = append(out, "-u", "-S", t.Socket)
	return append(out, args...)
}

// Command builds the exec. $TMUX is removed from its environment so nothing
// but -S decides which server it reaches.
func (t Target) Command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, t.Bin, t.Args(args...)...)
	cmd.Env = withoutTMUX(os.Environ())
	return cmd
}

// Error is a failed tmux exec, carrying what tmux printed on stderr.
type Error struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *Error) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		return fmt.Sprintf("tmux %s: %v", strings.Join(e.Args, " "), e.Err)
	}
	return fmt.Sprintf("tmux %s: %v: %s", strings.Join(e.Args, " "), e.Err, msg)
}

func (e *Error) Unwrap() error { return e.Err }

// Run executes one tmux command (or one Chain) and returns stdout.
func (t Target) Run(ctx context.Context, args ...string) ([]byte, error) {
	if t.Socket == "" {
		return nil, ErrNoSocket
	}
	cmd := t.Command(ctx, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, &Error{Args: args, Stderr: stderr.String(), Err: err}
	}
	return out, nil
}

func withoutTMUX(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMUX=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Chain joins commands with ";" so they run in ONE exec, in order, stopping at
// the first failure (E5, PV3).
//
// tmux decides whether an argument ends a command by its LAST byte only
// (cmd-parse.y:1094-1110): a trailing ';' is a separator and a trailing `\;`
// is a literal ';'. So every argument whose last byte is ';' is sent as
// s[:len-1] + `\;`, which tmux turns back into s exactly — including an
// argument that already ended in `\;`. A ';' anywhere else is data and is
// left alone (Q5). Empty commands are dropped.
func Chain(cmds ...[]string) []string {
	var out []string
	for _, c := range cmds {
		if len(c) == 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, ";")
		}
		for _, a := range c {
			out = append(out, EscapeArg(a))
		}
	}
	return out
}

// Chain is the method form of Chain, for call sites that hold a Target.
func (t Target) Chain(cmds ...[]string) []string { return Chain(cmds...) }

// EscapeArg is Chain's per-argument rule, exported for builders and tests.
func EscapeArg(a string) string {
	if strings.HasSuffix(a, ";") {
		return a[:len(a)-1] + `\;`
	}
	return a
}

// Lit is a string tmux will not format-expand. Only Literal constructs one: the
// field is unexported so neither a string literal nor a string variable can be
// converted to it, which is what makes the builders' Lit positions a compile
// error for raw input (§2.4 LiteralArgs). (Architecture §4.5 wrote
// `type Lit string`; an untyped constant is assignable to that, so it would not
// have closed the hole it exists for.)
type Lit struct{ s string }

// Literal escapes s for a format-expanded position so tmux expands it back to
// exactly s and runs nothing (E2).
//
// The rule is per RUN of '#', not per byte (measured in P1 on 3.8,
// TestE2E_LiteralRoundTrip): format_expand1 halves a run ("##" → "#"), except
// a run immediately before '[', which it copies verbatim because "#[…]" and
// "##[" are style markers left for format_draw. So "#[fg=red]" stays as it is
// and every other run is doubled: "#{x}" → "##{x}", "#(cmd)" → "##(cmd)",
// "a#" → "a##". '{', '}', '(' and the single-letter aliases only matter after
// an odd run, and Literal never leaves one (architecture §4.5 wrote
// ReplaceAll(s, "#", "##"), which turned "#[fg=red]" into "##[fg=red]").
func Literal(s string) Lit {
	if !strings.Contains(s, "#") {
		return Lit{s: s}
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i := 0; i < len(s); {
		if s[i] != '#' {
			b.WriteByte(s[i])
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '#' {
			j++
		}
		run := s[i:j]
		b.WriteString(run)
		if j >= len(s) || s[j] != '[' {
			b.WriteString(run)
		}
		i = j
	}
	return Lit{s: b.String()}
}

// StoredTitle is the Lit that sets a pane title back to s, where s is a
// title as tmux REPORTS it (#{pane_title}, which is what a snapshot holds).
//
// tmux stores a title with every `\` doubled: `select-pane -T 'a\b'` and an
// OSC 2 "a\b" both read back as `a\\b`, 4 bytes (measured on 3.8, I-4; the
// control characters that would need other escapes are refused or stripped
// before that). Setting s itself would double it once more, so each `\\` is
// halved first; a `\` that is not part of a pair is not tmux's encoding and
// is kept. The result then goes through Literal like any other title.
func StoredTitle(s string) Lit {
	if !strings.Contains(s, `\\`) {
		return Literal(s)
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		b.WriteByte(s[i])
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == '\\' {
			i++
		}
	}
	return Literal(b.String())
}

// String is the escaped form, as it goes on the command line.
func (l Lit) String() string { return l.s }

// IsZero reports whether the Lit was never set (an empty Literal("") is also
// zero: an empty name or title is the same as none for every builder).
func (l Lit) IsZero() bool { return l.s == "" }
