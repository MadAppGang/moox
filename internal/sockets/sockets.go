// Package sockets answers the identity questions about a tmux server socket
// (moox architecture §4.1, §4.2): what it is called for @moox-sockets, whether
// it is THE login default server, which state key it gets, whether a setting
// matches it, and where every per-socket file lives.
//
// Identity is decided on the CANONICAL path, never on a spelling: tmux keeps a
// -S path verbatim but realpaths the -L directory (E3), so "/tmp/x/s" and
// "/private/tmp/x/s" are one socket with two spellings, and two processes must
// agree on its key whatever they were told (A13, X11).
//
// This package is a leaf (stdlib only, §2.3): it reads the filesystem to
// resolve symlinks and never writes.
package sockets

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// LegacyKey is the state key of the login default server (D7: its state keeps
// today's paths).
const LegacyKey = "default"

// tmpRoot is the directory tmux's -L default lives under. A variable only so
// the Legacy tests can point it at a scratch tree that has a /tmp-style
// symlink and a tmux-<uid> directory that is absent, then present.
var tmpRoot = "/tmp"

// Name is what @moox-sockets matches against: the socket file's basename.
func Name(socketPath string) string { return filepath.Base(socketPath) }

// Canonical resolves dir(socketPath) through its DEEPEST EXISTING ancestor:
// EvalSymlinks(that ancestor) + the not-yet-existing remainder + "/" + base.
//
// filepath.EvalSymlinks fails with ENOENT on a path that does not exist, and
// /tmp/tmux-<uid> does not exist after a reboot before the first server
// (R2-M7), so resolving the whole directory would make the key depend on
// whether a server had ever run. The socket file itself is never resolved: it
// may not exist yet either, and a socket is not a symlink.
func Canonical(socketPath string) (string, error) {
	if socketPath == "" {
		return "", errors.New("sockets: empty socket path")
	}
	abs, err := filepath.Abs(socketPath)
	if err != nil {
		return "", fmt.Errorf("sockets: %s: %w", socketPath, err)
	}
	dir, base := filepath.Split(filepath.Clean(abs))
	dir = filepath.Clean(dir)

	cur := dir
	var rest []string
	for {
		_, err := os.Lstat(cur)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("sockets: %s: %w", cur, err)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
	real, err := filepath.EvalSymlinks(cur)
	if err != nil {
		return "", fmt.Errorf("sockets: resolving %s: %w", cur, err)
	}
	parts := append([]string{real}, rest...)
	return filepath.Join(append(parts, base)...), nil
}

// legacyPath is realpath(/tmp)/tmux-<uid>/default.
func legacyPath() (string, error) {
	real, err := filepath.EvalSymlinks(tmpRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(real, "tmux-"+strconv.Itoa(os.Getuid()), "default"), nil
}

// Legacy reports whether the socket is THE login default server:
// Canonical(socket) == EvalSymlinks("/tmp") + "/tmux-<uid>/default".
//
// $TMUX_TMPDIR is deliberately ignored, so a scratch server started with
// TMUX_TMPDIR=<scratch>, or with -S <scratch>/default, is never legacy (the
// 2026-08-18 clone class).
func Legacy(socketPath string) bool {
	c, err := Canonical(socketPath)
	if err != nil {
		return false
	}
	want, err := legacyPath()
	if err != nil {
		return false
	}
	return c == want
}

// Key names the state directory: "default" iff Legacy, otherwise
// base + "@" + hex8(sha256(dir(Canonical))). Every non-legacy socket carries
// the hash, including one in tmux's own directory, so the result does not
// depend on the caller's environment. An unresolvable path hashes its cleaned
// absolute spelling (it cannot be legacy, and the caller has nothing better).
//
// The gate computes this ONCE and exports it as MOOX_STATE_KEY; the hook and
// scripts never recompute it.
func Key(socketPath string) string {
	if Legacy(socketPath) {
		return LegacyKey
	}
	c, err := Canonical(socketPath)
	if err != nil {
		abs, aerr := filepath.Abs(socketPath)
		if aerr != nil {
			abs = socketPath
		}
		c = filepath.Clean(abs)
	}
	sum := sha256.Sum256([]byte(filepath.Dir(c)))
	return filepath.Base(c) + "@" + hex.EncodeToString(sum[:])[:8]
}

// Match reports whether a server named name takes part, given @moox-sockets.
//
// The setting is split on whitespace and each word is a path.Match glob
// against the name; "*" matches every name; an empty setting means "default";
// a malformed pattern matches nothing (Lint reports it).
func Match(name, setting string) bool {
	words := strings.Fields(setting)
	if len(words) == 0 {
		words = []string{LegacyKey}
	}
	for _, w := range words {
		ok, err := path.Match(w, name)
		if err == nil && ok {
			return true
		}
	}
	return false
}

// Lint reports every malformed pattern in a @moox-sockets setting.
func Lint(setting string) []error {
	var errs []error
	for _, w := range strings.Fields(setting) {
		if _, err := path.Match(w, ""); err != nil {
			errs = append(errs, fmt.Errorf("sockets: @moox-sockets pattern %q: %w", w, err))
		}
	}
	return errs
}
