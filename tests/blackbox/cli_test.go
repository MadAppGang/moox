package blackbox_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TEST-1, REQ-1: metadata must be a concrete, noninteractive semver line.
func TestCLI_MooxVersionPrintsSemverWithoutStartingServer(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN")
	before := image(t, e.base)
	r := e.mooxRun("--version")
	requireCode(t, r, 0)
	semver := `(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?`
	if !regexp.MustCompile("^moox " + semver + "\n?$").MatchString(r.out) {
		t.Errorf("version stdout=%q, want exactly moox <semver>", r.out)
	}
	if r.err != "" {
		t.Errorf("version stderr=%q, want empty", r.err)
	}
	equalImage(t, image(t, e.base), before, "version-only environment")
}

// TEST-2, REQ-1.
func TestCLI_TTVersionPrintsProductSemverWithoutStartingTUI(t *testing.T) {
	e := newSandbox(t, "TT_BIN")
	before := image(t, e.base)
	r := e.run(e.tt, nil, "", "--version")
	requireCode(t, r, 0)
	if !regexp.MustCompile(`^tt (?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)? \(moox\)\n?$`).MatchString(r.out) {
		t.Errorf("version stdout=%q, want exactly tt <semver> (moox)", r.out)
	}
	if r.err != "" {
		t.Errorf("version stderr=%q, want empty", r.err)
	}
	equalImage(t, image(t, e.base), before, "tt version-only environment")
}

// TEST-3, REQ-2/REQ-26: inspect packaged public artifacts, not source contents.
func TestCLI_RootPrintsSuppliedShareWithRequiredArtifacts(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "MOOX_SHARE")
	r := e.mooxRun("root")
	requireCode(t, r, 0)
	got, err := filepath.EvalSymlinks(strings.TrimSpace(r.out))
	if err != nil {
		t.Fatalf("root output is not an existing path: %q: %v", r.out, err)
	}
	want, err := filepath.EvalSymlinks(e.share)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("root=%q, want supplied installed share %q", got, want)
	}
	for _, file := range []string{"moox.conf", "shell/moox.zsh", "shell/moox.bash", "plugins/PINS", "VERSION"} {
		info, err := os.Stat(filepath.Join(got, file))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			t.Errorf("required nonempty share file %s: %v", file, err)
		}
	}
	for _, dir := range []string{"conf", "scripts", "plugins/tmux-copy-image", "LICENSES"} {
		info, err := os.Stat(filepath.Join(got, dir))
		if err != nil || !info.IsDir() {
			t.Errorf("required share directory %s: %v", dir, err)
		}
	}
	matches, err := filepath.Glob(filepath.Join(got, "conf", "*.conf"))
	if err != nil || len(matches) == 0 {
		t.Errorf("conf/*.conf is absent: %v", err)
	}
}

// TEST-4, REQ-35.
func TestCLI_UnknownVerbReturnsUsageExit2WithoutWrites(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN")
	before := image(t, e.base)
	r := e.mooxRun("no-such-blackbox-verb")
	requireCode(t, r, 2)
	equalImage(t, image(t, e.base), before, "usage-error environment")
}

// TEST-5, REQ-3/REQ-35. An external dependency double is used only for the
// documented minimum-version/error boundary; no internal calls are asserted.
func TestCLI_SetupRejectsMissingOrOlderTmuxWithExit69(t *testing.T) {
	for _, tc := range []struct{ name, version string }{
		{"missing", ""}, {"older_than_3_6", "tmux 3.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSandbox(t, "MOOX_BIN", "MOOX_SHARE")
			binary := filepath.Join(e.base, "control", "dependency-tmux")
			if tc.version != "" {
				writeFile(t, binary, "#!/bin/sh\nprintf '%s\\n' "+shQuote(tc.version)+"\n", 0700)
			}
			before := image(t, e.home)
			r := e.run(e.moox, map[string]string{"MOOX_TMUX_BIN": binary}, "", "setup", "--dry-run")
			requireCode(t, r, 69)
			equalImage(t, image(t, e.home), before, "HOME on unsupported tmux")
		})
	}
}

// TEST-6, REQ-23/REQ-35: provide a valid --from so a missing source cannot
// accidentally satisfy the explicit --publish/socket usage precondition.
func TestCLI_ImportPublishWithoutExplicitSocketReturnsUsageExit2(t *testing.T) {
	e := newSandbox(t, "MOOX_BIN", "MOOX_SHARE")
	fixture := authorResurrectFixture(e)
	before := image(t, e.home)
	r := e.run(e.moox, map[string]string{"MOOX_TMUX_SOCKET": ""}, "", "import", "--from", fixture.file, "--publish")
	requireCode(t, r, 2)
	equalImage(t, image(t, e.home), before, "HOME after invalid import request")
}
