package saver

// client.go is the REQUESTER half of §6.4a, shared by `moox restore`,
// `moox restart --snapshot` and the tt TUI's restore offer: neither process
// restores by itself (PR-C3). It bootstraps a server with handover when none
// runs, makes sure a daemon serves it, sends ONE per-nonce request, and
// waits on its own channels for the answer and for the panes.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MadAppGang/moox/internal/engine"
	"github.com/MadAppGang/moox/internal/sockets"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// Running reports whether a `tt serve` saver serves the server: the
// observer pid file in the per-socket tt dir names a live process AND
// saver.state names the same pid (a bare `tt observe` writes the same pid
// file but runs no saver).
func Running(p sockets.Paths) bool {
	data, err := os.ReadFile(filepath.Join(p.TTState, "observer.pid"))
	if err != nil {
		return false
	}
	var rec struct{ PID int }
	if json.Unmarshal(data, &rec) != nil || rec.PID <= 0 {
		return false
	}
	if err := syscall.Kill(rec.PID, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	st, err := os.ReadFile(filepath.Join(p.State, "saver.state"))
	if err != nil {
		return false
	}
	pid, _, _ := strings.Cut(strings.TrimSpace(string(st)), " ")
	return pid == strconv.Itoa(rec.PID)
}

// NewNonce is a request's 16-hex nonce.
func NewNonce() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Errors of the requester (cmd/moox maps them to §12.4 exit codes).
var (
	// ErrRefused: the daemon (or the local precondition check) refused; the
	// error text carries the reason word. Exit 3 (75 for busy-restoring).
	ErrRefused = errors.New("restore refused")
	// ErrBusyRestoring: refused busy-restoring (exit 75).
	ErrBusyRestoring = errors.New("restore refused: busy-restoring")
	// ErrNoAnswer: the daemon did not answer in time (exit 124).
	ErrNoAnswer = errors.New("no answer from the daemon")
	// ErrFailed: the daemon started the restore and Restore errored (exit 1).
	ErrFailed = errors.New("restore failed")
)

// Client is one requester for one server.
type Client struct {
	Target tmuxtarget.Target
	Paths  sockets.Paths
	// Root is the share root: the bootstrap runs <Root>/moox.conf.
	Root string
	// TT is the tt binary that `serve --ensure` runs; "" means
	// #{@moox-bin}/tt read from the server.
	TT string
	// Environ is the environment of the bootstrap server and of tt.
	Environ []string
	// Session is the bootstrap session's name ("main" for moox).
	Session string
	// AckWait bounds the wait for started/refused, PanesWait the wait for
	// moox-restored-<nonce>.
	AckWait, PanesWait time.Duration
}

// Outcome is what a request produced.
type Outcome struct {
	Bootstrapped bool
	Nonce        string
	// Result is @moox-restore-result (PASS | INCOMPLETE | NOTHING).
	Result string
	// Target is @moox-restore-target ($sid), when no client was attached.
	Target string
}

func (c Client) run(ctx context.Context, d time.Duration, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	return c.Target.Run(cctx, args...)
}

// Alive reports whether the server answers.
func (c Client) Alive(ctx context.Context) bool {
	_, err := c.run(ctx, 5*time.Second, "display-message", "-p", "#{pid}")
	return err == nil
}

// ServerEnv is environ with every MOOX_* variable removed except MOOX_BIN
// (tests and dev trees point the config at their binaries with it, §12.4)
// and MOOX_STUB_COMMANDS=1 (it only ever makes a restore safer: the boot
// daemon reads it from the server's env, and dropping it relaunched real
// commands from a stub run, G2-H7), and TMUX/TMUX_PANE removed, plus extra
// (e.g. MOOX_NO_AUTO_RESTORE=1).
func ServerEnv(environ []string, extra ...string) []string {
	out := make([]string, 0, len(environ)+len(extra))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		switch {
		case name == "TMUX" || name == "TMUX_PANE" || name == "TT_NO_AUTO_RESTORE":
			continue
		case kv == "MOOX_STUB_COMMANDS=1":
		case strings.HasPrefix(name, "MOOX_") && name != "MOOX_BIN":
			continue
		}
		out = append(out, kv)
	}
	return append(out, extra...)
}

// StartServer starts the server on its socket the way §8.2 step 5 does:
//
//	<tmux> -u -S <S> -f <root>/moox.conf new-session -d -s <session> -P -F '#{session_id}'
//
// tmux runs the whole config (gate included) before the new-session, so on
// return @moox-restoring is decided. It returns the new session's id.
func StartServer(ctx context.Context, t tmuxtarget.Target, root, session string, env []string) (string, error) {
	conf := filepath.Join(root, "moox.conf")
	if _, err := os.Stat(conf); err != nil {
		return "", fmt.Errorf("moox config %s: %w", conf, err)
	}
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, t.Bin, t.Args("-f", conf, "new-session", "-d", "-s", session, "-P", "-F", "#{session_id}")...)
	cmd.Env = env
	if home := envValue(env, "HOME"); home != "" {
		cmd.Dir = home
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("starting the server on %s: %v: %s", t.Socket, err, strings.TrimSpace(string(out)))
	}
	id := strings.TrimSpace(string(out))
	if i := strings.LastIndex(id, "\n"); i >= 0 {
		id = strings.TrimSpace(id[i+1:])
	}
	return id, nil
}

func envValue(env []string, name string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == name {
			return v
		}
	}
	return ""
}

// Bootstrap starts the server with handover (MOOX_NO_AUTO_RESTORE=1) when
// nothing answers on the socket (§6.4a step 0, R2-H3): the gate takes check
// 4 (HANDOVER), forks the daemon as saver, and does not restore Last().
func (c Client) Bootstrap(ctx context.Context) (bool, error) {
	if c.Alive(ctx) {
		return false, nil
	}
	session := c.Session
	if session == "" {
		session = "main"
	}
	_, err := StartServer(ctx, c.Target, c.Root, session, ServerEnv(c.Environ, "MOOX_NO_AUTO_RESTORE=1"))
	return err == nil, err
}

// EnsureDaemon is step 1: with no live saver, run `tt serve --ensure`; the
// server must then be matched (@moox-active 1).
func (c Client) EnsureDaemon(ctx context.Context) error {
	// A daemon the gate forked a moment ago may not have written its state
	// yet: give it a few seconds before asking for another (a signal sent
	// meanwhile is kept for its waiter, PV6).
	deadline := time.Now().Add(5 * time.Second)
	for !Running(c.Paths) && time.Now().Before(deadline) && c.daemonStarting(ctx) {
		t := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	if !Running(c.Paths) {
		tt := c.TT
		if tt == "" {
			out, err := c.run(ctx, 5*time.Second, "display-message", "-p", "#{@moox-bin}")
			if err != nil {
				return err
			}
			if bin := strings.TrimSpace(string(out)); bin != "" {
				tt = filepath.Join(bin, "tt")
			}
		}
		if tt != "" {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			cmd := exec.CommandContext(cctx, tt, "serve", "--ensure", "--tmux-socket", c.Target.Socket, "--tmux-bin", c.Target.Bin)
			cmd.Env = ServerEnv(c.Environ)
			_ = cmd.Run() // --ensure always exits 0 (RR12); the outcome is @moox-active
			cancel()
		}
	}
	out, err := c.run(ctx, 5*time.Second, "display-message", "-p", "#{@moox-active}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "1" {
		return fmt.Errorf("%w: not-active (the server is not matched by @moox-sockets)", ErrRefused)
	}
	return nil
}

// Request is steps 2–4 for an absolute, already validated snapshot path:
// one chained `set @moox-restore-req-<nonce> … ; wait-for -S
// moox-restore-req`, then the requester's own channels.
func (c Client) Request(ctx context.Context, path string, stub bool) (Outcome, error) {
	o := Outcome{Nonce: NewNonce()}
	req := RestoreReqPrefix + o.Nonce
	res := RestoreResPrefix + o.Nonce
	if _, err := c.run(ctx, 10*time.Second, tmuxtarget.Chain(
		[]string{"set-option", "-g", req, FormatRestoreRequest(os.Getpid(), time.Now().Unix(), stub, path)},
		[]string{"wait-for", "-S", ChanRestoreReq},
	)...); err != nil {
		return o, fmt.Errorf("sending the request: %w", err)
	}
	ackWait := c.AckWait
	if ackWait <= 0 {
		ackWait = 60 * time.Second
	}
	if err := c.waitChan(ctx, RestoreAckPrefix+o.Nonce, ackWait); err != nil {
		_, _ = c.run(context.Background(), 5*time.Second, "set-option", "-gu", req)
		return o, err
	}
	answer, err := c.option(ctx, res)
	if err != nil {
		return o, err
	}
	switch {
	case answer == "started":
	case answer == "refused busy-restoring":
		c.unset(res)
		return o, ErrBusyRestoring
	case strings.HasPrefix(answer, "refused"):
		c.unset(res)
		return o, fmt.Errorf("%w: %s", ErrRefused, strings.TrimSpace(strings.TrimPrefix(answer, "refused")))
	default:
		c.unset(res)
		return o, fmt.Errorf("unexpected answer %q", answer)
	}
	panesWait := c.PanesWait
	if panesWait <= 0 {
		panesWait = 10 * time.Minute
	}
	if err := c.waitChan(ctx, RestoredPrefix+o.Nonce, panesWait); err != nil {
		return o, err
	}
	out, err := c.run(ctx, 10*time.Second, "display-message", "-p",
		"#{"+res+"}"+FieldSep+"#{@moox-restore-result}"+FieldSep+"#{@moox-restore-target}")
	c.unset(res)
	if err != nil {
		return o, err
	}
	f := strings.SplitN(strings.TrimRight(string(out), "\n"), FieldSep, 3)
	for len(f) < 3 {
		f = append(f, "")
	}
	if strings.HasPrefix(f[0], "failed") {
		return o, fmt.Errorf("%w: %s", ErrFailed, strings.TrimSpace(strings.TrimPrefix(f[0], "failed")))
	}
	o.Result, o.Target = f[1], f[2]
	return o, nil
}

// WaitLaunch waits until @moox-restoring is unset (the LAUNCH verdict), in
// 2 s rounds that re-read it, within d.
func (c Client) WaitLaunch(ctx context.Context, d time.Duration) error {
	deadline := time.Now().Add(d)
	for {
		v, err := c.option(ctx, "@moox-restoring")
		if err != nil {
			return err
		}
		if v == "" {
			return nil
		}
		if time.Now().After(deadline) {
			return ErrNoAnswer
		}
		// A round: block on the boot channel for at most 2 s (a signal that
		// is not for us costs one early re-read).
		_ = c.waitChan(ctx, "moox-restored", 2*time.Second)
	}
}

func (c Client) unset(name string) {
	_, _ = c.run(context.Background(), 5*time.Second, "set-option", "-gu", name)
}

func (c Client) option(ctx context.Context, name string) (string, error) {
	out, err := c.run(ctx, 10*time.Second, "show-options", "-gqv", name)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// waitChan blocks on `wait-for <ch>` for at most d (per-nonce channels are
// race-free: a signal sent before the wait began is kept for it, Q6).
func (c Client) waitChan(ctx context.Context, ch string, d time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	_, err := c.Target.Run(wctx, "wait-for", ch)
	if wctx.Err() != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrNoAnswer
	}
	return err
}

// ResolveSnapshot is step 2's validation: path made absolute against cwd,
// read, decoded and validated; "" means Last()'s absolute path.
func ResolveSnapshot(e *engine.Engine, path, cwd string) (string, error) {
	if path == "" {
		last, err := e.Last()
		if err != nil {
			return "", err
		}
		return last.Path, nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	return path, validSnapshot(path)
}

// daemonStarting: the server is matched (the gate ran and forked a daemon),
// so waiting briefly for its state is worth it.
func (c Client) daemonStarting(ctx context.Context) bool {
	out, err := c.run(ctx, 5*time.Second, "display-message", "-p", "#{@moox-active}")
	return err == nil && strings.TrimSpace(string(out)) == "1"
}
