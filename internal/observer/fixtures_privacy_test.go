package observer_test

// §9.1's privacy obligation, as a test rather than as a note in a README.
//
// Every jobs/ and projects/ fixture derived from a real record has `intent`,
// `output`, `suggestedReply`, timeline `text` and all transcript
// `content`/`text`/`lastPrompt`/`message` fields REDACTED — and redacted means
// the KEY IS GONE, not that the value was replaced. A replaced value is a
// judgement call about what counts as sensitive, made once, by whoever captured
// the fixture; an absent key is checkable.
//
// The fixtures are committed to a dotfiles repository that is pushed to a
// remote, so this is not hypothetical tidiness. The check runs over the WHOLE
// testdata tree rather than a list, so a fixture added by a later phase is
// covered without anyone remembering to add it here.

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// privacyDenylist is the set of JSON key names that carry user prompts,
// assistant messages or tool output. None may appear anywhere under testdata/.
//
// Each entry names where it comes from, because a denylist without provenance
// is a list somebody will shorten:
//
//	intent          the user's own instruction to the background agent
//	output          the agent's assistant messages (§11 assumption 8)
//	suggestedReply  a draft reply written FOR the user
//	lastPrompt      the last thing typed, on the `last-prompt` transcript record
//	message         the transcript's conversation payload
//	content         the text inside a message
//	text            timeline detail text, and transcript content blocks
var privacyDenylist = []string{
	`"intent"`,
	`"output"`,
	`"suggestedReply"`,
	`"lastPrompt"`,
	`"message"`,
	`"content"`,
	`"text"`,
}

func TestNoFixtureCarriesAPrivacyDenylistedFieldName(t *testing.T) {
	checked := checkFixtureTree(t, "testdata")
	if checked < 40 {
		t.Fatalf("only %d fixtures were checked; the walk is not seeing the tree", checked)
	}
	t.Logf("%d fixtures checked against %d denylisted keys", checked, len(privacyDenylist))
}

// The panel's golden frames live in the module's own testdata
// (moox/cmd/tt/testdata, #17). This walk checks only what it can: that no JSON
// fixture there, and no golden, carries a denylisted KEY — it catches a pasted
// `tt sessions --json` record. A golden is a rendered frame with no keys in
// it, so session names or a `needs` text rendered from a real snapshot would
// pass here; that they come from the synthetic fleet is asserted in the main
// package, by TestEveryPanelGoldenIsRenderedFromTheSyntheticFleet
// (panel_golden_test.go).
func TestNoPanelFixtureCarriesAPrivacyDenylistedFieldName(t *testing.T) {
	root := filepath.Join("..", "..", "cmd", "tt", "testdata")
	checked := checkFixtureTree(t, root)
	if checked == 0 {
		t.Fatalf("no fixture under %s was checked; the walk is not seeing the tree", root)
	}
	t.Logf("%d panel fixtures checked against %d denylisted keys", checked, len(privacyDenylist))
}

// checkFixtureTree walks one testdata tree and reports every fixture that
// carries a denylisted key. It returns how many files it read.
func checkFixtureTree(t *testing.T, root string) int {
	t.Helper()
	var checked int
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch filepath.Ext(p) {
		case ".json", ".jsonl", ".golden":
			// .golden: a rendered panel frame. Only a pasted JSON record
			// would carry a key; its origin is checked in the main package.
		default:
			// README.md, the .key fixture, .keep markers. The .key file is
			// deliberately not JSON and is asserted elsewhere to be never
			// opened; it is not a record and has no field names.
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		checked++
		src := string(data)
		for _, key := range privacyDenylist {
			if strings.Contains(src, key) {
				t.Errorf("%s carries the denylisted key %s. Redacted means the KEY IS GONE, "+
					"not that the value was replaced — these fixtures are committed to a "+
					"repository with a remote", p, key)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return checked
}

func TestTheDenylistCanActuallyFail(t *testing.T) {
	// A scanner that finds nothing is indistinguishable from a scanner that
	// looks at nothing — the same reasoning the static audits in
	// internal/observer/audit are built on. This proves the check has teeth
	// without writing a violating file into testdata/.
	const violating = `{"sessionId":"a","intent":"do the thing I asked"}`
	var hits int
	for _, key := range privacyDenylist {
		if strings.Contains(violating, key) {
			hits++
		}
	}
	if hits == 0 {
		t.Error("the denylist does not match a record that plainly violates it")
	}
}

func TestEveryFixtureDirectoryHasAREADMEMarkingRealVersusSynthetic(t *testing.T) {
	// "We tested the permission-prompt path" and "we tested our GUESS at the
	// permission-prompt path" are different claims, and only the README
	// distinguishes them. A fixture set without one invites the second claim to
	// be read as the first.
	for _, dir := range []string{"testdata/registry", "testdata/jobs", "testdata/projects"} {
		readme := filepath.Join(dir, "README.md")
		data, err := os.ReadFile(readme)
		if err != nil {
			t.Errorf("%s: %v", readme, err)
			continue
		}
		src := string(data)
		for _, marker := range []string{"REAL", "SYNTHETIC"} {
			if !strings.Contains(src, marker) {
				t.Errorf("%s does not use the %s marker", readme, marker)
			}
		}
	}
}

func TestTheJobsFixtureTreeHasTheShapeTheRealTreeHas(t *testing.T) {
	// The fixtures are only evidence if they reproduce the shape that made the
	// real tree hard: a root FILE that is not a job, directories with no
	// record, and a state.json buried deep enough that a walk finds it.
	entries, err := os.ReadDir("testdata/jobs")
	if err != nil {
		t.Fatalf("reading testdata/jobs: %v", err)
	}
	var files, dirs int
	for _, e := range entries {
		if e.IsDir() {
			dirs++
		} else {
			files++
		}
	}
	if files == 0 {
		t.Error("no root-level FILE in the jobs fixture tree; pins.json is what M3 was about")
	}
	if dirs < 10 {
		t.Errorf("%d job directories; the real tree has 14", dirs)
	}

	var recordless, deep int
	err = filepath.WalkDir("testdata/jobs", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != "state.json" {
			return err
		}
		rel, _ := filepath.Rel("testdata/jobs", p)
		if strings.Count(filepath.ToSlash(rel), "/") > 1 {
			deep++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking: %v", err)
	}
	if deep == 0 {
		t.Error("no state.json below depth 1; M13's adversarial decoy is missing and the " +
			"depth-1 rule is untested")
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := fs.Stat(os.DirFS("testdata/jobs"), e.Name()+"/state.json"); err != nil {
			recordless++
		}
	}
	if recordless == 0 {
		t.Error("every fixture directory carries a record; 7 of the 14 real ones do not")
	}
}
