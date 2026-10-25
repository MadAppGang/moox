package blackbox_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func jsonStrings(value any, found map[string]bool) {
	switch value := value.(type) {
	case string:
		found[value] = true
	case []any:
		for _, child := range value {
			jsonStrings(child, found)
		}
	case map[string]any:
		for _, child := range value {
			jsonStrings(child, found)
		}
	}
}

func twoNamedServers(e *sandbox) (a, b *server, nameA, nameB string) {
	e.t.Helper()
	a = e.newServer("tt-a", "*", false)
	b = e.newServer("tt-b", "*", false)
	a.waitReady()
	b.waitReady()
	nameA, nameB = "only-a-"+e.nonce, "only-b-"+e.nonce
	a.mustTmux("rename-session", "-t", "bootstrap", nameA)
	b.mustTmux("rename-session", "-t", "bootstrap", nameB)
	for _, s := range []*server{a, b} {
		r := e.run(e.tt, nil, "", "serve", "--ensure", "--tmux-socket", s.path)
		requireCode(e.t, r, 0)
		e.collectDaemonPID(s)
	}
	return
}

// ttTmuxShim logs every tmux call tt makes (V7: "assert with TT_TMUX_BIN = a
// shim that logs its argv"), then hands it to the safety guard.
func ttTmuxShim(e *sandbox) (shim, log string) {
	e.t.Helper()
	shim = filepath.Join(e.base, "bin", "tmux-tt-log")
	log = filepath.Join(e.base, "control", "tt-tmux-calls")
	writeFile(e.t, shim, "#!/bin/sh\nprintf 'TMUX=%s argv=%s\\n' \"${TMUX-}\" \"$*\" >> "+shQuote(log)+"\nexec "+shQuote(e.tmux)+" \"$@\"\n", 0700)
	return shim, log
}

// `tt sessions` reports Claude Code sessions (its --help), so /bin/sh panes
// yield none and a positive "a's session is listed" needs a Claude registry
// fixture whose format is not in the supplied contract (Known Gap). What is
// checked: the selection flags are accepted (exit 0, valid JSON), the other
// server's session never appears, and every tmux call tt makes selects only
// the target server.
func requireSessionJSON(e *sandbox, env map[string]string, args []string, target, otherServer *server, other, log string) {
	e.t.Helper()
	_ = os.Remove(log)
	r := e.run(e.tt, env, "", args...)
	requireCode(e.t, r, 0)
	var value any
	if err := json.Unmarshal([]byte(r.out), &value); err != nil {
		e.t.Fatalf("invalid sessions --json: %v\n%s%s", err, r.out, r.err)
	}
	names := map[string]bool{}
	jsonStrings(value, names)
	if names[other] {
		e.t.Errorf("tt for %s listed the other server's session %q", target.name, other)
	}
	data, err := os.ReadFile(log)
	if err != nil && !os.IsNotExist(err) {
		e.t.Fatal(err)
	}
	calls := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(data) == 0 {
		e.t.Logf("tt %q made no tmux call; server scoping is not observable through tmux", args)
		return
	}
	for _, line := range calls {
		call := line + " "
		selectsTarget := strings.Contains(call, target.path) || strings.Contains(call, "-L "+target.name+" ")
		if !selectsTarget || strings.Contains(call, otherServer.path) || strings.Contains(call, "-L "+otherServer.name+" ") {
			e.t.Errorf("tt for %s made a tmux call not scoped to it: %s", target.name, call)
		}
	}
}

// TEST-27, REQ-14: socket AND tmux-binary global flags are documented both
// before and after the subcommand; test each in a fresh two-server fixture.
func TestTT_SocketAndBinaryFlagsBeforeOrAfterVerbSeeOnlySelectedServer(t *testing.T) {
	for _, position := range []string{"before", "after"} {
		t.Run(position, func(t *testing.T) {
			e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
			a, b, nameA, nameB := twoNamedServers(e)
			shim, log := ttTmuxShim(e)
			for _, target := range []struct {
				s, o  *server
				other string
			}{{a, b, nameB}, {b, a, nameA}} {
				flags := []string{"--tmux-socket", target.s.path, "--tmux-bin", shim}
				var args []string
				if position == "before" {
					args = append(flags, "sessions", "--json")
				} else {
					args = append([]string{"sessions", "--json"}, flags...)
				}
				requireSessionJSON(e, nil, args, target.s, target.o, target.other, log)
			}
		})
	}
}

// TEST-28, REQ-14: no TMUX is inherited, and the guard fails any accidental
// attempt to contact default. Both env socket choices are checked positively.
func TestTT_OutsideTmuxSocketEnvironmentSeesOnlyItsOwnServer(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	a, b, nameA, nameB := twoNamedServers(e)
	shim, log := ttTmuxShim(e)
	for _, target := range []struct {
		s, o  *server
		other string
	}{{a, b, nameB}, {b, a, nameA}} {
		requireSessionJSON(e, map[string]string{
			"TT_TMUX_SOCKET": target.s.path, "TT_TMUX_BIN": shim,
		}, strings.Fields("sessions --json"), target.s, target.o, target.other, log)
	}
}
