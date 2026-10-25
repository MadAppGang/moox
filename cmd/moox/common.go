package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/engine"
	"github.com/MadAppGang/moox/internal/mooxroot"
	"github.com/MadAppGang/moox/internal/setup"
	"github.com/MadAppGang/moox/internal/setup/manifest"
	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// sockFlags are -L NAME / -S PATH.
type sockFlags struct{ name, path string }

func addSockFlags(fs *flag.FlagSet) *sockFlags {
	s := &sockFlags{}
	fs.StringVar(&s.name, "L", "", "server socket NAME")
	fs.StringVar(&s.path, "S", "", "server socket PATH")
	return s
}

func (s *sockFlags) check() error {
	switch {
	case s.name != "" && s.path != "":
		return errors.New("give one of -L and -S")
	case strings.Contains(s.name, "/"):
		return errors.New("-L takes a NAME; use -S for a path")
	}
	return nil
}

func (s *sockFlags) given() bool { return s.name != "" || s.path != "" }

// target resolves the server (§8.2 step 4): -L/-S > $MOOX_TMUX_SOCKET > the
// first field of $TMUX > the manifest's mode (separate → -L moox, replace →
// the default socket) > the default socket.
func (m *meta) target(s *sockFlags) (tmuxtarget.Target, error) {
	flagSock := ""
	if s != nil {
		flagSock = s.name + s.path
	}
	if flagSock == "" && m.env.Getenv("MOOX_TMUX_SOCKET") == "" && m.env.Getenv("TMUX") == "" {
		if mf := m.manifest(); mf != nil && mf.Mode == string(setup.Separate) {
			flagSock = setup.SeparateSocket
		}
	}
	return tmuxtarget.Resolve(m.env, "MOOX", flagSock, "")
}

// roots are the XDG roots of this environment.
func (m *meta) roots() (sockets.Roots, error) { return sockets.RootsFrom(m.env) }

// manifest is XC/moox/setup.json, or nil.
func (m *meta) manifest() *manifest.Manifest {
	r, err := m.roots()
	if err != nil {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(r.Config, "moox", manifest.FileName))
	if err != nil {
		return nil
	}
	mf, err := manifest.Decode(b)
	if err != nil {
		return nil
	}
	return mf
}

func (m *meta) engine(t tmuxtarget.Target) (*engine.Engine, error) {
	return engine.New(engine.Options{Target: t, Env: m.env, Producer: "moox " + mooxroot.VersionString()})
}

// lookPath finds a program on THIS environment's PATH.
func (m *meta) lookPath(name string) (string, error) {
	for _, dir := range filepath.SplitList(m.env.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		p := filepath.Join(dir, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() && fi.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", exec.ErrNotFound
}

// tmuxVersion checks D1's floor (§8.2 step 1): exit 69 below 3.6 or with no
// tmux at all; next-* and master pass.
func (m *meta) tmuxVersion(t tmuxtarget.Target, verb string) (tmuxtarget.Version, int) {
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()
	v, err := t.Version(ctx)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox %s: tmux not found or not runnable (%v); install tmux 3.6 or later\n", verb, err)
		return v, exitTmuxAbsent
	}
	if !v.AtLeast(3, 6) {
		fmt.Fprintf(m.stderr, "moox %s: %s is too old; moox needs tmux 3.6 or later (brew upgrade tmux)\n", verb, v)
		return v, exitTmuxAbsent
	}
	return v, exitOK
}

// alive reports whether the server answers.
func alive(ctx context.Context, t tmuxtarget.Target) bool {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := t.Run(cctx, "display-message", "-p", "#{pid}")
	return err == nil
}

// sameServer: $TMUX names the target server.
func (m *meta) sameServer(t tmuxtarget.Target) bool {
	tm := m.env.Getenv("TMUX")
	if tm == "" {
		return false
	}
	sock, _, _ := strings.Cut(tm, ",")
	a, err1 := sockets.Canonical(sock)
	b, err2 := sockets.Canonical(t.Socket)
	return err1 == nil && err2 == nil && a == b
}

func flagSet(m *meta, name, usage string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(m.stderr)
	fs.Usage = func() { fmt.Fprintln(m.stderr, usage) }
	return fs
}

// option reads one global option.
func option(ctx context.Context, t tmuxtarget.Target, name string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := t.Run(cctx, "show-options", "-gqv", name)
	return strings.TrimRight(string(out), "\n"), err
}
