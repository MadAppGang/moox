// Package relaunch decides, once and purely, what a restored pane runs
// (architecture §6.5): the B30 qualification first, then the B32 priority,
// with the eval-safety rules of the vendored cc_relaunch.sh ported exactly
// (StripSessionFlags, the dedupe of valueless flags, the eval-safe charset,
// the Claude-launcher test). Nothing here reads tmux or the filesystem; the
// one fact that needs a file (nvim's Session.vim) is a function the caller
// passes in.
package relaunch

import (
	"encoding/base64"
	"path"
	"regexp"
	"strings"

	"github.com/MadAppGang/moox/internal/engine/internal/sidmap"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

// Decision kinds.
const (
	KindClaudish  = "claudish"
	KindTyped     = "typed"     // a typed recording WITH a sid
	KindRecording = "recording" // a typed recording without a sid, replayed verbatim
	KindLauncher  = "launcher"  // a Claude launcher full_command
	KindSidOnly   = "sid-only"
	KindProgram   = "program" // an @moox-processes match
	KindNvim      = "nvim"
	KindFrozen    = "frozen-banner"
	KindLayout    = "layout"
	KindSkip      = "skip"
)

// Decision is what a row restores as. Cmd is "" for layout, skip and
// frozen-banner: an empty MOOX_RESUME is never emitted.
type Decision struct {
	Cmd    string
	Kind   string
	Claude bool // decides the LAUNCH success rule (§6.5)
	Reason string
	// Qualified: the row passed B30's qualification (step 2). The BOOT
	// verdict's `considered` counts these; a qualified row that still ends in
	// `skip` (3f: a legacy launcher whose command is not eval-safe) is
	// considered and never written, so it shows as a loss.
	Qualified bool
}

func qualified(d Decision) Decision {
	d.Qualified = true
	return d
}

// Options are the inputs Compose needs beyond the row.
type Options struct {
	ClaudishCmd string // @claude-continuity-claudish-cmd, default "claudish"
	ClaudeCmd   string // @claude-continuity-claude-cmd, default "claude"
	Processes   string // @moox-processes (resurrect syntax); "" = the default list
	// NvimSession is @moox-strategy-nvim = session (the default).
	NvimSession bool
	// SessionVim reports whether <cwd>/Session.vim exists.
	SessionVim func(cwd string) bool
	// UnenrichedImport: the row came from a resurrect import, where an
	// unenriched Claude launcher has only its full_command (B30's legacy arm).
	UnenrichedImport bool
	// Stub (V3/V4, MOOX_STUB_COMMANDS=1): every command becomes a harmless
	// stub; StubClaude is the stub binary named claude.
	Stub       bool
	StubClaude string
}

// The option names, kept from continuity (PR-H10): cc_popup.sh and cc_thaw.sh
// already read them, so there is no @moox-claudish-cmd.
const (
	OptClaudishCmd  = "@claude-continuity-claudish-cmd"
	OptClaudeCmd    = "@claude-continuity-claude-cmd"
	OptProcesses    = "@moox-processes"
	OptStrategyNvim = "@moox-strategy-nvim"
)

// FromOptions builds Options from tmux option values.
func FromOptions(get func(name string) string) Options {
	return Options{
		ClaudishCmd: get(OptClaudishCmd),
		ClaudeCmd:   get(OptClaudeCmd),
		Processes:   get(OptProcesses),
		NvimSession: get(OptStrategyNvim) != "off",
	}
}

// DefaultProcesses is resurrect's default list.
const DefaultProcesses = "vi vim view nvim emacs man less more tail top htop irssi weechat mutt"

// SafeSID is B34 (R33).
func SafeSID(s string) bool { return snapshot.SafeSID(s) }

// evalSafe is _cc_is_safe_to_eval: a whitelist of what launcher commands use.
var evalSafe = regexp.MustCompile(`^[A-Za-z0-9 _/.:@=+,%^-]*$`)

// modelSafe is R3-M4's model-id charset.
var modelSafe = regexp.MustCompile(`^[A-Za-z0-9_/.:@=+,%^-]+$`)

// IsEvalSafe reports whether a ps-derived string may be eval'd.
func IsEvalSafe(s string) bool { return evalSafe.MatchString(s) }

// excludedFlags are one-shot and free-text forms: ps lost the argv
// boundaries, so `--name "My Session"` replays as a stray prompt word.
var excludedFlags = []string{"-p", "--print", "--prompt", "--name", "--system-prompt", "--append-system-prompt", "--output-format"}

func hasFlag(cmd string, flags ...string) bool {
	padded := " " + cmd + " "
	for _, f := range flags {
		if strings.Contains(padded, " "+f+" ") {
			return true
		}
	}
	return false
}

func hasExcludedFlag(cmd string) bool { return hasFlag(cmd, excludedFlags...) }

// IsClaudeLauncher is _cc_is_claude_launcher: the exec token is claude or
// claudish, it is not an MCP server, and it carries no one-shot or free-text
// flag.
func IsClaudeLauncher(cmd string) bool {
	switch sidmap.ExecBase(cmd) {
	case "claude", "claudish":
	default:
		return false
	}
	padded := " " + cmd + " "
	if strings.Contains(padded, " --mcp ") || strings.Contains(padded, " --mcp=") || strings.Contains(padded, " mcp ") {
		return false
	}
	return !hasExcludedFlag(cmd)
}

// session-flag patterns of _cc_strip_session_flags, in its order.
var stripRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`[[:space:]]+(-r|--resume|--session-id)([[:space:]]+|=)[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`), ""},
	{regexp.MustCompile(`[[:space:]]+--resume[[:space:]]+[^[:space:]-][^[:space:]]*`), ""},
	{regexp.MustCompile(`[[:space:]]+--resume=[^[:space:]]*`), ""},
	{regexp.MustCompile(`[[:space:]]+--from-pr([[:space:]]+[^[:space:]-][^[:space:]]*)?`), ""},
	{regexp.MustCompile(`[[:space:]]+(--resume|--continue|-c|--fork-session)([[:space:]])`), "$2"},
	{regexp.MustCompile(`[[:space:]]+(--resume|--continue|-c|--fork-session)[[:space:]]*$`), ""},
}

// StripSessionFlags drops session-selection flags (and the modifiers that
// would turn an appended --resume into a fork) so one authoritative
// `--resume <sid>` can be appended (B33). Applied to a fixed point, at most 6
// passes, exactly as cc_relaunch.sh does. Only for commands that get a
// --resume appended: its bare `-c` removal would break `python -c …` (R3-H5).
func StripSessionFlags(cmd string) string {
	cur, prev := cmd, ""
	for i := 0; cur != prev && i < 6; i++ {
		prev = cur
		for _, r := range stripRules {
			cur = r.re.ReplaceAllString(cur, r.repl)
		}
	}
	return cur
}

// DedupeValuelessFlags collapses REPEATED valueless long flags to one
// (_cc_dedupe_valueless_flags; R11, R12): `--dangerously-skip-permissions`
// four times becomes once. A flag followed by a value is never touched.
func DedupeValuelessFlags(cmd string) string {
	toks := strings.Fields(cmd)
	seen := map[string]bool{}
	out := make([]string, 0, len(toks))
	for i, tok := range toks {
		next := ""
		if i+1 < len(toks) {
			next = toks[i+1]
		}
		if strings.HasPrefix(tok, "--") && !strings.Contains(tok, "=") && (next == "" || strings.HasPrefix(next, "-")) {
			if seen[tok] {
				continue
			}
			seen[tok] = true
		}
		out = append(out, tok)
	}
	return strings.Join(out, " ")
}

// FirstLine is the first line of a recording, newlines dropped (R35).
func FirstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return s
}

func or(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

// replayModel extracts the model of a claudish replay (`--model m` / `-m m`
// / `--model=m`), or "".
func replayModel(replay string) string {
	f := strings.Fields(replay)
	for i, tok := range f {
		if (tok == "--model" || tok == "-m") && i+1 < len(f) {
			return f[i+1]
		}
		if v, ok := strings.CutPrefix(tok, "--model="); ok {
			return v
		}
	}
	return ""
}

// Compose decides a row (§6.5); the first matching step wins (PR-H19).
func Compose(p snapshot.Pane, o Options) Decision {
	d := compose(p, o)
	if o.Stub && d.Cmd != "" {
		d.Cmd = Stub(d.Cmd, d.Claude, o.StubClaude)
	}
	return d
}

func compose(p snapshot.Pane, o Options) Decision {
	// 1. Role exclusions about the pane, not the command.
	switch p.Role {
	case snapshot.RoleTTObserver:
		return Decision{Kind: KindSkip, Reason: "tt-observer (dropped at plan time, B45)"}
	case snapshot.RoleMCPHelper:
		return Decision{Kind: KindLayout, Reason: "mcp-helper (B14)"}
	case snapshot.RoleFrozen:
		return Decision{Kind: KindFrozen, Reason: "tombstone (B38)"}
	}

	sid := p.SID()
	reason := ""
	if sid != "" && !SafeSID(sid) {
		reason = "UNSAFE-SID dropped"
		sid = ""
	}
	launch := FirstLine(p.Launch)
	// The pane's command is the FIRST line of full_command: an imported one
	// carries resurrect's continuation lines (other children) after it (I-2,
	// PR-H14), and none of them may qualify or shape a relaunch.
	full := FirstLine(p.FullCommand)
	rule, derived, procMatch := matchProcessRule(full, or(o.Processes, DefaultProcesses))

	// 2. Qualification (B30). A sid is authoritative and full_command alone
	// never disqualifies (B31, R4).
	legacyClaude := o.UnenrichedImport && strings.Contains(full, "claude")
	if sid == "" && launch == "" && !procMatch && !legacyClaude {
		return Decision{Kind: KindSkip, Reason: join(reason, "skipped_unknown")}
	}

	resume := ""
	if sid != "" {
		resume = " --resume " + sid
	}

	// 3a. claudish replay + sid (R9, R10). The replay passes the same test as
	// (c) (R2-M5), else falls back to the model alone (R3-M4).
	if sid != "" && p.Claude != nil && (p.Claude.Launcher == "claudish" || p.Claude.Replay != "") {
		cl := or(o.ClaudishCmd, "claudish")
		replay := strings.TrimSpace(p.Claude.Replay)
		if IsEvalSafe(replay) && !hasExcludedFlag(replay) {
			cmd := cl
			if replay != "" {
				cmd += " " + replay
			}
			return qualified(Decision{Cmd: cmd + resume, Kind: KindClaudish, Claude: true, Reason: reason})
		}
		cmd := cl
		if m := replayModel(replay); m != "" && modelSafe.MatchString(m) {
			cmd += " --model " + m
		}
		return qualified(Decision{Cmd: cmd + resume, Kind: KindClaudish, Claude: true, Reason: join(reason, "REPLAY-UNSAFE")})
	}

	// 3b. a typed recording.
	if launch != "" {
		if sid != "" {
			return qualified(Decision{Cmd: StripSessionFlags(launch) + resume, Kind: KindTyped, Claude: true, Reason: reason})
		}
		cmd, kind := nvimStrategy(launch, p.Cwd, o)
		if kind == "" {
			kind = KindRecording
		}
		return qualified(Decision{Cmd: cmd, Kind: kind, Reason: reason})
	}

	// 3c. a Claude launcher full_command (also B30's unenriched legacy launcher).
	if fc := strings.TrimSpace(full); fc != "" && IsClaudeLauncher(fc) && IsEvalSafe(fc) {
		return qualified(Decision{Cmd: DedupeValuelessFlags(StripSessionFlags(fc)) + resume, Kind: KindLauncher, Claude: true, Reason: reason})
	}

	// 3d. sid only.
	if sid != "" {
		return qualified(Decision{Cmd: or(o.ClaudeCmd, "claude") + resume, Kind: KindSidOnly, Claude: true, Reason: reason})
	}

	// 3e. an @moox-processes match (T8/T9). The ps-derived part of the
	// command (all of it for a plain rule, the arguments a `->… *` rule
	// substitutes) passes the same eval-safe test as 3a/3c: ps joined argv
	// with spaces and lost every quote, so `vim '$(x).md'` would come back as
	// live shell code in the restored pane (G1-C1). The user-written half of
	// a `->` rule is the user's own configuration and is trusted. A refused
	// row is qualified (a visible loss in BOOT) and restores as a plain
	// shell in its cwd.
	if procMatch {
		if !IsEvalSafe(derived) {
			return qualified(Decision{Kind: KindSkip, Reason: join(reason, "PROCESS-UNSAFE the saved command holds shell syntax ps cannot quote; restored as a shell")})
		}
		cmd := rule
		nv, kind := nvimStrategy(cmd, p.Cwd, o)
		if kind == "" {
			kind = KindProgram
		}
		return qualified(Decision{Cmd: nv, Kind: kind, Reason: reason})
	}

	// 3f. nothing usable (a legacy claude row whose command is not safe).
	return qualified(Decision{Kind: KindSkip, Reason: join(reason, "skipped_unknown")})
}

func join(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "; " + b
}

// nvimStrategy is B46 (@moox-strategy-nvim session) on the exec token's
// BASENAME (which also fixes resurrect's /opt/homebrew/bin/nvim miss):
// <cwd>/Session.vim present → `nvim -S`; `-S` in the original → `nvim`;
// otherwise the original. kind is "" when the strategy does not apply.
func nvimStrategy(cmd, cwd string, o Options) (string, string) {
	if !o.NvimSession {
		return cmd, ""
	}
	f := strings.Fields(cmd)
	if len(f) == 0 {
		return cmd, ""
	}
	switch path.Base(f[0]) {
	case "nvim", "vim":
	default:
		return cmd, ""
	}
	if o.SessionVim != nil && cwd != "" && o.SessionVim(cwd) {
		return f[0] + " -S", KindNvim
	}
	for _, a := range f[1:] {
		if a == "-S" {
			return f[0], KindNvim
		}
	}
	return cmd, KindNvim
}

// ---------------------------------------------------------------------------
// @moox-processes (resurrect syntax, T8/T9)
// ---------------------------------------------------------------------------

// rules splits the option into words, honouring double quotes:
// `vim "~rails server" "git log->git log *"`.
func rules(setting string) []string {
	var out []string
	var cur strings.Builder
	inQ := false
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range setting {
		switch {
		case r == '"':
			inQ = !inQ
		case (r == ' ' || r == '\t' || r == '\n') && !inQ:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// matchProcesses returns the restore command of the first matching rule:
//
//	name          full_command is `name` or starts with `name ` (also on its
//	              first token's basename, so /opt/homebrew/bin/nvim matches nvim)
//	~text         text appears anywhere in full_command
//	match->cmd    restore cmd instead; a " *" in cmd is replaced by the
//	              original's arguments after the matched program
//
// Unlike resurrect, the match text is literal, not a regex.
func matchProcesses(full, setting string) (string, bool) {
	cmd, _, ok := matchProcessRule(full, setting)
	return cmd, ok
}

// matchProcessRule is matchProcesses that also returns the part of cmd that
// came from ps (derived): the whole full_command for a rule without `->`,
// the substituted arguments for a `->… *` rule, "" for a `->` rule without
// `*` (all of its text is the user's).
func matchProcessRule(full, setting string) (cmd, derived string, ok bool) {
	full = strings.TrimSpace(full)
	if full == "" {
		return "", "", false
	}
	base := full
	if f := strings.Fields(full); len(f) > 0 {
		base = path.Base(f[0]) + strings.TrimPrefix(full, f[0])
	}
	for _, r := range rules(setting) {
		match, restore, hasRestore := strings.Cut(r, "->")
		tilde := strings.HasPrefix(match, "~")
		m := strings.TrimPrefix(match, "~")
		if m == "" {
			continue
		}
		hit := false
		if tilde {
			hit = strings.Contains(full, m)
		} else {
			for _, cand := range []string{full, base} {
				if cand == m || strings.HasPrefix(cand, m+" ") {
					hit = true
					break
				}
			}
		}
		if !hit {
			continue
		}
		if !hasRestore {
			return full, full, true
		}
		cmd := restore
		if strings.Contains(cmd, " *") {
			derived = commandArgs(full, m)
			cmd = strings.TrimSpace(strings.Replace(cmd, " *", " "+derived, 1))
		}
		return cmd, derived, true
	}
	return "", "", false
}

// commandArgs is resurrect's _get_command_arguments,
// `sed "s,^.*${match}[^ ]* *,,"`: everything after the LAST occurrence of the
// match (greedy .*), the rest of that word and the blanks after it.
func commandArgs(full, m string) string {
	i := strings.LastIndex(full, m)
	if i < 0 {
		return ""
	}
	rest := full[i+len(m):]
	if j := strings.IndexByte(rest, ' '); j >= 0 {
		return strings.TrimLeft(rest[j:], " ")
	}
	return ""
}

// ---------------------------------------------------------------------------
// stub mode (V3/V4)
// ---------------------------------------------------------------------------

// StubClaude is the default stub "binary named claude": sleep exec'd with
// argv[0] claude (zsh and bash both have `exec -a`), so the LAUNCH OK rule
// sees a claude exec token under the pane without any file on disk, and a
// real claude on PATH can never be reached from a stub restore.
const StubClaude = "-a claude sleep"

// Stub is the harmless command a stub-mode restore runs instead of cmd: it
// prints the original, base64'd and single-quoted (base64's alphabet holds no
// shell metacharacter and no quote), then sleeps — or, for a Claude kind,
// execs the stub binary named claude so the LAUNCH OK rule is exercised.
func Stub(cmd string, claude bool, stubClaude string) string {
	b := base64.StdEncoding.EncodeToString([]byte(cmd))
	tail := "exec sleep 600"
	if claude {
		tail = "exec " + or(stubClaude, StubClaude) + " 600"
	}
	return "printf 'MOOX-STUB %s\\n' '" + b + "'; " + tail
}

// StubPayload extracts the original command from a stub (tests, verdicts).
func StubPayload(stub string) (string, bool) {
	const pre = "printf 'MOOX-STUB %s\\n' '"
	rest, ok := strings.CutPrefix(stub, pre)
	if !ok {
		return "", false
	}
	i := strings.IndexByte(rest, '\'')
	if i < 0 {
		return "", false
	}
	b, err := base64.StdEncoding.DecodeString(rest[:i])
	if err != nil {
		return "", false
	}
	return string(b), true
}
