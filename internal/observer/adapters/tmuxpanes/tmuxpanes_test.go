package tmuxpanes

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// No test in this file starts, touches, or even names a tmux server. Every one
// of them substitutes runPanes, which is why the variable exists — on
// 2026-08-17 a scratch tmux server on this machine sourced ~/.tmux.conf and its
// plugins cloned the entire workspace, destroying 16 sessions, 44 windows and
// 71 panes. Twice.

func withOutput(t *testing.T, out string, err error) *int {
	t.Helper()
	calls := 0
	prev := runPanes
	runPanes = func(ctx context.Context, _ tmuxtarget.Target) ([]byte, error) {
		calls++
		return []byte(out), err
	}
	t.Cleanup(func() { runPanes = prev })
	return &calls
}

func TestSigilsAreKeptExactlyAsTmuxPrintsThem(t *testing.T) {
	// Measured fact 7: the @ and % are PART of the window and pane
	// identifiers. Stripping them yields ids tmux will not match, and it fails
	// SILENTLY — no error, just a pane that never resolves, which for a
	// liveness gate means reporting every live session as dead.
	withOutput(t, strings.Join([]string{
		"madbench:@51.%107",
		"magai:@15.%24",
		"orbit-demo:@3.%5",
	}, "\n")+"\n", nil)

	panes, err := New(tmuxtarget.Target{}).ListPanes(context.Background())
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	for _, want := range []string{"madbench:@51.%107", "magai:@15.%24", "orbit-demo:@3.%5"} {
		if !panes[want] {
			t.Errorf("pane %q missing; got keys %v", want, keys(panes))
		}
	}
	// The de-sigilled form must NOT be a key. If it were, a caller comparing
	// against the registry's raw reference would silently match nothing.
	if panes["madbench:51.107"] {
		t.Error("a de-sigilled pane id is present, so the sigils were stripped somewhere")
	}
}

func TestSessionNamesContainingColonsSurvive(t *testing.T) {
	// A tmux session name may contain colons. The pane id is used VERBATIM as
	// a map key and is never split, so this works by construction — this test
	// is here to keep it that way if someone later adds parsing.
	withOutput(t, "mag:api:worker:@7.%13\n", nil)

	panes, err := New(tmuxtarget.Target{}).ListPanes(context.Background())
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if !panes["mag:api:worker:@7.%13"] {
		t.Errorf("colon-bearing session name lost; got %v", keys(panes))
	}
}

func TestAFailedListingIsAnErrorAndNeverAnEmptySet(t *testing.T) {
	// This is the whole reason ListPanes returns an error at all. An empty set
	// returned as a success marks every pane as gone, therefore every
	// interactive session as dead, therefore the entire default view as empty
	// — from one tmux hiccup. That exact bug class ("one malformed record
	// blanked everything") has shipped in this repository once already.
	withOutput(t, "no server running on /tmp/tmux-501/default\n", errors.New("exit status 1"))

	panes, err := New(tmuxtarget.Target{}).ListPanes(context.Background())
	if err == nil {
		t.Fatal("a failed listing returned no error")
	}
	if panes != nil {
		t.Errorf("a failed listing returned a non-nil set: %v", keys(panes))
	}
	if !strings.Contains(err.Error(), "no server running") {
		t.Errorf("tmux's own message is not in the error: %v", err)
	}
}

func TestZeroPanesWithASuccessfulExitIsAlsoAnError(t *testing.T) {
	// A running tmux server always has at least one pane. Zero lines with exit
	// 0 is a malfunction, not an answer, and the same reasoning as above
	// applies: the safe report is "we could not ask".
	withOutput(t, "\n\n", nil)

	if _, err := New(tmuxtarget.Target{}).ListPanes(context.Background()); err == nil {
		t.Fatal("an empty successful listing returned no error")
	}
}

func TestOneMalformedLineDoesNotDiscardTheRest(t *testing.T) {
	withOutput(t, strings.Join([]string{
		"madbench:@51.%107",
		"this is not a pane id",
		"",
		"magai:@15.%24",
	}, "\n"), nil)

	panes, err := New(tmuxtarget.Target{}).ListPanes(context.Background())
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if len(panes) != 2 {
		t.Fatalf("expected the two well-formed ids to survive, got %v", keys(panes))
	}
	if panes["this is not a pane id"] {
		t.Error("a malformed line became a pane id")
	}
}

func TestTheGateIsAskedExactlyOncePerCall(t *testing.T) {
	calls := withOutput(t, "madbench:@51.%107\n", nil)
	if _, err := New(tmuxtarget.Target{}).ListPanes(context.Background()); err != nil {
		t.Fatalf("ListPanes: %v", err)
	}
	if *calls != 1 {
		t.Errorf("tmux was invoked %d times for one call; one list-panes -a covers the whole server", *calls)
	}
}

func TestTheCommandIsReadOnlyAndServerWide(t *testing.T) {
	// The hard rules, as assertions rather than as comments.
	if ListPanesArgs[0] != "list-panes" {
		t.Errorf("subcommand is %q; only list-panes, list-sessions and display-message -p are permitted",
			ListPanesArgs[0])
	}

	joined := strings.Join(ListPanesArgs, " ")
	for _, banned := range []string{"run-shell", "kill-", "split-window", "new-session", "set-option", "set-hook"} {
		if strings.Contains(joined, banned) {
			t.Errorf("argv contains %q, which is banned by name: %q", banned, joined)
		}
	}

	// -a is required, not incidental. A session's pane can be in any window,
	// so a window-scoped listing would report panes as gone merely because the
	// observer was looking somewhere else. (§5.5b's pane-singleton guard is
	// deliberately the opposite — window-scoped. Two different questions.)
	found := false
	for _, a := range ListPanesArgs {
		if a == "-a" {
			found = true
		}
	}
	if !found {
		t.Errorf("argv is not server-wide (-a missing): %q", joined)
	}

	if got := ListPanesArgs[len(ListPanesArgs)-1]; got != PaneFormat+"\t#{pane_pid}" {
		t.Errorf("the -F format is %q; it must be PaneFormat, a TAB, and #{pane_pid} (moox §4.5)", got)
	}
	if PaneFormat != "#{session_name}:#{window_id}.#{pane_id}" {
		t.Errorf("PaneFormat is %q; it must be character-for-character the form the registry's tmux field carries", PaneFormat)
	}
}

func TestPanePIDsComeFromTheSameListing(t *testing.T) {
	calls := withOutput(t, "madbench:@51.%107\t4242\nmag:api.x:@7.%13\t99\nodd:@1.%2\tnot-a-pid\n", nil)
	panes, pids, err := New(tmuxtarget.Target{}).ListPanePIDs(context.Background())
	if err != nil {
		t.Fatalf("ListPanePIDs: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("tmux was invoked %d times; ids and pids are one listing", *calls)
	}
	for _, want := range []string{"madbench:@51.%107", "mag:api.x:@7.%13", "odd:@1.%2"} {
		if !panes[want] {
			t.Errorf("pane %q missing; got %v", want, keys(panes))
		}
	}
	if len(pids) != 2 || pids[0] != 4242 || pids[1] != 99 {
		t.Fatalf("pids = %v, want [4242 99] (an unparsable pid names no pid)", pids)
	}
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
