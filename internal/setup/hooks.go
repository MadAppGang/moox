package setup

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Rc is one shell rc file the hook line goes into.
type Rc struct {
	Shell string // zsh | bash
	Path  string
	File  File
	Line  string
	// Already: the rc file, or a file it sources one level deep, already
	// loads the hook (PR-L5). AlreadyVia names that file.
	Already    bool
	AlreadyVia string
}

// hookNeedles are the names whose presence means "the hook is loaded": moox's
// two shell entry files and the vendored hook itself (claude-continuity.zsh
// or .bash), which a dotfiles rc may source directly.
var hookNeedles = []string{"moox.zsh", "moox.bash", "claude-continuity."}

// HookLine is the guarded rc line for a shell (§8.3).
func HookLine(shell, root string) string {
	switch shell {
	case "zsh":
		p := shQuote(root + "/shell/moox.zsh")
		return "[[ -r " + p + " ]] && source " + p + "  " + hookMarker
	case "bash":
		p := shQuote(root + "/shell/moox.bash")
		return "[ -r " + p + " ] && . " + p + "  " + hookMarker
	}
	return ""
}

// shQuote double-quotes a path for sh/zsh/bash.
func shQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`")
	return `"` + r.Replace(s) + `"`
}

func detectRcs(env Env, shell string) []Rc {
	switch shell {
	case "zsh":
		dir := env.Home
		if env.ZDotDir != "" && filepath.IsAbs(env.ZDotDir) {
			dir = env.ZDotDir
		}
		return []Rc{newRc(env, "zsh", filepath.Join(dir, ".zshrc"))}
	case "bash":
		rcs := []Rc{newRc(env, "bash", filepath.Join(env.Home, ".bashrc"))}
		// A login bash reads ~/.bash_profile and not ~/.bashrc; when the
		// profile exists and does not source ~/.bashrc itself, the hook goes
		// there too (§7.1).
		profile := filepath.Join(env.Home, ".bash_profile")
		if pf := statFile(profile); pf.Kind != Missing && !sourcesBashrc(pf.Content) {
			rcs = append(rcs, newRc(env, "bash", profile))
		}
		return rcs
	}
	return nil
}

func newRc(env Env, shell, path string) Rc {
	rc := Rc{Shell: shell, Path: path, File: statFile(path), Line: HookLine(shell, env.Root)}
	if rc.File.Kind == Missing {
		return rc
	}
	if mentionsHook(rc.File.Content) {
		rc.Already, rc.AlreadyVia = true, path
		return rc
	}
	for _, src := range sourcedFiles(rc.File.Content, env.Home, filepath.Dir(path), env.Getenv) {
		if b, err := os.ReadFile(src); err == nil && mentionsHook(b) {
			rc.Already, rc.AlreadyVia = true, src
			return rc
		}
	}
	return rc
}

// mentionsHook: a non-comment line names one of the hook files.
func mentionsHook(b []byte) bool {
	for _, l := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		for _, n := range hookNeedles {
			if strings.Contains(t, n) {
				return true
			}
		}
	}
	return false
}

// sourceRe finds `source X` / `. X` anywhere in a line (also after `&&`).
var sourceRe = regexp.MustCompile(`(?:^|[;&|\s])(?:source|\.)\s+("[^"]+"|'[^']+'|[^\s;&|)]+)`)

// defaultRe is `${NAME:-word}` / `${NAME-word}`, the shape a dotfiles rc uses to
// find its repo: `source "${DOTFILES:-$HOME/dotfiles}/zsh/zshrc.package"`.
var defaultRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:?)-((?:[^{}]|\$\{HOME\})*)\}`)

// sourcedFiles are the files an rc sources with a LITERAL path (after `~`,
// $HOME and ${HOME} expansion, and `${NAME:-word}` resolved from getenv —
// unset or empty takes the word; anything else with a `$` is skipped), one
// level deep, that exist.
func sourcedFiles(b []byte, home, dir string, getenv func(string) string) []string {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	var out []string
	seen := map[string]bool{}
	for _, l := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") {
			continue
		}
		for _, m := range sourceRe.FindAllStringSubmatch(t, -1) {
			p := strings.Trim(m[1], `"'`)
			p = defaultRe.ReplaceAllStringFunc(p, func(s string) string {
				g := defaultRe.FindStringSubmatch(s)
				if v, ok := lookup(getenv, g[1]); ok && (g[2] == "" || v != "") {
					return v
				}
				return g[3]
			})
			p = strings.ReplaceAll(p, "${HOME}", home)
			p = strings.ReplaceAll(p, "$HOME", home)
			if strings.HasPrefix(p, "~/") {
				p = filepath.Join(home, p[2:])
			}
			if strings.Contains(p, "$") || p == "" {
				continue
			}
			if !filepath.IsAbs(p) {
				p = filepath.Join(dir, p)
			}
			if seen[p] {
				continue
			}
			if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// lookup reports a variable's value and whether it is set. getenv returns ""
// for unset, so an empty value counts as unset: `${NAME-word}` then takes the
// word too, which only matters for a variable set to "" on purpose.
func lookup(getenv func(string) string, name string) (string, bool) {
	v := getenv(name)
	return v, v != ""
}

// sourcesBashrc: a bash_profile line sources ~/.bashrc.
func sourcesBashrc(b []byte) bool {
	for _, l := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "#") {
			continue
		}
		for _, m := range sourceRe.FindAllStringSubmatch(t, -1) {
			if strings.HasSuffix(strings.Trim(m[1], `"'`), ".bashrc") {
				return true
			}
		}
	}
	return false
}
