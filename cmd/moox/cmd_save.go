package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/engine"
	"github.com/MadAppGang/moox/internal/mooxroot"
	"github.com/MadAppGang/moox/internal/saver"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

const saveUsage = `usage: moox save [-L NAME | -S PATH] [--reason TEXT] [--wait DURATION] [--direct] [--force]`

// exitDeadline is `--wait` passing with no result (§12.4).
const exitDeadline = 124

// directSaveTimeout bounds an in-process save (§5.6 (4)).
const directSaveTimeout = 120 * time.Second

// cmdSave is `moox save` (§5.5): a request to the server's daemon, answered
// on the requester's OWN option and channel, or — with --direct, or when no
// saver runs — a save in this process under the same save.lock. The result
// word goes to stdout; the exit code follows the §5.5 table.
func cmdSave(m *meta, args []string) int {
	fs := flag.NewFlagSet("save", flag.ContinueOnError)
	fs.SetOutput(m.stderr)
	fs.Usage = func() { fmt.Fprintln(m.stderr, saveUsage) }
	nameL := fs.String("L", "", "server socket NAME")
	pathS := fs.String("S", "", "server socket PATH")
	sock := fs.String("tmux-socket", "", "server socket NAME or PATH (as tt's global flag)")
	reason := fs.String("reason", "", "why (logged)")
	wait := fs.Duration("wait", 30*time.Second, "how long to wait for the daemon's answer")
	direct := fs.Bool("direct", false, "save in this process, not through the daemon")
	force := fs.Bool("force", false, "skip the pristine guard (A15)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	given := 0
	for _, s := range []string{*nameL, *pathS, *sock} {
		if s != "" {
			given++
		}
	}
	switch {
	case fs.NArg() != 0:
		fmt.Fprintf(m.stderr, "moox save: unexpected argument %q\n%s\n", fs.Arg(0), saveUsage)
		return exitUsage
	case given > 1:
		fmt.Fprintf(m.stderr, "moox save: give one of -L, -S, --tmux-socket\n%s\n", saveUsage)
		return exitUsage
	case *nameL != "" && strings.Contains(*nameL, "/"):
		fmt.Fprintf(m.stderr, "moox save: -L takes a NAME; use -S for a path\n")
		return exitUsage
	case *wait <= 0:
		fmt.Fprintf(m.stderr, "moox save: --wait must be positive\n")
		return exitUsage
	}
	// The manifest-aware resolution every verb uses (§8.2 step 4): a bare
	// `moox save` in separate mode targets -L moox, not the default socket
	// (G2-H2).
	target, err := m.target(&sockFlags{path: *nameL + *pathS + *sock})
	if err != nil {
		fmt.Fprintf(m.stderr, "moox save: %v\n", err)
		return exitTmuxAbsent
	}
	e, err := engine.New(engine.Options{Target: target, Env: m.env, Producer: "moox " + mooxroot.VersionString()})
	if err != nil {
		fmt.Fprintf(m.stderr, "moox save: %v\n", err)
		return exitError
	}

	// The server must take part: a save on a server @moox-sockets does not
	// match is "not-active" (exit 3), daemon or not.
	rctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	out, err := target.Run(rctx, "display-message", "-p", "#{@moox-active}")
	cancel()
	if err != nil {
		fmt.Fprintf(m.stderr, "moox save: %s: %v\n", target.Socket, err)
		return exitError
	}
	if strings.TrimSpace(string(out)) != "1" {
		return report(m, "not-active")
	}

	if !*direct && saver.Running(e.Paths()) {
		return requestSave(m, target, *wait, *force, *reason)
	}
	ctx, cancel := context.WithTimeout(m.ctx, directSaveTimeout)
	defer cancel()
	res, err := e.Save(ctx, engine.SaveRequest{Reason: strings.TrimSpace("direct " + *reason), Direct: true,
		Contents: true, Force: *force})
	return report(m, saver.ResultOf(res, err))
}

func report(m *meta, result string) int {
	fmt.Fprintln(m.stdout, result)
	return saver.ExitCode(result)
}

func newNonce() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// requestSave is §5.5 steps 1 and 3.
func requestSave(m *meta, t tmuxtarget.Target, wait time.Duration, force bool, reason string) int {
	nonce := newNonce()
	req := saver.SaveReqPrefix + nonce
	res := saver.SaveResPrefix + nonce
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	_, err := t.Run(ctx, tmuxtarget.Chain(
		[]string{"set-option", "-g", req, saver.FormatSaveRequest(time.Now().Unix(), force, reason)},
		[]string{"wait-for", "-S", saver.ChanSave},
	)...)
	cancel()
	if err != nil {
		fmt.Fprintf(m.stderr, "moox save: sending the request: %v\n", err)
		return exitError
	}
	// The per-request channel is race-free: an answer signalled before this
	// waiter starts is kept for it (Q6).
	wctx, wcancel := context.WithTimeout(m.ctx, wait)
	_, err = t.Run(wctx, "wait-for", saver.SavedChanPrefix+nonce)
	timedOut := wctx.Err() != nil
	wcancel()
	if timedOut {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = t.Run(cctx, "set-option", "-gu", req)
		ccancel()
		fmt.Fprintf(m.stderr, "moox save: no answer from the daemon within %s\n", wait)
		return exitDeadline
	}
	if err != nil {
		fmt.Fprintf(m.stderr, "moox save: waiting for the answer: %v\n", err)
		return exitError
	}
	ctx, cancel = context.WithTimeout(m.ctx, 10*time.Second)
	defer cancel()
	out, err := t.Run(ctx, tmuxtarget.Chain(
		[]string{"show-options", "-gqv", res},
		[]string{"set-option", "-gu", res},
	)...)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox save: reading the answer: %v\n", err)
		return exitError
	}
	_, result, ok := strings.Cut(strings.TrimSpace(string(out)), " ")
	if !ok || result == "" {
		fmt.Fprintf(m.stderr, "moox save: the answer %q does not parse\n", strings.TrimSpace(string(out)))
		return exitError
	}
	return report(m, result)
}
