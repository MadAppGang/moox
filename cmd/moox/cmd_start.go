package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/MadAppGang/moox/internal/saver"
	"github.com/MadAppGang/moox/internal/setup/manifest"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

const startUsage = `usage: moox [-L NAME | -S PATH] [-s SESSION] [--nested]`

// bootWait bounds the wait for a boot restore (§8.2 step 5), in rounds.
const (
	bootWait  = 120 * time.Second
	bootRound = 2 * time.Second
)

// cmdStart is the default verb (§8.2): start the server if none runs (the
// boot gate restores it), wait for the boot restore, then attach — only
// when stdin and stdout are terminals.
func cmdStart(m *meta, args []string) int {
	fs := flagSet(m, "moox", startUsage)
	sf := addSockFlags(fs)
	session := fs.String("s", "", "session to attach to (or create)")
	nested := fs.Bool("nested", false, "attach even inside another tmux server")
	// --dry-attach prints the attach target instead of exec'ing tmux (tests;
	// TestE2E_Start_WaitsForRestoreThenAttachTarget).
	dryAttach := fs.Bool("dry-attach", false, "print the attach target and exit")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := sf.check(); err != nil || fs.NArg() != 0 {
		fmt.Fprintf(m.stderr, "moox: %v\n%s\n", firstErr(err, fs.Args()), startUsage)
		return exitUsage
	}
	t, err := m.target(sf)
	if err != nil {
		fmt.Fprintf(m.stderr, "moox: %v\n", err)
		return exitTmuxAbsent
	}
	// 1. tmux >= 3.6.
	if _, code := m.tmuxVersion(t, "start"); code != exitOK {
		return code
	}
	// 2. First run: set up first (interactive with a terminal, else --yes).
	if r, err := m.roots(); err == nil {
		if _, err := os.Stat(filepath.Join(r.Config, "moox", manifest.FileName)); os.IsNotExist(err) {
			if m.env.Getenv("TMUX") != "" {
				// Inside tmux a first run only shows what setup would do: it
				// would rewrite ~/.tmux.conf under a running server, which
				// the next `prefix r` re-sources (G2-H12).
				out := m.stdout
				m.stdout = m.stderr
				code := cmdSetup(m, []string{"--yes", "--dry-run"})
				m.stdout = out
				if code != exitOK {
					return code
				}
				fmt.Fprintln(m.stderr, "moox: first run inside tmux — nothing was written. Run `moox setup` "+
					"(best from a terminal outside tmux), then `moox`.")
				return exitNotApplic
			}
			fmt.Fprintln(m.stderr, "moox: first run — setting up")
			sa := []string{}
			if !(m.stdinTTY && m.stdoutTTY) {
				sa = append(sa, "--yes")
			}
			// Setup's report goes to stderr: stdout stays the attach line.
			out := m.stdout
			m.stdout = m.stderr
			code := cmdSetup(m, sa)
			m.stdout = out
			if code != exitOK {
				return code
			}
			// The socket may follow the mode setup just chose.
			if !sf.given() {
				if t, err = m.target(sf); err != nil {
					fmt.Fprintf(m.stderr, "moox: %v\n", err)
					return exitTmuxAbsent
				}
			}
		}
	}
	// 3. Nesting.
	if m.env.Getenv("TMUX") != "" {
		if m.sameServer(t) {
			if *session == "" {
				fmt.Fprintln(m.stderr, "moox: already inside this server")
				return exitOK
			}
			if _, err := t.Run(m.ctx, "switch-client", "-t", "="+*session); err != nil {
				fmt.Fprintf(m.stderr, "moox: %v\n", err)
				return exitError
			}
			return exitOK
		}
		if !*nested {
			cur, _, _ := strings.Cut(m.env.Getenv("TMUX"), ",")
			fmt.Fprintf(m.stderr, "moox: you are inside tmux server %s; attaching to %s would nest (use --nested)\n", cur, t.Socket)
			return exitUsage
		}
	}
	// 5. No server yet: start it; the gate decides and runs the boot restore.
	attach := []string{"attach-session"}
	if *session != "" {
		attach = []string{"new-session", "-A", "-s", *session}
	}
	if !alive(m.ctx, t) {
		root, err := m.rootDir()
		if err != nil {
			fmt.Fprintf(m.stderr, "moox: %v\n", err)
			return exitError
		}
		name := *session
		if name == "" {
			name = "main"
		}
		sid, err := saver.StartServer(m.ctx, t, root, name, saver.ServerEnv(m.environ, keepNoAutoRestore(m)...))
		if err != nil {
			fmt.Fprintf(m.stderr, "moox: %v\n", err)
			return exitError
		}
		target := waitBoot(m, t, sid)
		attach = []string{"attach-session", "-t", target}
	}
	if *dryAttach {
		fmt.Fprintln(m.stdout, strings.Join(attach, " "))
		return exitOK
	}
	// 6. Attach only with a terminal (R2-M1): the detached restart and
	// scripts stop here.
	if !(m.stdinTTY && m.stdoutTTY) {
		fmt.Fprintf(m.stderr, "moox: server %s is running; attach from a terminal with `moox`\n", t.Socket)
		return exitOK
	}
	argv := append([]string{t.Bin}, t.Args(attach...)...)
	env := m.environ
	if *nested {
		env = withoutVar(env, "TMUX")
	}
	err = syscall.Exec(t.Bin, argv, env)
	fmt.Fprintf(m.stderr, "moox: exec tmux: %v\n", err)
	return exitError
}

func firstErr(err error, rest []string) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("unexpected argument %q", strings.Join(rest, " "))
}

// keepNoAutoRestore passes an explicit MOOX_NO_AUTO_RESTORE=1 the caller set
// (`moox restart --snapshot`, R3-M1); every other MOOX_* is cleared.
func keepNoAutoRestore(m *meta) []string {
	if m.env.Getenv("MOOX_NO_AUTO_RESTORE") == "1" {
		return []string{"MOOX_NO_AUTO_RESTORE=1"}
	}
	return nil
}

func withoutVar(env []string, name string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return out
}

// waitBoot is §8.2 step 5's wait: when the gate decided a boot restore
// (@moox-restoring set on return), wait up to 120 s in 2 s rounds of
// `wait-for moox-restored`, re-reading @moox-boot-result between rounds (a
// signal sent before a round began is not kept for it, Q6 (b)). It returns
// the session to attach: @moox-restore-target, else the bootstrap's id, else
// the first session left.
func waitBoot(m *meta, t tmuxtarget.Target, bootstrap string) string {
	restoring, _ := option(m.ctx, t, "@moox-restoring")
	if restoring != "" {
		fmt.Fprintf(m.stderr, "moox: restoring the saved workspace on %s …\n", filepath.Base(t.Socket))
		deadline := time.Now().Add(bootWait)
		for {
			res, err := option(m.ctx, t, "@moox-boot-result")
			if err != nil || res != "" {
				if res != "" {
					fmt.Fprintf(m.stderr, "moox: boot restore %s\n", res)
				}
				break
			}
			if time.Now().After(deadline) || m.ctx.Err() != nil {
				fmt.Fprintln(m.stderr, "moox: the boot restore is still running; attaching anyway")
				break
			}
			wctx, cancel := context.WithTimeout(m.ctx, bootRound)
			_, _ = t.Run(wctx, "wait-for", "moox-restored")
			cancel()
		}
	}
	if target, _ := option(m.ctx, t, "@moox-restore-target"); target != "" && hasSession(m.ctx, t, target) {
		return target
	}
	if bootstrap != "" && hasSession(m.ctx, t, bootstrap) {
		return bootstrap
	}
	cctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
	defer cancel()
	out, err := t.Run(cctx, "list-sessions", "-F", "#{session_id}")
	if err == nil {
		if f := strings.Fields(string(out)); len(f) > 0 {
			return f[0]
		}
	}
	return bootstrap
}

func hasSession(ctx context.Context, t tmuxtarget.Target, id string) bool {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err := t.Run(cctx, "has-session", "-t", id)
	return err == nil
}
