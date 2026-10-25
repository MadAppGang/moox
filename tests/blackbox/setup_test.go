package blackbox_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const originalTmux = "# black-box original: preserve CRLF and no final newline\r\nset -g history-limit 4173\r\n# nonce; #(not-a-command)"
const localConfig = "# user-owned local.conf must never be overwritten\nset -g @blackbox-local 4189\n"

func seedPersonalFiles(e *sandbox, existing bool) map[string]string {
	files := map[string]string{
		filepath.Join(e.home, ".zshrc"):                   "# original zshrc\nexport BLACKBOX_ZSH=191\n",
		filepath.Join(e.home, ".bashrc"):                  "# original bashrc\nexport BLACKBOX_BASH=193\n",
		filepath.Join(e.config, "moox", "local.conf"):     localConfig,
		filepath.Join(e.home, ".claude", "settings.json"): "{\"env\":{\"BLACKBOX_MARKER\":\"197\"}}\n",
	}
	if existing {
		files[filepath.Join(e.home, ".tmux.conf")] = originalTmux
	}
	for path, content := range files {
		writeFile(e.t, path, content, 0600)
	}
	return files
}

func assertPersonalBytes(e *sandbox, files map[string]string) {
	e.t.Helper()
	for path, want := range files {
		if got := readFile(e.t, path); !bytes.Equal(got, []byte(want)) {
			e.t.Errorf("%s not restored byte-identically\ngot %q\nwant %q", path, got, want)
		}
	}
}

// Only stable, user-facing configuration payloads are compared for idempotency;
// daemon logs and undocumented manifest timestamps are not an oracle.
func managedConfig(e *sandbox) map[string]fileImage {
	result := map[string]fileImage{}
	for _, root := range []string{e.home, e.config} {
		for path, row := range image(e.t, root) {
			base := filepath.Base(path)
			if base == ".tmux.conf" || base == "tmux.conf" || base == ".zshrc" ||
				base == ".bashrc" || base == "local.conf" || base == "settings.json" {
				result[filepath.Join(root, path)] = row
			}
		}
	}
	return result
}

func contentCopies(e *sandbox, text string) map[string]fileImage {
	found := map[string]fileImage{}
	for _, root := range []string{e.home, e.config, e.state, e.data} {
		for rel, row := range image(e.t, root) {
			if row.Mode.IsRegular() && row.Data == text {
				found[filepath.Join(root, rel)] = row
			}
		}
	}
	return found
}

func requireUserTmuxConfig(e *sandbox) {
	e.t.Helper()
	// Both stock tmux config locations are public, user-facing paths. The excerpt
	// does not specify which one setup chooses, so do not require one spelling.
	for _, path := range []string{filepath.Join(e.home, ".tmux.conf"), filepath.Join(e.config, "tmux", "tmux.conf")} {
		content, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(content), "moox.conf") {
			return
		}
	}
	e.t.Fatal("setup with no existing config did not install a user tmux config referencing moox.conf")
}

// TEST-7, REQ-5. Includes both clean-HOME and existing-config cases.
func TestSetup_DryRunListsActionsAndLeavesAllIsolatedFilesUnchanged(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "no_existing_config"
		if existing {
			name = "existing_config"
		}
		t.Run(name, func(t *testing.T) {
			e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
			seedPersonalFiles(e, existing)
			before := image(t, e.base)
			r := e.mooxRun("setup", "--dry-run")
			requireCode(t, r, 0)
			if strings.TrimSpace(r.out+r.err) == "" {
				t.Error("dry-run must list proposed actions, not print nothing")
			}
			equalImage(t, image(t, e.base), before, "setup --dry-run")
		})
	}
}

// TEST-8, REQ-6/REQ-7/REQ-27: no-existing-config installation.
// TEST-31 separately forces both --mode replace/separate branches from §8.1.
func TestSetup_YesInstallsOnFakeHomeAndSecondRunKeepsConfigBytes(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	seedPersonalFiles(e, false)
	requireCode(t, e.mooxRun("setup", "--yes"), 0)
	requireUserTmuxConfig(e)
	if got := string(readFile(t, filepath.Join(e.config, "moox", "local.conf"))); got != localConfig {
		t.Fatalf("setup overwrote user local.conf: %q", got)
	}
	first := managedConfig(e)
	requireCode(t, e.mooxRun("setup", "--yes"), 0)
	if second := managedConfig(e); !reflect.DeepEqual(first, second) {
		t.Errorf("second setup changed managed configuration\nfirst=%#v\nsecond=%#v", first, second)
	}
}

// TEST-9, REQ-6/REQ-7/REQ-9. Accept either permitted existing-config decision,
// but original bytes must remain recoverable and a rerun must not multiply
// copies or alter the selected configuration.
func TestSetup_ExistingConfigStaysRecoverableAndUninstallRestoresEveryByte(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	originals := seedPersonalFiles(e, true)
	requireCode(t, e.mooxRun("setup", "--yes"), 0)
	copies := contentCopies(e, originalTmux)
	if len(copies) == 0 {
		t.Fatal("existing tmux config was neither kept nor backed up byte-identically")
	}
	first := managedConfig(e)
	requireCode(t, e.mooxRun("setup", "--yes"), 0)
	equalImage(t, managedConfig(e), first, "existing-config setup idempotency")
	equalImage(t, contentCopies(e, originalTmux), copies, "original backup idempotency")
	// F4: local.conf is "never overwritten". Setup may add its own setting
	// (separate mode needs @moox-sockets to match `moox`), but the user's bytes
	// must survive unchanged, and uninstall (below) must restore every byte.
	if got := string(readFile(t, filepath.Join(e.config, "moox", "local.conf"))); !strings.HasPrefix(got, localConfig) {
		t.Fatalf("setup overwrote the user's local.conf content: %q", got)
	}
	r := e.mooxRun("uninstall", "--yes")
	requireCode(t, r, 0)
	assertPersonalBytes(e, originals)
}

// TEST-10, REQ-9: removing a new install restores prior *absence* as well as
// prior shell/Claude/local configuration bytes.
func TestSetup_UninstallRemovesNewConfigAndRestoresOriginalShellFiles(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "TT_BIN", "MOOX_SHARE")
	originals := seedPersonalFiles(e, false)
	requireCode(t, e.mooxRun("setup", "--yes"), 0)
	requireUserTmuxConfig(e)
	requireCode(t, e.mooxRun("uninstall", "--yes"), 0)
	assertPersonalBytes(e, originals)
	for _, path := range []string{filepath.Join(e.home, ".tmux.conf"), filepath.Join(e.config, "tmux", "tmux.conf")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("newly installed tmux config was not removed: %s (%v)", path, err)
		}
	}
}
