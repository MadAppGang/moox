package blackbox_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// TEST-29, REQ-35: exact §8.1 spellings, invalid enums and missing values.
// The valid import fixture prevents unrelated source-validation errors from
// satisfying these usage checks.
func TestCLI_RequiredArgumentsAndInvalidEnumsReturnUsageExit2WithoutWrites(t *testing.T) {
	for _, kind := range []string{
		"unpin_missing_file_or_all", "import_missing_from", "import_missing_from_value",
		"import_out_conflicts_with_publish", "invalid_setup_mode", "missing_setup_mode",
		"invalid_wait_duration", "missing_save_reason", "missing_restore_snapshot",
		"invalid_doctor_fix", "unknown_snapshot_verb",
		// legacy_publish_missing_replace_live removed (ruling 2026-10-25): under the
		// required relocated TMUX_TMPDIR, -L default is not the legacy key, so
		// --replace-live is not required; see TestPaths_RelocatedDefaultIsNotLegacyKey….
	} {
		t.Run(kind, func(t *testing.T) {
			e := newSandbox(t, "MOOX_BIN", "MOOX_SHARE")
			fixture := authorResurrectFixture(e)
			args := map[string][]string{
				"unpin_missing_file_or_all":         {"snapshot", "unpin"},
				"import_missing_from":               {"import", "--out", filepath.Join(e.base, "out.json")},
				"import_missing_from_value":         {"import", "--from"},
				"import_out_conflicts_with_publish": {"import", "--from", fixture.file, "--out", filepath.Join(e.base, "out.json"), "--publish", "-L", "moox-bb-" + e.nonce + "-unused", "--yes"},
				"invalid_setup_mode":                {"setup", "--mode", "merge", "--yes"},
				"missing_setup_mode":                {"setup", "--mode"},
				"invalid_wait_duration":             {"save", "--wait", "not-a-duration", "--direct"},
				"missing_save_reason":               {"save", "--reason"},
				"missing_restore_snapshot":          {"restore", "--snapshot"},
				"invalid_doctor_fix":                {"doctor", "--fix", "not-a-fix"},
				"unknown_snapshot_verb":             {"snapshot", "not-a-verb"},
			}[kind]
			env := offlineTmux(e) // no real default contact, including the legacy-key refusal
			before := image(t, e.base)
			requireCode(t, e.run(e.moox, env, "", args...), 2)
			equalImage(t, image(t, e.base), before, "usage-error environment")
		})
	}
}

// TEST-30, REQ-1: both public version spellings must expose identical metadata.
func TestCLI_VersionVerbMatchesVersionFlagAndDoesNotWriteFiles(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN")
	before := image(t, e.base)
	flag, verb := e.mooxRun("--version"), e.mooxRun("version")
	requireCode(t, flag, 0)
	requireCode(t, verb, 0)
	if verb.out != flag.out || !strings.HasPrefix(verb.out, "moox ") || verb.err != "" {
		t.Errorf("version verb=%+v, want identical flag=%+v", verb, flag)
	}
	equalImage(t, image(t, e.base), before, "version alias")
}

// TEST-31, REQ-6/REQ-7/REQ-9/REQ-13: force BOTH public choices instead of
// accepting either branch. No interactive attach is needed to test setup.
func TestSetup_ExplicitReplaceOrSeparatePreservesOriginalAndIsIdempotent(t *testing.T) {
	for _, mode := range []string{"replace", "separate"} {
		t.Run(mode, func(t *testing.T) {
			e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
			originals := seedPersonalFiles(e, true)
			args := []string{"setup", "--yes", "--mode", mode, "--no-claude-hooks", "--accessible"}
			requireCode(t, e.mooxRun(args...), 0)
			originalPath := filepath.Join(e.home, ".tmux.conf")
			got := string(readFile(t, originalPath))
			if mode == "separate" {
				if got != originalTmux {
					t.Errorf("separate mode changed personal tmux config: %q", got)
				}
				// The choice is exercised via the exact --mode spelling. Interactive
				// start/attach to the literal -L moox name is not attempted: the sandbox
				// permits only nonce-owned names and supplies no PTY attachment protocol.
				// No setup.json field names or exact prose are invented.
			} else {
				if got == originalTmux || !strings.Contains(got, "moox.conf") {
					t.Errorf("replace mode did not replace config with moox: %q", got)
				}
				// Exact shared backup root from §4.2; a copy elsewhere is not sufficient.
				found := false
				for _, row := range image(t, filepath.Join(e.config, "moox", "backups")) {
					if row.Mode.IsRegular() && row.Data == originalTmux {
						found = true
					}
				}
				if !found {
					t.Error("replace mode lacks byte-identical original under XC/moox/backups/<ts>/")
				}
			}
			manifest := filepath.Join(e.config, "moox", "setup.json")
			requireMode(t, manifest, 0600)
			if !bytes.Equal(readFile(t, filepath.Join(e.home, ".claude", "settings.json")), []byte(originals[filepath.Join(e.home, ".claude", "settings.json")])) {
				t.Error("--no-claude-hooks changed Claude settings")
			}
			first, copies := managedConfig(e), contentCopies(e, originalTmux)
			requireCode(t, e.mooxRun(args...), 0)
			equalImage(t, managedConfig(e), first, "explicit-mode setup idempotency")
			equalImage(t, contentCopies(e, originalTmux), copies, "explicit-mode backups")
			requireCode(t, e.mooxRun("uninstall", "--yes"), 0)
			assertPersonalBytes(e, originals)
		})
	}
}

// TEST-32, REQ-9: the newly supplied uninstall dry-run must not restore/remove
// anything yet; the subsequent explicit --yes still restores exact originals.
func TestSetup_UninstallDryRunLeavesInstalledAndOriginalFilesUnchanged(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	originals := seedPersonalFiles(e, true)
	requireCode(t, e.mooxRun("setup", "--yes", "--mode", "replace", "--no-claude-hooks"), 0)
	before := image(t, e.base)
	requireCode(t, e.mooxRun("uninstall", "--dry-run"), 0)
	equalImage(t, image(t, e.base), before, "uninstall dry-run")
	requireCode(t, e.mooxRun("uninstall", "--yes"), 0)
	assertPersonalBytes(e, originals)
}
