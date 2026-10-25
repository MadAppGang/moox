package setup

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/MadAppGang/moox/internal/setup/manifest"
)

// The Claude Code hooks moox registers (§8.3): the vendored continuity
// plugin's SessionStart and Stop scripts. Any existing hook naming the same
// script under another tmux-claude-continuity checkout is REPLACED by the
// vendored one, so the hook never runs twice.
var claudeHooks = []struct{ event, script string }{
	{"SessionStart", "on_session_start.sh"},
	{"Stop", "on_stop.sh"},
}

const continuityScripts = "tmux-claude-continuity/scripts/"

// HookCommand is the settings.json command for one script.
func HookCommand(root, script string) string {
	p := filepath.Join(root, "plugins", "tmux-claude-continuity", "scripts", script)
	if !strings.Contains(p, "'") {
		return "bash '" + p + "'"
	}
	return "bash " + shQuote(p)
}

// claudeMerge is a planned settings.json merge.
type claudeMerge struct {
	dir    string // ~/.claude
	name   string // settings.json
	before []byte // nil: the file does not exist
	after  []byte
}

func claudeAction(env Env, f Facts, p *Plan) Action {
	path := filepath.Join(env.claudeRoot(), "settings.json")
	if !f.ClaudeDir {
		return Action{Kind: "note", State: Add, Path: path,
			Message: "Claude Code is not installed here (" + env.claudeRoot() + " is missing); its hooks were not added"}
	}
	if !f.Jq {
		p.Warnings = append(p.Warnings, "jq is not on PATH: the Claude SessionStart hook reads the session id with jq "+
			"and does nothing without it — install jq (brew install jq)")
	}
	switch f.Settings.Kind {
	case Symlink:
		return Action{Kind: manifest.KindClaudeMerge, State: Manual, Path: path,
			Message: fmt.Sprintf("%s is a symlink, so moox does not write it; merge these hooks into the file it points at:\n%s",
				path, indent(string(snippet(env.Root)), "    "))}
	case Other:
		p.Warnings = append(p.Warnings, path+" is not a regular file; Claude hooks not added")
		return Action{Kind: "note", State: Add, Path: path, Message: "Claude hooks skipped"}
	}
	before := f.Settings.Content
	if f.Settings.Kind == Missing {
		before = nil
	}
	after, err := MergeClaudeSettings(before, env.Root)
	if err != nil {
		p.Warnings = append(p.Warnings, fmt.Sprintf("%s: %v; Claude hooks not added", path, err))
		return Action{Kind: "note", State: Add, Path: path, Message: "Claude hooks skipped"}
	}
	a := Action{Kind: manifest.KindClaudeMerge, State: Add, Path: path,
		claude: &claudeMerge{dir: env.claudeRoot(), name: "settings.json", before: before, after: after}}
	// Already set up: the merge changes nothing beyond formatting.
	if before != nil {
		if v, err := parseOJSON(before); err == nil && string(v.encode()) == string(after) {
			a.State = Already
		}
	}
	return a
}

func indent(s, pre string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i := range lines {
		lines[i] = pre + lines[i]
	}
	return strings.Join(lines, "\n")
}

func hookEntry(root, script string) *ojson {
	return &ojson{kind: '{', obj: []okv{{"type", ostr("command")}, {"command", ostr(HookCommand(root, script))}}}
}

// snippet is the JSON a user merges by hand into a symlinked settings.json.
func snippet(root string) []byte {
	b, _ := MergeClaudeSettings(nil, root)
	return b
}

// MergeClaudeSettings returns settings.json with the two hooks present
// exactly once each. A hook whose command names the same continuity script
// is replaced in place by the vendored one; further copies are removed.
// Other keys and their order are kept. nil input means a new file.
func MergeClaudeSettings(in []byte, root string) ([]byte, error) {
	doc := &ojson{kind: '{'}
	if len(strings.TrimSpace(string(in))) > 0 {
		v, err := parseOJSON(in)
		if err != nil {
			return nil, fmt.Errorf("not valid JSON: %w", err)
		}
		if v.kind != '{' {
			return nil, fmt.Errorf("not a JSON object")
		}
		doc = v
	}
	hooks := doc.get("hooks")
	if hooks == nil {
		hooks = &ojson{kind: '{'}
		doc.set("hooks", hooks)
	} else if hooks.kind != '{' {
		return nil, fmt.Errorf(`"hooks" is not an object`)
	}
	for _, h := range claudeHooks {
		ev := hooks.get(h.event)
		if ev == nil {
			ev = &ojson{kind: '['}
			hooks.set(h.event, ev)
		} else if ev.kind != '[' {
			return nil, fmt.Errorf(`"hooks.%s" is not an array`, h.event)
		}
		placed := false
		var groups []*ojson
		for _, g := range ev.arr {
			list := g.get("hooks")
			if list == nil || list.kind != '[' {
				groups = append(groups, g)
				continue
			}
			var kept []*ojson
			changed := false
			for _, e := range list.arr {
				if !strings.Contains(e.get("command").str(), continuityScripts+h.script) {
					kept = append(kept, e)
					continue
				}
				changed = true
				if !placed {
					kept = append(kept, hookEntry(root, h.script))
					placed = true
				}
			}
			if changed {
				if len(kept) == 0 {
					continue // the group held only a duplicate
				}
				// Keep an unchanged entry byte-for-byte: replace only if it differs.
				list.arr = kept
			}
			groups = append(groups, g)
		}
		if !placed {
			groups = append(groups, &ojson{kind: '{', obj: []okv{{"hooks", &ojson{kind: '[', arr: []*ojson{hookEntry(root, h.script)}}}}})
		}
		ev.arr = groups
	}
	return doc.encode(), nil
}

// RemoveClaudeHooks takes moox's two hook entries out of settings.json by
// value (uninstall, when the file changed since the merge). Groups and event
// arrays it leaves empty are removed, and "hooks" if it is left empty.
func RemoveClaudeHooks(in []byte, root string) ([]byte, bool, error) {
	doc, err := parseOJSON(in)
	if err != nil || doc.kind != '{' {
		return nil, false, fmt.Errorf("settings.json is not a JSON object")
	}
	hooks := doc.get("hooks")
	if hooks == nil || hooks.kind != '{' {
		return in, false, nil
	}
	removed := false
	for _, h := range claudeHooks {
		ev := hooks.get(h.event)
		if ev == nil || ev.kind != '[' {
			continue
		}
		want := HookCommand(root, h.script)
		ours := func(cmd string) bool {
			if root != "" {
				return cmd == want
			}
			// The share root is gone (brew uninstall ran first): any
			// command naming a moox share's vendored script is moox's.
			return strings.Contains(cmd, "/moox/plugins/"+continuityScripts+h.script)
		}
		var groups []*ojson
		for _, g := range ev.arr {
			list := g.get("hooks")
			if list == nil || list.kind != '[' {
				groups = append(groups, g)
				continue
			}
			var kept []*ojson
			for _, e := range list.arr {
				if ours(e.get("command").str()) {
					removed = true
					continue
				}
				kept = append(kept, e)
			}
			if len(kept) == 0 && len(list.arr) > 0 {
				continue
			}
			list.arr = kept
			groups = append(groups, g)
		}
		ev.arr = groups
		if len(groups) == 0 {
			hooks.del(h.event)
		}
	}
	if len(hooks.obj) == 0 {
		doc.del("hooks")
	}
	return doc.encode(), removed, nil
}
