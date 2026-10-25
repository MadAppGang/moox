package main

import (
	"bytes"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// The usage text is printed exactly once, on exactly one stream.
//
// Before this fix every subcommand printed it twice: each FlagSet's Usage
// wrote it to stderr, flag.Parse calls Usage on both -h/--help and a bad flag,
// and the call site then printed it AGAIN — to stdout for --help, to stderr
// for a flag error. `tt sessions --help` put the full usage on both streams.
//
// The one rule: --help is a request, so the usage goes to stdout and stderr
// stays empty; a bad flag is an error, so the flag's complaint and the usage go
// to stderr and stdout stays empty. The usage header is counted, not merely
// looked for, because "contains" is exactly the assertion the bug passed.
//
// Every argv here makes the command return before it binds, dials or watches
// anything: runObserve and runMCP only get that far once the flags parse.
// ---------------------------------------------------------------------------

// assertUsageOnce checks one subcommand's --help and bad-flag paths.
func assertUsageOnce(t *testing.T, name, usageText string) {
	t.Helper()
	header := "usage: tt " + name + " [flags]"
	if !strings.HasPrefix(usageText, header) {
		t.Fatalf("the %s usage text does not start with %q; the test is counting the wrong line", name, header)
	}

	for _, help := range []string{"--help", "-help", "-h"} {
		t.Run(help, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code, handled := runSubcommand(name, []string{help}, &stdout, &stderr)
			if !handled {
				t.Fatalf("%s is not a known command", name)
			}
			if code != 0 {
				t.Fatalf("%s exited %d, want 0; stderr:\n%s", help, code, stderr.String())
			}
			if got := stdout.String(); got != usageText {
				t.Fatalf("%s: stdout is not the usage text exactly once (%d headers):\n%s",
					help, strings.Count(got, header), got)
			}
			if stderr.Len() != 0 {
				t.Fatalf("%s wrote to stderr (%d usage headers):\n%s",
					help, strings.Count(stderr.String(), header), stderr.String())
			}
		})
	}

	t.Run("an unknown flag", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code, handled := runSubcommand(name, []string{"--no-such-flag"}, &stdout, &stderr)
		if !handled {
			t.Fatalf("%s is not a known command", name)
		}
		if code != 2 {
			t.Fatalf("exit code %d, want 2 (a usage error); stderr:\n%s", code, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Fatalf("a usage error wrote to stdout: %q", stdout.String())
		}
		got := stderr.String()
		if n := strings.Count(got, header); n != 1 {
			t.Fatalf("stderr carries the usage header %d times, want 1:\n%s", n, got)
		}
		if n := strings.Count(got, usageText); n != 1 {
			t.Fatalf("stderr carries the full usage text %d times, want 1:\n%s", n, got)
		}
		// The flag's own complaint still comes first: silencing Usage must not
		// silence the line that says what was wrong.
		if !strings.HasPrefix(got, "flag provided but not defined: -no-such-flag\n") {
			t.Fatalf("stderr does not open with the flag error:\n%s", got)
		}
	})
}

func TestSessionsUsageIsPrintedExactlyOnce(t *testing.T) {
	assertUsageOnce(t, "sessions", sessionsUsage)
}

func TestObserveUsageIsPrintedExactlyOnce(t *testing.T) {
	assertUsageOnce(t, "observe", observeUsage)
}

func TestMCPUsageIsPrintedExactlyOnce(t *testing.T) {
	assertUsageOnce(t, "mcp", mcpUsage)
}
