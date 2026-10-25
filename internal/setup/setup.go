// Package setup is `moox setup` and `moox uninstall` (architecture §8.3, F5):
// Detect what the machine has, build a Plan with zero questions (Defaults),
// optionally ask at most two (Ask), then Apply it through statedir — the one
// writer — recording every step in the manifest so Uninstall can walk it back.
//
// Nothing here follows a symlink to write: a symlinked rc file or
// settings.json is reported with the line or JSON to add by hand, and a
// symlinked ~/.tmux.conf is backed up by moving the link itself.
package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/setup/manifest"
	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// Mode is where moox runs (§8.3).
type Mode string

const (
	// Replace: ~/.tmux.conf becomes the 2-line moox entry; plain tmux is moox.
	Replace Mode = "replace"
	// Separate: the user's config stays; moox runs as its own `-L moox` server.
	Separate Mode = "separate"
)

// SeparateSocket is the socket name Separate mode runs on.
const SeparateSocket = "moox"

// ManagedMarker is the first-line marker of a file moox wrote (§3.2).
const ManagedMarker = "moox-managed"

// hookMarker ends every line setup appends to a shell rc file.
const hookMarker = "# moox"

// ErrUserAborted is the form or a confirmation aborted (exit 130).
var ErrUserAborted = errors.New("setup: aborted by the user")

// Env is everything setup reads from the outside world. Tests build one over
// a scratch HOME; the real one comes from the process (cmd/moox).
type Env struct {
	Home       string // $HOME, absolute
	ConfigHome string // XDG_CONFIG_HOME or $HOME/.config
	Shell      string // $SHELL
	ZDotDir    string // $ZDOTDIR
	Root       string // the STABLE share root (holds moox.conf)
	// Getenv resolves `${NAME:-word}` in an rc's source lines (nil: every
	// NAME is unset, so the word is used).
	Getenv func(string) string
	// LookPath finds a program on PATH (jq, tmux-fingers).
	LookPath func(string) (string, error)
	// TmuxVersion is `tmux -V`.
	TmuxVersion func() (tmuxtarget.Version, error)
	// ThemeDetect is `theme.sh detect`: "light", "dark" or "" (unknown).
	ThemeDetect func() string
	Now         func() time.Time
	// MooxVersion goes into the manifest.
	MooxVersion string
}

func (e Env) claudeRoot() string { return filepath.Join(e.Home, ".claude") }
func (e Env) mooxConfig() string { return filepath.Join(e.ConfigHome, "moox") }

// LocalConf is XC/moox/local.conf.
func (e Env) LocalConf() string { return filepath.Join(e.mooxConfig(), "local.conf") }

// ManifestPath is XC/moox/setup.json.
func (e Env) ManifestPath() string { return filepath.Join(e.mooxConfig(), manifest.FileName) }

func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// FileKind is what sits at a user path.
type FileKind int

const (
	Missing FileKind = iota
	Regular
	Symlink
	Other
)

// File is one user file setup cares about.
type File struct {
	Path string
	Kind FileKind
	// Content is the file's bytes (a symlink: its target's, if readable).
	Content []byte
	// Managed: the first line carries the moox-managed marker.
	Managed bool
}

func statFile(path string) File {
	f := File{Path: path}
	fi, err := os.Lstat(path)
	if err != nil {
		return f
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		f.Kind = Symlink
	case fi.Mode().IsRegular():
		f.Kind = Regular
	default:
		f.Kind = Other
		return f
	}
	if b, err := os.ReadFile(path); err == nil { // reading through a link is fine; writing never is
		f.Content = b
		first, _, _ := strings.Cut(string(b), "\n")
		f.Managed = strings.Contains(first, ManagedMarker)
	}
	return f
}

// Facts is what Detect found.
type Facts struct {
	Tmux    tmuxtarget.Version
	TmuxErr error
	// Configs are ~/.tmux.conf and XC/tmux/tmux.conf, in that order.
	Configs []File
	// ShellName is the basename of $SHELL ("zsh", "bash", …).
	ShellName string
	// Rcs are the rc files the hook goes into for ShellName (zsh: one; bash:
	// .bashrc, plus .bash_profile when it exists and does not source
	// .bashrc). Each is decided add / already / symlink.
	Rcs []Rc
	// Claude
	ClaudeDir bool // ~/.claude exists
	Settings  File // ~/.claude/settings.json
	Jq        bool
	Fingers   bool
	LocalConf File
	// Theme is what theme.sh detect reported ("" = unknown).
	Theme string
	// MooxConfigLink: XC/moox is a symlink (a stow tree fold, PR-H17), so
	// setup.json and backups/ would land in its target.
	MooxConfigLink bool
	Manifest       *manifest.Manifest
}

// UserConfig reports whether a non-moox tmux config exists.
func (f Facts) UserConfig() bool {
	for _, c := range f.Configs {
		if c.Kind != Missing && !c.Managed {
			return true
		}
	}
	return false
}

// Detect reads the machine. It writes nothing.
func Detect(env Env) (Facts, error) {
	if env.Home == "" || !filepath.IsAbs(env.Home) {
		return Facts{}, fmt.Errorf("setup: HOME %q is not an absolute path", env.Home)
	}
	if env.Root == "" {
		return Facts{}, errors.New("setup: no moox share root")
	}
	var f Facts
	if env.TmuxVersion != nil {
		f.Tmux, f.TmuxErr = env.TmuxVersion()
	} else {
		f.TmuxErr = errors.New("tmux version unknown")
	}
	f.Configs = []File{
		statFile(filepath.Join(env.Home, ".tmux.conf")),
		statFile(filepath.Join(env.ConfigHome, "tmux", "tmux.conf")),
	}
	f.ShellName = filepath.Base(env.Shell)
	f.Rcs = detectRcs(env, f.ShellName)
	if fi, err := os.Stat(env.claudeRoot()); err == nil && fi.IsDir() {
		f.ClaudeDir = true
	}
	f.Settings = statFile(filepath.Join(env.claudeRoot(), "settings.json"))
	if env.LookPath != nil {
		_, err := env.LookPath("jq")
		f.Jq = err == nil
		_, err = env.LookPath("tmux-fingers")
		f.Fingers = err == nil
	}
	f.LocalConf = statFile(env.LocalConf())
	if fi, err := os.Lstat(env.mooxConfig()); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		f.MooxConfigLink = true
	}
	if env.ThemeDetect != nil {
		f.Theme = env.ThemeDetect()
	}
	if b, err := os.ReadFile(env.ManifestPath()); err == nil {
		m, err := manifest.Decode(b)
		if err != nil {
			return f, err
		}
		f.Manifest = m
	}
	return f, nil
}

// Choices are the answers Ask may change.
type Choices struct {
	Mode        Mode
	ClaudeHooks bool
	Theme       string // auto | light | dark
}

// State is what an action will do.
type State string

const (
	Add     State = "add"
	Already State = "already"
	Manual  State = "manual" // print what to do by hand; nothing is written
)

// Action is one ordered step of a plan.
type Action struct {
	Kind  string // manifest kinds, plus "note"
	State State
	Path  string
	// Content: a write's bytes; Line: an append's line.
	Content []byte
	Line    string
	// Message: what a manual step asks the user to do, or a note.
	Message string
	// claude: the settings.json merge (Kind claude-merge).
	claude *claudeMerge
}

// Describe is one line for --dry-run and the summary.
func (a Action) Describe() string {
	if a.Kind == "note" {
		return "note: " + a.Message
	}
	var what string
	switch a.Kind {
	case manifest.KindBackup:
		what = "back up " + a.Path
	case manifest.KindWrite:
		what = "write " + a.Path
	case manifest.KindAppend:
		what = fmt.Sprintf("append to %s: %s", a.Path, a.Line)
	case manifest.KindClaudeMerge:
		what = "add the Claude Code hooks to " + a.Path
	default:
		what = a.Message
	}
	switch a.State {
	case Already:
		return "already: " + what
	case Manual:
		return "by hand: " + a.Message
	}
	return what
}

// Plan is what setup will do (§8.3).
type Plan struct {
	Choices
	Actions  []Action
	Warnings []string
	Notes    []string
}

// Pending reports whether any action would write.
func (p Plan) Pending() bool {
	for _, a := range p.Actions {
		if a.State == Add && a.Kind != "note" {
			return true
		}
	}
	return false
}

// Defaults is the zero-question plan: Separate if a non-moox config exists,
// else Replace; no Claude hooks; theme auto.
func Defaults(env Env, f Facts) Plan {
	c := Choices{Mode: Replace, Theme: "auto"}
	if f.UserConfig() {
		c.Mode = Separate
	}
	// A machine set up before keeps its recorded mode.
	if f.Manifest != nil && (f.Manifest.Mode == string(Replace) || f.Manifest.Mode == string(Separate)) {
		c.Mode = Mode(f.Manifest.Mode)
	}
	return Build(env, f, c)
}

// Build turns choices into the ordered actions.
func Build(env Env, f Facts, c Choices) Plan {
	if c.Theme == "" {
		c.Theme = "auto"
	}
	p := Plan{Choices: c}
	if f.TmuxErr != nil {
		p.Warnings = append(p.Warnings, "tmux: "+f.TmuxErr.Error())
	} else if !f.Tmux.AtLeast(3, 6) {
		p.Warnings = append(p.Warnings, fmt.Sprintf("tmux %s is older than 3.6; moox needs 3.6 or later", f.Tmux))
	}

	if f.MooxConfigLink {
		p.Warnings = append(p.Warnings, env.mooxConfig()+" is a symlink: setup.json and backups/ are written where it "+
			"points (stow it with --no-folding to keep it a real directory)")
	}

	// 1. tmux config.
	switch c.Mode {
	case Replace:
		entry := EntryFile(env.Root)
		for i, cf := range f.Configs {
			switch {
			case cf.Kind == Missing:
			case cf.Managed:
				if i == 0 {
					p.Actions = append(p.Actions, Action{Kind: manifest.KindWrite, State: Already, Path: cf.Path})
				}
			case cf.Kind == Other:
				p.Warnings = append(p.Warnings, cf.Path+" is not a regular file or symlink; left alone")
			default:
				p.Actions = append(p.Actions, Action{Kind: manifest.KindBackup, State: Add, Path: cf.Path})
			}
		}
		if top := f.Configs[0]; top.Kind == Missing || !top.Managed {
			if top.Kind != Other {
				p.Actions = append(p.Actions, Action{Kind: manifest.KindWrite, State: Add, Path: top.Path, Content: entry})
			}
		}
	case Separate:
		p.Notes = append(p.Notes, "your tmux config stays; run moox as its own server with `moox` (tmux -L "+SeparateSocket+")")
	}

	// 2. local.conf.
	local := localConfContent(env, c)
	switch {
	case f.LocalConf.Kind == Missing:
		p.Actions = append(p.Actions, Action{Kind: manifest.KindWrite, State: Add, Path: env.LocalConf(), Content: local})
	case c.Mode == Separate && !hasSocketsLine(f.LocalConf.Content, SeparateSocket):
		if f.LocalConf.Kind == Regular {
			p.Actions = append(p.Actions, Action{Kind: manifest.KindAppend, State: Add, Path: env.LocalConf(),
				Line: separateSocketsLine})
		} else {
			p.Actions = append(p.Actions, Action{Kind: manifest.KindAppend, State: Manual, Path: env.LocalConf(),
				Message: fmt.Sprintf("%s is a symlink; add this line to it: %s", env.LocalConf(), separateSocketsLine)})
		}
	default:
		p.Actions = append(p.Actions, Action{Kind: manifest.KindWrite, State: Already, Path: env.LocalConf()})
	}

	// 3. Shell hook.
	if len(f.Rcs) == 0 {
		p.Notes = append(p.Notes, fmt.Sprintf("your shell is %s: continuity resume needs zsh or bash; layouts and cwd still restore",
			nonEmpty(f.ShellName, "unknown")))
	}
	for _, rc := range f.Rcs {
		switch {
		case rc.Already:
			p.Actions = append(p.Actions, Action{Kind: manifest.KindAppend, State: Already, Path: rc.Path, Line: rc.Line})
		case rc.File.Kind == Symlink:
			p.Actions = append(p.Actions, Action{Kind: manifest.KindAppend, State: Manual, Path: rc.Path, Line: rc.Line,
				Message: fmt.Sprintf("%s is a symlink, so moox does not write it; add this line to the file it points at:\n    %s",
					rc.Path, rc.Line)})
		case rc.File.Kind == Other:
			p.Warnings = append(p.Warnings, rc.Path+" is not a regular file; shell hook not added")
		case rc.File.Kind == Missing:
			p.Actions = append(p.Actions, Action{Kind: manifest.KindWrite, State: Add, Path: rc.Path,
				Content: []byte(rc.Line + "\n")})
		default:
			p.Actions = append(p.Actions, Action{Kind: manifest.KindAppend, State: Add, Path: rc.Path, Line: rc.Line})
		}
	}

	// 4. Claude hooks.
	if c.ClaudeHooks {
		p.Actions = append(p.Actions, claudeAction(env, f, &p))
	}

	// 5. Theme: a non-auto choice is a pin, the one input theme.sh treats as
	// final (theme.sh `detect` step 1).
	if f.Theme != "" {
		p.Notes = append(p.Notes, "theme: detected "+f.Theme+" (auto follows each screen; `prefix T` switches)")
	}
	if c.Theme == "light" || c.Theme == "dark" {
		pin := filepath.Join(env.Home, ".config", "tmux", "theme.pinned")
		pf := statFile(pin)
		switch {
		case pf.Kind == Regular && strings.TrimSpace(string(pf.Content)) == c.Theme:
			p.Actions = append(p.Actions, Action{Kind: manifest.KindWrite, State: Already, Path: pin})
		case pf.Kind == Missing:
			p.Actions = append(p.Actions, Action{Kind: manifest.KindWrite, State: Add, Path: pin, Content: []byte(c.Theme + "\n")})
		default:
			p.Warnings = append(p.Warnings, pin+" already pins a theme; left alone")
		}
	}
	return p
}

func nonEmpty(s, def string) string {
	if s == "" || s == "." {
		return def
	}
	return s
}

const separateSocketsLine = `set -g @moox-sockets "` + SeparateSocket + `"  ` + hookMarker

// hasSocketsLine reports whether local.conf's @moox-sockets setting (the
// last `set[-option] [-flags] @moox-sockets VALUE` line wins, `-a` appends,
// `-u` unsets) lets the server named name take part, by the same matching
// rule the gate uses. Matching the option NAME's text was always true for
// "moox", so `set -g @moox-sockets "default"` passed as separate mode
// (G2-H6).
func hasSocketsLine(b []byte, name string) bool {
	value, seen := "", false
	for _, l := range strings.Split(string(b), "\n") {
		v, op, ok := socketsSetting(l)
		if !ok {
			continue
		}
		switch op {
		case 'u':
			value, seen = "", false
		case 'a':
			value, seen = value+v, true
		default:
			value, seen = v, true
		}
	}
	return seen && sockets.Match(name, value)
}

// socketsSetting parses one tmux config line that sets @moox-sockets: its
// value (unquoted, trailing comment dropped) and 'a'/'u' for -a/-u.
func socketsSetting(line string) (value string, op byte, ok bool) {
	f := strings.Fields(line)
	if len(f) < 2 || (f[0] != "set" && f[0] != "set-option") {
		return "", 0, false
	}
	i := 1
	for ; i < len(f) && strings.HasPrefix(f[i], "-"); i++ {
		switch {
		case strings.Contains(f[i], "u"):
			op = 'u'
		case strings.Contains(f[i], "a") && op == 0:
			op = 'a'
		}
	}
	if i >= len(f) || f[i] != "@moox-sockets" {
		return "", 0, false
	}
	rest := strings.TrimSpace(line[strings.Index(line, "@moox-sockets")+len("@moox-sockets"):])
	switch {
	case strings.HasPrefix(rest, `"`) || strings.HasPrefix(rest, "'"):
		q := rest[:1]
		if j := strings.Index(rest[1:], q); j >= 0 {
			return rest[1 : 1+j], op, true // quoted: verbatim, for -a
		}
		rest = rest[1:]
	default:
		if j := strings.Index(rest, "#"); j >= 0 {
			rest = rest[:j]
		}
	}
	return strings.TrimSpace(rest), op, true
}

// EntryFile is the 2-line ~/.tmux.conf (§3.2, templates/tmux.conf.entry),
// naming the stable share root.
func EntryFile(root string) []byte {
	tmpl, err := os.ReadFile(filepath.Join(root, "templates", "tmux.conf.entry"))
	if err != nil || !strings.Contains(string(tmpl), "@MOOX_SHARE@") {
		tmpl = []byte("# " + ManagedMarker + " — edit ~/.config/moox/local.conf instead (moox setup wrote this file)\n" +
			"source-file \"@MOOX_SHARE@/moox.conf\"\n")
	}
	return []byte(strings.ReplaceAll(string(tmpl), "@MOOX_SHARE@", confQuoteInner(root)))
}

// confQuoteInner escapes a path for a double-quoted tmux config string.
func confQuoteInner(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`)
	return r.Replace(s)
}

// localConfContent is templates/local.conf, plus the Separate-mode sockets line.
func localConfContent(env Env, c Choices) []byte {
	tmpl, err := os.ReadFile(filepath.Join(env.Root, "templates", "local.conf"))
	if err != nil {
		tmpl = []byte("# ~/.config/moox/local.conf — your own tmux settings, loaded after moox's.\n")
	}
	s := string(tmpl)
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if c.Mode == Separate {
		s += "\n# moox setup: your ~/.tmux.conf stays; continuity runs on the `moox` server (tmux -L moox).\n" +
			separateSocketsLine + "\n"
	}
	return []byte(s)
}
