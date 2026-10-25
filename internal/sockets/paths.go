package sockets

import (
	"errors"
	"path/filepath"
)

// Env is the part of the environment the path layout reads.
type Env interface{ Getenv(string) string }

// ErrNoHome means $HOME is unset or relative, so no path can be built. (§4.1
// wrote `For(...) Paths`; a layout rooted at "" would put state under "/".)
var ErrNoHome = errors.New("sockets: HOME is not an absolute path")

// Paths is every per-socket location (§4.2, exact). The legacy column — today's
// paths — is used for the "default" key and nothing else (D7, no migration).
type Paths struct {
	Socket string // as given
	Key    string // LegacyKey or base@hex8
	Legacy bool

	// Snapshots holds moox-<ts>.json, moox-<ts>.contents.gz and moox-last.
	// For default it is resurrect's directory; resurrect's own files there
	// are never touched (A6).
	Snapshots string
	Launch    string // launch recordings <N>
	Pending   string // pending files <N>
	Panes     string // Claude position sidecars <S>-<W>-<P>.session-id
	// ByPidDir is the SHARED by-pid registry. One rule (R3-C2): the option
	// @claude-continuity-by-pid-dir if set (test servers only), else this
	// default. Never derived from Panes. Use ByPid to apply the option.
	ByPidDir  string
	FreezeDir string // $HOME/.config/tmux-cc/frozen/<key>

	State  string // moox runtime: save.lock, restore/, restoring-from, pins, active, saver.state, daemon.log
	Log    string // State/log (§4.2a: new for default as well)
	Lock   string // State/save.lock — created once, never unlinked (§5.6)
	Marker string // State/active — exists exactly while the server participates (§3.3)

	TTState     string // observer.sock, observer.pid, agents-cache.*, panel-mode
	TTPanelConf string // shared
	UserConfig  string // XC/moox: local.conf, setup.json, backups/ (shared)
	// Veto is $HOME/.config/tmux-claude/moox-excluded: present exactly while a
	// gate on the legacy default socket did not match it (§3.3, R3-C4).
	// Meaningful for the Legacy socket only.
	Veto string
}

// ByPid applies the @claude-continuity-by-pid-dir option (R3-C2): the option
// if non-empty, else the shared default.
func (p Paths) ByPid(option string) string {
	if option != "" {
		return option
	}
	return p.ByPidDir
}

// Roots are the XDG base directories with their defaults applied.
type Roots struct{ Home, State, Data, Config string }

// RootsFrom reads HOME and XDG_STATE_HOME/XDG_DATA_HOME/XDG_CONFIG_HOME. A
// relative XDG value is ignored, as the XDG spec requires.
func RootsFrom(env Env) (Roots, error) {
	home := env.Getenv("HOME")
	if home == "" || !filepath.IsAbs(home) {
		return Roots{}, ErrNoHome
	}
	home = filepath.Clean(home)
	xdg := func(name, def string) string {
		if v := env.Getenv(name); v != "" && filepath.IsAbs(v) {
			return filepath.Clean(v)
		}
		return filepath.Join(home, def)
	}
	return Roots{
		Home:   home,
		State:  xdg("XDG_STATE_HOME", ".local/state"),
		Data:   xdg("XDG_DATA_HOME", ".local/share"),
		Config: xdg("XDG_CONFIG_HOME", ".config"),
	}, nil
}

// For returns every per-socket path (§4.2) for a socket and an environment.
// The continuity hook uses a literal $HOME/.config (not XDG_CONFIG_HOME) for
// its legacy dirs, and that literal stays.
func For(socketPath string, env Env) (Paths, error) {
	r, err := RootsFrom(env)
	if err != nil {
		return Paths{}, err
	}
	key := Key(socketPath)
	legacy := key == LegacyKey
	cc := filepath.Join(r.Home, ".config", "tmux-claude")
	state := filepath.Join(r.State, "moox", "sockets", key)

	p := Paths{
		Socket:      socketPath,
		Key:         key,
		Legacy:      legacy,
		ByPidDir:    filepath.Join(cc, "panes", "by-pid"),
		FreezeDir:   filepath.Join(r.Home, ".config", "tmux-cc", "frozen", key),
		State:       state,
		Log:         filepath.Join(state, "log"),
		Lock:        filepath.Join(state, "save.lock"),
		Marker:      filepath.Join(state, "active"),
		TTPanelConf: filepath.Join(r.Config, "tt", "panel.conf"),
		UserConfig:  filepath.Join(r.Config, "moox"),
		Veto:        filepath.Join(cc, "moox-excluded"),
	}
	if legacy {
		p.Snapshots = filepath.Join(r.Data, "tmux", "resurrect")
		p.Launch = filepath.Join(cc, "launch")
		p.Pending = filepath.Join(cc, "pending")
		p.Panes = filepath.Join(cc, "panes")
		p.TTState = filepath.Join(r.State, "tt")
	} else {
		p.Snapshots = filepath.Join(state, "snapshots")
		p.Launch = filepath.Join(state, "launch")
		p.Pending = filepath.Join(state, "pending")
		p.Panes = filepath.Join(state, "panes")
		p.TTState = filepath.Join(r.State, "tt", "sockets", key)
	}
	return p, nil
}
