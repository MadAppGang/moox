package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// testGuardTmuxBin is the tmux binary every in-process test resolves to: a
// path that cannot exist, so no unit test can reach a tmux server.
const testGuardTmuxBin = "/nonexistent/tt-test-guard/tmux"

// TestMain stubs out everything that would otherwise touch the developer's real
// machine for the whole test binary. newModel() calls listSessions() AND
// loadSnapshot() during construction; unstubbed, every test would shell out to
// the real tmux server and read the real resurrect snapshot — slow, and
// order-dependent on whatever happened to be running.
//
// restoreSnapshot is stubbed to a hard failure rather than a no-op on purpose:
// nothing in this file should be reaching it, and a test that does needs to say
// so by substituting its own fake.
func TestMain(m *testing.M) {
	listSessions = func() []session { return nil }
	loadSnapshot = func() snapshot { return snapshot{} }
	restoreSnapshot = func() error { return stubErr("restoreSnapshot called without a stub") }
	// The panel's policy is resolved from the real ~/.claude/settings.json in
	// production; a test must not depend on what that file says today.
	panelPolicy = observer.DefaultPolicy
	// Nor may it read or write the developer's saved grouping.
	loadPanelMode = func() (panelMode, bool) { return defaultPanelMode, false }
	savePanelMode = func(panelMode) error { return nil }
	// Nor may the developer's own panel.conf change what a row looks like:
	// "" is "the built-in format only".
	panelConfPath = func() string { return "" }
	// Nor may the server the test runner happens to sit in decide anything
	// (moox §4.5): every in-process tmux exec resolves to the -L default
	// socket path — so per-socket paths are the legacy ones — through a binary
	// that does not exist, so an unstubbed exec fails instead of reaching a
	// real server.
	resolveTarget = func() (tmuxtarget.Target, error) {
		sock, err := tmuxtarget.LabelPath(tmuxtarget.MapEnv{}, "default")
		return tmuxtarget.Target{Bin: testGuardTmuxBin, Socket: sock}, err
	}
	os.Exit(m.Run())
}

// testSnapshot is a stand-in for a real resurrect save file, big enough to look
// like a workspace worth restoring.
func testSnapshot() snapshot {
	return snapshot{
		path:     "/tmp/tmux_resurrect_test.txt",
		sessions: []string{"magus", "mnemex", "authflow"},
		windows:  9,
		savedAt:  time.Unix(1_780_000_000, 0),
	}
}

// newRestorableModel builds the state the feature exists for: no sessions, and a
// snapshot on disk. The seams it swaps are package globals, so it registers a
// cleanup to put them back — otherwise the stub leaks into whichever test runs
// next and the suite only passes in one particular order.
func newRestorableModel(t *testing.T) model {
	t.Helper()
	stubSeam(t, &loadSnapshot, testSnapshot)
	m := newModel()
	m, _ = applyMsg(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	return m
}

// stubSeam swaps one of the package-level seams for the duration of a test and
// restores it afterwards. Generic so the same helper covers listSessions,
// loadSnapshot, restoreSnapshot and the mutators, which have four different
// signatures between them.
func stubSeam[T any](t *testing.T, seam *T, stub T) {
	t.Helper()
	original := *seam
	t.Cleanup(func() { *seam = original })
	*seam = stub
}

// newTestModel builds a model with a known set of sessions, bypassing tmux so
// the tests are deterministic and don't touch the real server.
func newTestModel(sessions ...session) model {
	m := newModel()
	m.list.SetItems(toItems(sessions))
	// Give it a size so the list and forms render.
	m, _ = applyMsg(m, tea.WindowSizeMsg{Width: 100, Height: 30})
	return m
}

func applyMsg(m model, msg tea.Msg) (model, tea.Cmd) {
	updated, cmd := m.Update(msg)
	return updated.(model), cmd
}

// keyMsg builds the tea.KeyMsg a terminal would send for s. The ctrl-chords
// matter here: they must arrive as their own key Type (not as runes), because
// that is exactly what makes them distinguishable from the plain letters that
// now feed the search box.
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "ctrl+t":
		return tea.KeyMsg{Type: tea.KeyCtrlT}
	case "ctrl+r":
		return tea.KeyMsg{Type: tea.KeyCtrlR}
	case "ctrl+x":
		return tea.KeyMsg{Type: tea.KeyCtrlX}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "/":
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
}

// runCmd executes a tea.Cmd but gives up if it doesn't return promptly. The
// filter command we care about (filterItems -> FilterMatchesMsg) returns
// immediately, but the focused filter input also schedules a cursor-blink
// command that blocks ~530ms and then reschedules itself forever. We must not
// follow that loop, so any cmd that doesn't complete within a short window is
// treated as "no message" and dropped.
func runCmd(cmd tea.Cmd) (tea.Msg, bool) {
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case msg := <-done:
		return msg, true
	case <-time.After(50 * time.Millisecond):
		return nil, false
	}
}

// pump drains the command tree returned by an Update and feeds every resulting
// message back through the model. The bubbles list does its filter matching
// asynchronously (a filterItems cmd returns a FilterMatchesMsg), so a test that
// merely presses keys without running those commands never populates the
// filtered items — and the list then refuses to apply an "empty" filter. pump
// runs those commands so the filter actually takes hold, mirroring what the
// real tea runtime does, while skipping the never-ending cursor-blink timer.
func pump(t *testing.T, m model, cmd tea.Cmd) model {
	t.Helper()
	m, _ = pumpTracked(t, m, cmd)
	return m
}

// pumpTracked is pump plus the one fact a test sometimes needs out of the
// command tree it drained: whether the program asked to exit. tea.Quit is just
// a command that returns a tea.QuitMsg, so "did it quit" is only observable by
// running the commands — it leaves no trace on the model. This matters for the
// flows where the quit is issued several Updates deep (a huh form completing),
// so the caller can't simply check the cmd it got back from the keystroke.
func pumpTracked(t *testing.T, m model, cmd tea.Cmd) (model, bool) {
	t.Helper()
	quit := false
	// Guard against runaway loops if a cmd ever schedules itself forever.
	for i := 0; cmd != nil && i < 50; i++ {
		msg, ok := runCmd(cmd)
		if !ok {
			break // blocking timer (cursor blink) — stop following it
		}
		switch msg := msg.(type) {
		case tea.BatchMsg:
			cmd = nil
			for _, c := range msg {
				if c == nil {
					continue
				}
				var q bool
				m, q = pumpTracked(t, m, c)
				quit = quit || q
			}
		case tea.QuitMsg:
			quit = true
			cmd = nil
		case nil:
			cmd = nil
		default:
			m, cmd = applyMsg(m, msg)
		}
	}
	return m, quit
}

// typeSearch searches the way a user now does: no "/" prefix and no enter to
// accept — just the characters. It pumps the resulting filter commands so the
// match set is actually populated (bubbles filters asynchronously via a
// FilterMatchesMsg), leaving the model in Filtering state with matches visible.
//
// Note there is no "accept" step to mirror: enter attaches, so Filtering is the
// state a searching user stays in right up until they pick something.
func typeSearch(t *testing.T, m model, query string) model {
	t.Helper()
	for _, r := range query {
		m2, cmd := applyMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = pump(t, m2, cmd)
	}
	return m
}

// TestAtoiOr pins the numeric-field parsing used by queryTmuxSessions. The
// load-bearing case is the empty string: tmux leaves last_attached empty for
// never-attached sessions, and that must parse to 0 (not error the whole row).
// Verified against live tmux output where restored sessions show an empty
// last_attached column.
func TestAtoiOr(t *testing.T) {
	cases := []struct {
		in       string
		fallback int
		want     int
	}{
		{"", 0, 0},                    // empty last_attached -> 0
		{"1780477336", 0, 1780477336}, // a real unix timestamp
		{"2", 0, 2},                   // window count
		{"  5  ", 0, 5},               // surrounding whitespace tolerated
		{"notanumber", 0, 0},          // garbage -> fallback
	}
	for _, c := range cases {
		if got := atoiOr(c.in, c.fallback); got != c.want {
			t.Errorf("atoiOr(%q, %d) = %d, want %d", c.in, c.fallback, got, c.want)
		}
	}
}

// TestSortSessions verifies the MRU ordering: attached/recent sessions first by
// lastAttached desc, never-attached ones (lastAttached 0) after them ordered by
// created desc, and name as the final deterministic tiebreak. names() pulls the
// resulting order so the expectations read as a simple slice.
func TestSortSessions(t *testing.T) {
	names := func(ss []session) []string {
		out := make([]string, len(ss))
		for i, s := range ss {
			out[i] = s.name
		}
		return out
	}
	eq := func(t *testing.T, got, want []string) {
		t.Helper()
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}

	t.Run("recently attached before less recent", func(t *testing.T) {
		got := sortSessions([]session{
			{name: "old", lastAttached: 100},
			{name: "new", lastAttached: 300},
			{name: "mid", lastAttached: 200},
		})
		eq(t, names(got), []string{"new", "mid", "old"})
	})

	t.Run("never-attached sort after attached, by created desc", func(t *testing.T) {
		// "live" was attached; the rest never were (lastAttached 0) and must
		// fall below it, ordered newest-created first.
		got := sortSessions([]session{
			{name: "restored-old", lastAttached: 0, created: 100},
			{name: "live", lastAttached: 500, created: 50},
			{name: "restored-new", lastAttached: 0, created: 300},
		})
		eq(t, names(got), []string{"live", "restored-new", "restored-old"})
	})

	t.Run("name tiebreak is deterministic", func(t *testing.T) {
		// Identical timestamps: name ascending breaks the tie predictably,
		// regardless of input order.
		got := sortSessions([]session{
			{name: "charlie", lastAttached: 0, created: 0},
			{name: "alpha", lastAttached: 0, created: 0},
			{name: "bravo", lastAttached: 0, created: 0},
		})
		eq(t, names(got), []string{"alpha", "bravo", "charlie"})
	})
}

// driveNewForm opens the new-session form and types name into it, then presses
// enter to complete it — the exact key sequence a user performs. It pumps the
// resulting commands so huh's internal value actually populates (a bare
// applyForm() can't read GetString before the form runs). Returns the model
// after applyForm has run.
func driveNewForm(t *testing.T, m model, name string) model {
	t.Helper()
	m, cmd := applyMsg(m, keyMsg(keyNew))
	m = pump(t, m, cmd)
	for _, r := range name {
		m, cmd = applyMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = pump(t, m, cmd)
	}
	m, cmd = applyMsg(m, keyMsg("enter"))
	m = pump(t, m, cmd)
	return m
}

// TestValidateNameRejectsDuplicate unit-tests the pure collision helper that
// applyForm gates on: an existing name is rejected, a free name passes, and the
// excluding argument (the rename target) is skipped so renaming around a session
// doesn't self-collide.
func TestValidateNameRejectsDuplicate(t *testing.T) {
	m := newTestModel(
		session{name: "alpha", windows: 1},
		session{name: "beta", windows: 1},
	)

	if got := m.validateName("alpha", ""); got == "" {
		t.Fatal("validateName should reject an existing name")
	}
	if got := m.validateName("gamma", ""); got != "" {
		t.Fatalf("validateName should accept a free name, got %q", got)
	}
	// Renaming "alpha" -> "alpha" (excluding itself) must be allowed; renaming
	// "alpha" -> "beta" must still collide.
	if got := m.validateName("alpha", "alpha"); got != "" {
		t.Fatalf("validateName should ignore the excluded name, got %q", got)
	}
	if got := m.validateName("beta", "alpha"); got == "" {
		t.Fatal("validateName should still reject a different existing name when excluding the rename target")
	}
}

// TestApplyFormRejectsDuplicateName drives the create form to completion with a
// name that collides with an existing session. The tmux mutator must NOT be
// called and the status field must explain the rejection.
func TestApplyFormRejectsDuplicateName(t *testing.T) {
	restore := newSession
	called := false
	newSession = func(string) error { called = true; return nil }
	defer func() { newSession = restore }()

	m := newTestModel(session{name: "dup", windows: 1})
	m = driveNewForm(t, m, "dup")

	if called {
		t.Fatal("duplicate name must NOT invoke newSession")
	}
	if m.status == "" {
		t.Fatal("duplicate name should set a status message")
	}
	if !strings.Contains(m.status, "dup") {
		t.Fatalf("status should name the offending session, got %q", m.status)
	}
}

// TestApplyFormCreatesValidName is the control for the rejection test: a free
// name DOES invoke newSession and leaves no error status.
func TestApplyFormCreatesValidName(t *testing.T) {
	restore := newSession
	var gotName string
	newSession = func(name string) error { gotName = name; return nil }
	defer func() { newSession = restore }()

	m := newTestModel(session{name: "existing", windows: 1})
	m = driveNewForm(t, m, "fresh")

	if gotName != "fresh" {
		t.Fatalf("valid name should invoke newSession with the name, got %q", gotName)
	}
	if m.status != "" {
		t.Fatalf("a successful create should leave no status, got %q", m.status)
	}
	if m.selected != "fresh" {
		t.Fatalf("a successful create should queue the new session for attach, got %q", m.selected)
	}
}

// TestApplyFormSurfacesMutatorError verifies that when the tmux mutator fails,
// its error is captured into status instead of being discarded.
func TestApplyFormSurfacesMutatorError(t *testing.T) {
	restore := newSession
	newSession = func(string) error { return errStub }
	defer func() { newSession = restore }()

	m := newTestModel()
	m = driveNewForm(t, m, "boom")

	if m.status == "" {
		t.Fatal("a failed mutation should set a status message")
	}
	if !strings.Contains(m.status, errStub.Error()) {
		t.Fatalf("status should include the tmux error %q, got %q", errStub.Error(), m.status)
	}
}

// errStub is a sentinel error used to assert a mutator failure surfaces.
var errStub = stubErr("tmux blew up")

type stubErr string

func (e stubErr) Error() string { return string(e) }

// TestStatusRendersAndClears verifies the status footer is shown in the list
// View when set, and that it's cleared by the next key entering the list path.
func TestStatusRendersAndClears(t *testing.T) {
	m := newTestModel(session{name: "main", windows: 1})
	m.status = "session main already exists"

	if !strings.Contains(m.View(), "session main already exists") {
		t.Fatalf("View should render the status footer, got:\n%s", m.View())
	}

	// A navigation key enters updateList, which clears the status at the top.
	m, _ = applyMsg(m, keyMsg("down"))
	if m.status != "" {
		t.Fatalf("status should clear on the next key, got %q", m.status)
	}
	if strings.Contains(m.View(), "session main already exists") {
		t.Fatalf("View should no longer render the cleared status, got:\n%s", m.View())
	}
}

func TestEmptyStateRenders(t *testing.T) {
	m := newTestModel()
	view := m.View()
	if !strings.Contains(view, "no tmux sessions") {
		t.Fatalf("empty state should mention 'no tmux sessions', got:\n%s", view)
	}
}

func TestSessionItemDescription(t *testing.T) {
	cases := []struct {
		windows int
		want    string
	}{
		{1, "1 window"},
		{3, "3 windows"},
	}
	for _, c := range cases {
		got := session{name: "x", windows: c.windows}.Description()
		if got != c.want {
			t.Errorf("windows=%d: got %q want %q", c.windows, got, c.want)
		}
	}
}

func TestFilterValueIsName(t *testing.T) {
	if got := (session{name: "magus"}).FilterValue(); got != "magus" {
		t.Errorf("FilterValue = %q, want magus", got)
	}
}

// TestEnterSelectsForAttach verifies that pressing enter on a highlighted item
// records it for attach and requests quit.
func TestEnterSelectsForAttach(t *testing.T) {
	m := newTestModel(session{name: "main", windows: 2})
	m, cmd := applyMsg(m, keyMsg("enter"))
	if m.selected != "main" {
		t.Fatalf("selected = %q, want main", m.selected)
	}
	if cmd == nil {
		t.Fatal("enter should return a quit command")
	}
}

// TestQuitKeys verifies quitting without selecting anything — and, just as
// importantly, that "q" is NOT one of the quit keys any more. Since every plain
// letter feeds the search box, a stray "q" must search for "q" rather than
// tearing down the program mid-type.
func TestQuitKeys(t *testing.T) {
	for _, k := range []string{"esc", "ctrl+c"} {
		t.Run(k, func(t *testing.T) {
			m := newTestModel(session{name: "main", windows: 1})
			m, cmd := applyMsg(m, keyMsg(k))
			if m.selected != "" {
				t.Fatalf("%s should not select a session, got %q", k, m.selected)
			}
			if cmd == nil {
				t.Fatalf("%s should return a quit command", k)
			}
		})
	}

	t.Run("q searches instead of quitting", func(t *testing.T) {
		m := newTestModel(session{name: "main", windows: 1})
		m, cmd := applyMsg(m, keyMsg("q"))
		if cmd != nil {
			if msg, ok := runCmd(cmd); ok {
				if _, quit := msg.(tea.QuitMsg); quit {
					t.Fatal("q must not quit — it belongs to the search box")
				}
			}
		}
		if m.list.FilterState() != list.Filtering {
			t.Fatalf("q should open the search, state=%v", m.list.FilterState())
		}
		if got := m.list.FilterValue(); got != "q" {
			t.Fatalf("q should be typed into the search box, got %q", got)
		}
	})
}

// TestActionKeysOpenForms verifies the ctrl-chord verbs switch into the
// corresponding modal mode and build a form (when an item is highlighted).
func TestActionKeysOpenForms(t *testing.T) {
	t.Run("new", func(t *testing.T) {
		m := newTestModel()
		m, _ = applyMsg(m, keyMsg(keyNew))
		if m.mode != modeNew || m.form == nil {
			t.Fatalf("%s should enter modeNew with a form, mode=%v form=%v", keyNew, m.mode, m.form)
		}
	})
	t.Run("rename", func(t *testing.T) {
		m := newTestModel(session{name: "main", windows: 1})
		m, _ = applyMsg(m, keyMsg(keyRename))
		if m.mode != modeRename || m.renameTarget != "main" {
			t.Fatalf("%s should enter modeRename targeting main, mode=%v target=%q", keyRename, m.mode, m.renameTarget)
		}
	})
	t.Run("kill", func(t *testing.T) {
		m := newTestModel(session{name: "main", windows: 1})
		m, _ = applyMsg(m, keyMsg(keyKill))
		if m.mode != modeConfirmDelete || m.deleteTarget != "main" {
			t.Fatalf("%s should enter modeConfirmDelete targeting main, mode=%v target=%q", keyKill, m.mode, m.deleteTarget)
		}
	})
}

// TestLettersTypeWhileSearching is the other half of the split: with the search
// box open, plain letters must reach the input and must never fire a verb.
// The old scheme had to suppress d/n/r while filtering; the new scheme has no
// such conflict to resolve, and this pins that.
func TestLettersTypeWhileSearching(t *testing.T) {
	m := newTestModel(session{name: "main", windows: 1})
	m = typeSearch(t, m, "dnrq")

	if m.mode != modeList {
		t.Fatalf("letters must not open a form while searching; mode=%v", m.mode)
	}
	if m.selected != "" {
		t.Fatalf("letters must not select a session while searching; selected=%q", m.selected)
	}
	if got := m.list.FilterValue(); got != "dnrq" {
		t.Fatalf("every letter should land in the search box, got %q", got)
	}
}

// TestVerbsWorkWhileSearching pins the inverse, and it is the property that
// makes the fzf flow usable: the ctrl-chords act on the highlighted match
// WITHOUT first having to leave the search box.
func TestVerbsWorkWhileSearching(t *testing.T) {
	t.Run("kill", func(t *testing.T) {
		m := newTestModel(session{name: "alpha", windows: 1}, session{name: "beta", windows: 1})
		m = typeSearch(t, m, "bet")
		m, _ = applyMsg(m, keyMsg(keyKill))
		if m.mode != modeConfirmDelete || m.deleteTarget != "beta" {
			t.Fatalf("%s while searching should target the match; mode=%v target=%q", keyKill, m.mode, m.deleteTarget)
		}
	})
	t.Run("rename", func(t *testing.T) {
		m := newTestModel(session{name: "alpha", windows: 1}, session{name: "beta", windows: 1})
		m = typeSearch(t, m, "bet")
		m, _ = applyMsg(m, keyMsg(keyRename))
		if m.mode != modeRename || m.renameTarget != "beta" {
			t.Fatalf("%s while searching should target the match; mode=%v target=%q", keyRename, m.mode, m.renameTarget)
		}
	})
}

// TestTypeToSearchOpensTheBox is the headline behavior: a bare letter starts a
// search, and — the part that is easy to get wrong — the keystroke that OPENED
// the box is itself typed into it rather than being swallowed.
func TestTypeToSearchOpensTheBox(t *testing.T) {
	m := newTestModel(
		session{name: "alpha", windows: 1},
		session{name: "beta", windows: 2},
		session{name: "gamma", windows: 3},
	)

	if m.list.FilterState() != list.Unfiltered {
		t.Fatalf("precondition: list should start unfiltered, got %v", m.list.FilterState())
	}

	m = typeSearch(t, m, "bet")

	if m.list.FilterState() != list.Filtering {
		t.Fatalf("typing should open the search box, state=%v", m.list.FilterState())
	}
	if got := m.list.FilterValue(); got != "bet" {
		t.Fatalf("the opening keystroke must not be swallowed, got %q want %q", got, "bet")
	}
	if got := len(m.list.VisibleItems()); got != 1 {
		t.Fatalf("search should narrow to 1 match, got %d", got)
	}
}

// TestSlashStillOpensSearch keeps the documented "/" habit working: it opens an
// EMPTY search rather than typing a literal slash.
func TestSlashStillOpensSearch(t *testing.T) {
	m := newTestModel(session{name: "main", windows: 1})
	m, cmd := applyMsg(m, keyMsg("/"))
	m = pump(t, m, cmd)

	if m.list.FilterState() != list.Filtering {
		t.Fatalf("/ should open the search box, state=%v", m.list.FilterState())
	}
	if got := m.list.FilterValue(); got != "" {
		t.Fatalf("/ should open an empty search, got %q", got)
	}
}

// TestEnterAttachesWhileSearching pins the fzf shortcut: enter goes straight
// into the highlighted match instead of merely "accepting" the search, which is
// what the bubbles list would do on its own and would cost a second enter.
func TestEnterAttachesWhileSearching(t *testing.T) {
	m := newTestModel(
		session{name: "alpha", windows: 1},
		session{name: "beta", windows: 2},
	)
	m = typeSearch(t, m, "bet")

	m, cmd := applyMsg(m, keyMsg("enter"))
	if m.selected != "beta" {
		t.Fatalf("enter while searching should select the match, got %q", m.selected)
	}
	if cmd == nil {
		t.Fatal("enter while searching should quit so main can attach")
	}
}

// TestArrowsMoveCursorWhileSearching covers the keybinding the list disables for
// us: with the search input focused, bubbles turns off its own cursor keys, so
// without our handling the highlight would be pinned to the first match.
func TestArrowsMoveCursorWhileSearching(t *testing.T) {
	m := newTestModel(
		session{name: "svc-api", windows: 1},
		session{name: "svc-web", windows: 2},
	)
	m = typeSearch(t, m, "svc")

	if got := len(m.list.VisibleItems()); got != 2 {
		t.Fatalf("precondition: search should match both sessions, got %d", got)
	}

	m, _ = applyMsg(m, keyMsg("down"))
	m, _ = applyMsg(m, keyMsg("enter"))
	if m.selected != "svc-web" {
		t.Fatalf("down then enter should attach to the 2nd match, got %q", m.selected)
	}
}

// TestNewFormSeedsFromSearchText covers the "search for something that does not
// exist yet, then create it" path: the text already typed carries into the
// prompt instead of being retyped.
func TestNewFormSeedsFromSearchText(t *testing.T) {
	restore := newSession
	var gotName string
	newSession = func(name string) error { gotName = name; return nil }
	defer func() { newSession = restore }()

	m := newTestModel(session{name: "alpha", windows: 1})
	m = typeSearch(t, m, "brand-new")

	if got := m.searchText(); got != "brand-new" {
		t.Fatalf("searchText() = %q, want brand-new", got)
	}

	m, cmd := applyMsg(m, keyMsg(keyNew))
	m = pump(t, m, cmd)
	if m.mode != modeNew {
		t.Fatalf("%s should open the new-session form, mode=%v", keyNew, m.mode)
	}

	// Accept the prompt without typing a thing. That it creates "brand-new" is
	// the seeding, observed where it actually matters — asserting on the form's
	// internal value would pass even if nothing reached tmux.
	m, cmd = applyMsg(m, keyMsg("enter"))
	m = pump(t, m, cmd)

	if gotName != "brand-new" {
		t.Fatalf("accepting the seeded prompt should create the searched-for name, got %q", gotName)
	}
	if m.selected != "brand-new" {
		t.Fatalf("the seeded create should also queue the attach, got %q", m.selected)
	}
}

// TestCreateAttachesImmediately pins the third behavior: naming a session is
// asking to be in it, so a successful create hands the name to main's attach
// path and quits instead of dropping back to the list.
func TestCreateAttachesImmediately(t *testing.T) {
	restore := newSession
	newSession = func(string) error { return nil }
	defer func() { newSession = restore }()

	m := newTestModel(session{name: "existing", windows: 1})
	m, cmd := applyMsg(m, keyMsg(keyNew))
	m = pump(t, m, cmd)
	for _, r := range "fresh" {
		m, cmd = applyMsg(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = pump(t, m, cmd)
	}

	// The enter that completes the form is the one that must also quit.
	m, cmd = applyMsg(m, keyMsg("enter"))
	m, quit := pumpTracked(t, m, cmd)

	if m.selected != "fresh" {
		t.Fatalf("a successful create should select the new session, got %q", m.selected)
	}
	if !quit {
		t.Fatal("a successful create should quit so main can attach")
	}
	if m.mode != modeList || m.form != nil {
		t.Fatalf("the form should be torn down before quitting, mode=%v form=%v", m.mode, m.form)
	}
}

// TestFailedCreateDoesNotAttach is the control: only a create that actually
// succeeded may quit into an attach. A failure must stay in the TUI and show
// the error, otherwise main would attach to a session that does not exist.
func TestFailedCreateDoesNotAttach(t *testing.T) {
	restore := newSession
	newSession = func(string) error { return errStub }
	defer func() { newSession = restore }()

	m := newTestModel()
	m = driveNewForm(t, m, "boom")

	if m.selected != "" {
		t.Fatalf("a failed create must not select anything, got %q", m.selected)
	}
	if m.mode != modeList {
		t.Fatalf("a failed create should return to the list, mode=%v", m.mode)
	}
}

// TestEscWithAppliedFilterDoesNotQuit verifies the two-stage esc behavior:
// when a filter is applied (not actively typing), esc clears the filter via the
// list instead of quitting the program.
func TestEscWithAppliedFilterDoesNotQuit(t *testing.T) {
	m := newTestModel(session{name: "main", windows: 1})
	m.list.SetFilterState(list.FilterApplied)

	m, cmd := applyMsg(m, keyMsg("esc"))
	if m.selected != "" {
		t.Fatalf("esc with applied filter should not select/quit; selected=%q", m.selected)
	}
	// The list should have handled esc (clearing the filter), not returned Quit.
	if cmd != nil {
		t.Fatalf("esc with applied filter should not issue tea.Quit")
	}
}

// TestEscWithoutFilterQuits verifies esc quits when no filter is active.
func TestEscWithoutFilterQuits(t *testing.T) {
	m := newTestModel(session{name: "main", windows: 1})
	m, cmd := applyMsg(m, keyMsg("esc"))
	if cmd == nil {
		t.Fatal("esc without a filter should quit")
	}
}

// TestFormAbortReturnsToList verifies esc in a form drops back to the list
// without leaving stale state.
func TestFormAbortReturnsToList(t *testing.T) {
	m := newTestModel(session{name: "main", windows: 1})
	m, _ = applyMsg(m, keyMsg(keyKill))
	if m.mode != modeConfirmDelete {
		t.Fatalf("setup: expected modeConfirmDelete, got %v", m.mode)
	}
	// esc aborts the huh form.
	m, _ = applyMsg(m, keyMsg("esc"))
	if m.mode != modeList {
		t.Fatalf("esc should return to list, mode=%v", m.mode)
	}
	if m.form != nil || m.deleteTarget != "" {
		t.Fatalf("aborting should clear form/target, form=%v target=%q", m.form, m.deleteTarget)
	}
}

// TestRefreshClearsStaleFilter reproduces the "No items" bug: after applying a
// filter and then deleting a session, refresh() calls SetItems which re-applies
// the OLD filter against the NEW items, collapsing VisibleItems() to zero even
// though Items() is non-empty. The user is left staring at an empty list.
//
// This drives the real Update() loop: WindowSizeMsg sizing, then typing a
// session name to search, then refresh(). With the fix (ResetFilter in refresh)
// the list comes back unfiltered and every session is visible again.
func TestRefreshClearsStaleFilter(t *testing.T) {
	m := newTestModel(
		session{name: "alpha", windows: 1},
		session{name: "beta", windows: 2},
		session{name: "gamma", windows: 3},
	)

	// Search down to exactly one session, the way a user would.
	m = typeSearch(t, m, "alpha")

	// Precondition: the filter must really be active and matching, otherwise
	// the test would false-pass (an inactive filter never reproduces the bug).
	if m.list.FilterState() != list.Filtering {
		t.Fatalf("precondition: filter should be Filtering, got %v", m.list.FilterState())
	}
	if got := len(m.list.VisibleItems()); got != 1 {
		t.Fatalf("precondition: filtered list should show exactly 1 item, got %d", got)
	}

	// Make refresh()'s tmux query deterministic: "alpha" was deleted, the rest
	// remain. This is the post-delete state the user lands in.
	restore := listSessions
	listSessions = func() []session {
		return []session{
			{name: "beta", windows: 2},
			{name: "gamma", windows: 3},
		}
	}
	defer func() { listSessions = restore }()

	// This is the exact code path that runs after ctrl+x -> confirm: refresh()
	// re-queries tmux and SetItems re-applies the (now stale) "alpha" filter.
	m = m.refresh()

	if len(m.list.Items()) == 0 {
		t.Fatal("setup: expected remaining items after refresh")
	}
	if got := len(m.list.VisibleItems()); got == 0 {
		t.Fatalf("stale filter bug: VisibleItems()=0 while Items()=%d; refresh() must clear the filter", len(m.list.Items()))
	}
}
