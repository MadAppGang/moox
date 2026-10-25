// Package jsonout is the S1 driving adapter: it turns one snapshot into one
// answer on stdout and nothing else.
//
// It is a DRIVING adapter, so it depends on the incoming port and never on a
// driven one. It opens no file, starts no process, arms no watch and reads no
// clock — the snapshot it is handed already carries the one instant every
// derived number in it was computed against, and this package passes that same
// instant to the encoder rather than reading a second one. A second clock read
// here would silently break §9.2(c)'s identity
// (`statusAgeMs == observedAt - statusSince`) by a few hundred microseconds,
// which is exactly the drift that identity test exists to catch.
//
// Everything that becomes JSON becomes JSON through wire.NewSnapshot. There is
// no encoder here, and there is deliberately no struct with a json tag: §5.1's
// AST audit bans them outside `wire`, which is what makes AC11's four-surface
// agreement structural instead of conventional. The human table is rendered
// from the same wire.Snapshot for the same reason — a surface that cannot see a
// field cannot show one the JSON does not have.
package jsonout

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/MadAppGang/moox/internal/observer"
	"github.com/MadAppGang/moox/internal/observer/wire"
)

// Exit codes, per §5.2. They are named because the CLI's contract is as much
// the exit code as the payload: a monitoring script reads the code first.
//
// ExitOK is returned for an empty session set as well as a full one — "no
// Claude is running" is a fact, not an error, and a surface that exited
// non-zero for it would make every `if tt sessions --json` in a script wrong.
const (
	ExitOK     = 0
	ExitNoRead = 1
	ExitUsage  = 2
)

// Source is the narrow incoming port this surface actually uses.
//
// It is NOT observer.SessionSource. §5.1 says every driving adapter depends on
// the incoming port and nothing else, and it will: Refresher below is the
// second half, and P4 (Refresh) through P6 (Enrich/Subscribe) complete the set.
// Until then *observer.Observer does not implement all four methods, so an
// adapter that demanded the whole interface could not be wired at all — and the
// alternative, stubbing three methods on the core so a type assertion passes,
// would mean shipping three methods that lie. A consumer-defined narrow
// interface is the idiomatic Go answer and it widens without a caller change.
type Source interface {
	Snapshot(ctx context.Context, opts observer.SnapshotOptions) (*observer.Snapshot, error)
}

// Refresher is the optional half. A Source that can re-read its corroboration
// source implements it; one that cannot is asked and answers by not
// implementing it, rather than by returning an error nobody can act on.
//
// P4 makes *observer.Observer satisfy this, at which point --refresh starts
// working with no change in this file or in cli.go.
type Refresher interface {
	Refresh(ctx context.Context, sources []string) (*observer.Snapshot, observer.RefreshOutcome, error)
}

// SourceRefresher is the refresh-only half, and it is preferred over Refresher
// wherever a source offers it.
//
// The reason is a defect P4 found in the Refresher path rather than a
// preference: the incoming port's Refresh carries NO SnapshotOptions, so the
// snapshot it returns is built with the defaults — and this surface would then
// print that snapshot, silently discarding --waiting, --kind, --cwd-prefix and
// --include-dead for any invocation that also passed --refresh. Refreshing the
// SOURCE and then asking for the snapshot separately honours the flags in ONE
// rebuild, which is also cheaper than the two the alternative would take.
//
// Refresher stays as the fallback because a via-socket client (P8) implements
// the port and nothing more.
type SourceRefresher interface {
	RefreshSources(ctx context.Context, sources []string) observer.RefreshOutcome
}

// Enricher is the second optional half: a Source that can run §7.3's 200 KiB
// transcript pass. It is separate from Source for the same reason Refresher is
// — a `sockclient` implements it and a bare fixture core does not, and a
// surface that demanded it could not be wired over either.
//
// Asked for by o.Snapshot.IncludeTranscripts, which S1 defaults to TRUE
// (§9.2a). The pass is the only expensive thing tt does, so a source that
// cannot enrich answers by not implementing this rather than by returning an
// error nobody can act on: the status answer is complete without a colour.
type Enricher interface {
	Enrich(ctx context.Context, snap *observer.Snapshot, ids []string) (*observer.Snapshot, error)
}

// Options is what the surface needs that the snapshot does not carry.
type Options struct {
	// JSON selects the machine surface. False renders the human table, which
	// §5.2 states explicitly is NOT a contract.
	JSON bool
	// TT is tt's own version string, resolved once by the composition root from
	// observer.ResolveVersion. Never resolved here: §5.1 gives the version one
	// source and an encoder reaching for it would be a second.
	TT string
	// Refresh asks for corroboration to be re-read before the answer is built.
	Refresh bool
	// Snapshot is what is asked of the core. Build it from
	// observer.DefaultSnapshotOptions and override, never from a literal — a
	// SnapshotOptions{} literal has IncludeUnknownLiveness false, which hides
	// every live: null session and is the one thing M7 forbids.
	Snapshot observer.SnapshotOptions
}

// Run obtains one snapshot, writes one answer, and returns the process exit
// code. It never calls os.Exit itself, so it is testable as a function.
//
// One-shot by construction: no watch is armed, nothing is retained, and the
// call returns as soon as the bytes are written.
func Run(ctx context.Context, src Source, o Options, stdout, stderr io.Writer) int {
	snap, err := obtain(ctx, src, o, stderr)
	// NoTranscriptReads wins over IncludeTranscripts: the CLI refuses the two
	// together, and a caller that set both anyway asked for no transcript read.
	if err == nil && o.Snapshot.IncludeTranscripts && !o.Snapshot.NoTranscriptReads {
		snap, err = enrich(ctx, src, snap, stderr)
	}
	if err != nil {
		// Exit 1 is reserved for "nothing at all could be read" (§5.2). A
		// source that answered with degraded contents does not come through
		// here — it comes through as a snapshot whose sources[].ok is false,
		// which is an answer and therefore a success.
		fmt.Fprintf(stderr, "tt: %v\n", err)
		return ExitNoRead
	}

	out := wire.NewSnapshot(snap, snap.ObservedAt, wire.Options{TT: o.TT})

	if o.JSON {
		// Compact, exactly as §5.1 writes it. Indentation would be a private
		// formatting choice on a surface whose bytes three later surfaces have
		// to match.
		enc := json.NewEncoder(stdout)
		if err := enc.Encode(out); err != nil {
			fmt.Fprintf(stderr, "tt: writing json: %v\n", err)
			return ExitNoRead
		}
		return ExitOK
	}

	if err := writeTable(stdout, out); err != nil {
		fmt.Fprintf(stderr, "tt: writing output: %v\n", err)
		return ExitNoRead
	}
	return ExitOK
}

// obtain gets the snapshot, optionally refreshing corroboration first.
//
// A rejected refresh is reported on stderr and the answer is still produced:
// §7.2's RefreshOutcome exists precisely because a rate limit, a timeout and a
// backoff are successful degraded answers, and turning one into a non-zero exit
// would make a corroboration hiccup look like "tt could not read anything".
func obtain(ctx context.Context, src Source, o Options, stderr io.Writer) (*observer.Snapshot, error) {
	if !o.Refresh {
		return src.Snapshot(ctx, o.Snapshot)
	}

	if rs, ok := src.(SourceRefresher); ok {
		// Refresh the source, report what that achieved, then ask the question
		// the flags actually describe.
		reportRejections(stderr, rs.RefreshSources(ctx, []string{observer.SourceAgentsCLI}))
		return src.Snapshot(ctx, o.Snapshot)
	}

	r, ok := src.(Refresher)
	if !ok {
		fmt.Fprintln(stderr, "tt: --refresh: this build has no corroboration source; answering from the authoritative sources only")
		return src.Snapshot(ctx, o.Snapshot)
	}

	snap, outcome, err := r.Refresh(ctx, nil)
	if err != nil {
		return nil, err
	}
	reportRejections(stderr, outcome)
	if snap != nil {
		return snap, nil
	}
	return src.Snapshot(ctx, o.Snapshot)
}

// enrich runs the transcript enrichment pass over the sessions this surface is
// about to print — all of them, since the CLI prints everything it was not
// asked to filter out.
//
// ids is nil rather than the printed session list because the two are the same
// set here and a second enumeration could only disagree with the first.
//
// A source that cannot enrich is not an error: the sessions keep
// transcriptCollected false, which is the documented "we did not look" value,
// and every status field is already complete. An enrichment that FAILS is not
// an error either — Enrich degrades per session and reports what happened in
// sources.transcripts — so the only thing handled here is a nil answer, which
// would mean a bug in the source rather than a fact about the machine.
func enrich(ctx context.Context, src Source, snap *observer.Snapshot, stderr io.Writer) (*observer.Snapshot, error) {
	e, ok := src.(Enricher)
	if !ok {
		return snap, nil
	}
	out, err := e.Enrich(ctx, snap, nil)
	if err != nil {
		// Reported, not fatal, and the unenriched snapshot is still printed:
		// "the colour pass failed" must not turn into "tt could not read
		// anything", which is what exit 1 means (§5.2).
		fmt.Fprintf(stderr, "tt: transcript enrichment: %v\n", err)
		return snap, nil
	}
	if out == nil {
		return snap, nil
	}
	return out, nil
}

// reportRejections writes the request-scoped half of a refresh outcome to
// stderr. It is stderr and not the payload deliberately: §7.2 says these
// advisories are request-scoped and must never be merged into the shared
// snapshot, because one caller's rejected refresh is not a fact about the
// observer that unrelated subscribers should see.
func reportRejections(stderr io.Writer, outcome observer.RefreshOutcome) {
	ids := make([]string, 0, len(outcome.Rejected))
	for id := range outcome.Rejected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintf(stderr, "tt: --refresh: %s: %s\n", id, outcome.Rejected[id])
	}
	for _, a := range outcome.Advisories {
		fmt.Fprintf(stderr, "tt: --refresh: %s\n", a.Code)
	}
}

// writeTable renders the human surface: a compact table plus one summary line.
//
// §5.2: explicitly not a contract, and no test asserts its layout beyond
// "contains the session names". It is here rather than in package main so that
// it renders from the same wire.Snapshot the JSON does.
func writeTable(w io.Writer, s wire.Snapshot) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	fmt.Fprintln(tw, "NAME\tKIND\tSTATUS\tAGE\tLIVE\tPID\tCWD")
	for _, sess := range s.Sessions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			dash(sess.Name),
			sess.Kind,
			statusCell(sess),
			ageCell(sess.StatusAgeMs),
			triState(sess.Live),
			intCell(sess.PID),
			dash(sess.CWD),
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	_, err := fmt.Fprintf(w,
		"\n%d sessions  (live %d, dead %d, unknown %d, attention %d)\n",
		s.Counts.Total, s.Counts.Live, s.Counts.Dead, s.Counts.Unknown, s.Counts.Attention)
	if err != nil {
		return err
	}
	for _, a := range s.Warnings {
		if _, err := fmt.Fprintf(w, "warning: %s\n", a.Code); err != nil {
			return err
		}
	}
	return nil
}

// statusCell shows the status and, when it is `waiting`, which of the two
// reasons AC2 distinguishes. A `waiting` session whose reason is dropped is the
// one case where the compact view would mislead rather than merely omit.
func statusCell(s wire.Session) string {
	if s.WaitReason == "" {
		return s.Status
	}
	return s.Status + "/" + s.WaitReason
}

// ageCell renders a pre-computed age. A nil age prints "?" rather than 0: the
// difference between "we do not know since when" and "since just now" is the
// whole of AC3.
func ageCell(ms *int64) string {
	if ms == nil {
		return "?"
	}
	d := *ms
	switch {
	case d < 60_000:
		return strconv.FormatInt(d/1000, 10) + "s"
	case d < 3_600_000:
		return strconv.FormatInt(d/60_000, 10) + "m"
	case d < 86_400_000:
		return strconv.FormatInt(d/3_600_000, 10) + "h"
	default:
		return strconv.FormatInt(d/86_400_000, 10) + "d"
	}
}

// triState renders a *bool as three visibly different things. "?" is not "no".
func triState(b *bool) string {
	switch {
	case b == nil:
		return "?"
	case *b:
		return "yes"
	default:
		return "no"
	}
}

func intCell(p *int) string {
	if p == nil {
		return "-"
	}
	return strconv.Itoa(*p)
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}
