package main

// moox §4.5's routing additions: the global flags, and the two print-and-exit
// names that must never open the TUI.

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/adapters/jsonout"
)

func TestRouteArgs_GlobalFlagsBeforeSubcommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		argv    []string
		kind    routeKind
		cmd     string
		args    []string
		globals globalFlags
	}{
		{"before the command", []string{"--tmux-socket", "a", "sessions", "--json"},
			routeSubcommand, "sessions", []string{"--json"}, globalFlags{socket: "a"}},
		{"= form, both flags", []string{"--tmux-socket=/x/s", "--tmux-bin=/opt/tmux", "sessions"},
			routeSubcommand, "sessions", []string{}, globalFlags{socket: "/x/s", bin: "/opt/tmux"}},
		{"after the command", []string{"sessions", "--json", "--tmux-socket", "a"},
			routeSubcommand, "sessions", []string{"--json"}, globalFlags{socket: "a"}},
		{"single dash", []string{"-tmux-socket", "a", "serve", "--ensure"},
			routeSubcommand, "serve", []string{"--ensure"}, globalFlags{socket: "a"}},
		{"the TUI takes them too", []string{"--tmux-socket", "work"},
			routeTUI, "", nil, globalFlags{socket: "work"}},
		{"TUI with its own flag", []string{"--tmux-bin", "/b/tmux", "--json"},
			routeTUI, "", nil, globalFlags{bin: "/b/tmux"}},
		{"after -- they are data", []string{"sessions", "--", "--tmux-socket", "a"},
			routeSubcommand, "sessions", []string{"--", "--tmux-socket", "a"}, globalFlags{}},
		{"missing value is refused, not the TUI", []string{"--tmux-socket"},
			routeSubcommand, globalFlagError, nil, globalFlags{}},
		{"empty value is refused", []string{"--tmux-socket=", "sessions"},
			routeSubcommand, globalFlagError, nil, globalFlags{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := routeArgs(tc.argv)
			if r.kind != tc.kind || r.name != tc.cmd || r.globals != tc.globals {
				t.Fatalf("routeArgs(%q) = %+v, want kind %v name %q globals %+v", tc.argv, r, tc.kind, tc.cmd, tc.globals)
			}
			if tc.kind == routeSubcommand && tc.cmd != globalFlagError && !reflect.DeepEqual(r.args, tc.args) &&
				!(len(r.args) == 0 && len(tc.args) == 0) {
				t.Fatalf("routeArgs(%q).args = %q, want %q", tc.argv, r.args, tc.args)
			}
			if tc.kind == routeTUI && (r.name != "" || r.args != nil) {
				t.Fatalf("a TUI route captured a command: %+v", r)
			}
		})
	}

	// The refusal is a usage error with nothing on stdout.
	var stdout, stderr bytes.Buffer
	code, handled := runSubcommand(globalFlagError, nil, &stdout, &stderr)
	if !handled || code != jsonout.ExitUsage || stdout.Len() != 0 || !strings.Contains(stderr.String(), "--tmux-socket") {
		t.Fatalf("missing-value refusal: handled %v code %d stdout %q stderr %q", handled, code, stdout.String(), stderr.String())
	}

	// useGlobals is what main applies; the route comes back unchanged.
	t.Cleanup(func() { useGlobals(route{}) })
	in := routeArgs([]string{"--tmux-socket", "a", "sessions"})
	if out := useGlobals(in); !reflect.DeepEqual(out, in) || currentGlobals() != (globalFlags{socket: "a"}) {
		t.Fatalf("useGlobals changed the route or did not record the globals: %+v / %+v", out, currentGlobals())
	}
}

func TestRouteArgs_VersionNeverTUI(t *testing.T) {
	for _, argv := range [][]string{
		{"--version"}, {"-version"}, {"--tmux-socket", "a", "--version"}, {"--version", "--tmux-socket=a"},
	} {
		r := routeArgs(argv)
		if r.kind != routeSubcommand || r.name != cmdVersion {
			t.Fatalf("routeArgs(%q) = %+v, want the version command — never the TUI", argv, r)
		}
	}
	for _, argv := range [][]string{{"--help"}, {"-h"}, {"-help"}} {
		if r := routeArgs(argv); r.kind != routeSubcommand || r.name != cmdHelp {
			t.Fatalf("routeArgs(%q) = %+v, want the help command — never the TUI", argv, r)
		}
	}
	// A subcommand's own --help stays the subcommand's.
	if r := routeArgs([]string{"sessions", "--help"}); r.name != "sessions" {
		t.Fatalf("tt sessions --help routed to %+v", r)
	}

	var stdout, stderr bytes.Buffer
	code, handled := runSubcommand(cmdVersion, nil, &stdout, &stderr)
	want := "tt " + observer.ResolveVersion() + " (moox)\n"
	if !handled || code != 0 || stdout.String() != want || stderr.Len() != 0 {
		t.Fatalf("tt --version: handled %v code %d stdout %q stderr %q, want %q", handled, code, stdout.String(), stderr.String(), want)
	}
	stdout.Reset()
	if code, _ := runSubcommand(cmdHelp, nil, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "--tmux-socket") {
		t.Fatalf("tt --help: code %d, usage does not document the global flags:\n%s", code, stdout.String())
	}
}

func TestServeFlags(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		ok   bool
	}{
		{[]string{"--ensure"}, true},
		{[]string{"--ensure", "--replace"}, true},
		{[]string{"--foreground"}, true},
		{[]string{"--stop"}, true},
		{[]string{"--status", "--json"}, true},
		{nil, false},
		{[]string{"--ensure", "--stop"}, false},
		{[]string{"--replace"}, false},
		{[]string{"--stop", "--json"}, false},
		{[]string{"--status", "extra"}, false},
	} {
		var stdout, stderr bytes.Buffer
		_, code, ok := parseServeFlags(tc.argv, &stdout, &stderr)
		if ok != tc.ok || (!ok && code != jsonout.ExitUsage) {
			t.Errorf("parseServeFlags(%q) ok=%v code=%d, want ok=%v", tc.argv, ok, code, tc.ok)
		}
	}
}

func TestDeactivationUnsetsOnlyMooxActiveSlots(t *testing.T) {
	hooks := `after-new-window[100] run-shell -b 'theme'
after-new-window[101] wait-for -S moox-dirty
after-new-window[105] run-shell -b 'copy'
client-attached[102] wait-for -S moox-touch
client-attached[103] run-shell -b 'reconcile'
pane-focus-in[104] run-shell -b 'tt'
pane-focus-in[0] other
session-created[101] wait-for -S moox-dirty
`
	got := activeSlotsIn(hooks)
	want := []string{"after-new-window[101]", "client-attached[102]", "client-attached[103]", "pane-focus-in[104]", "session-created[101]"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("activeSlotsIn = %q, want %q", got, want)
	}
}

func TestReloadBindingOnlyForAConfigList(t *testing.T) {
	if reloadBinding("/home/user/.tmux.conf") != nil {
		t.Fatal("one config file needs no re-bind")
	}
	got := reloadBinding("/t/harness.conf,/s/moox.conf")
	want := []string{"bind-key", "r", "source-file", "/t/harness.conf", "/s/moox.conf", ";", "display-message", "moox: reloaded"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reloadBinding = %q, want %q", got, want)
	}
}
