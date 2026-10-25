// Package importer turns a tmux-resurrect snapshot (TSV, as tmux-resurrect
// wrote it and tmux-claude-continuity enriched it) into a moox snapshot v1
// (architecture §6.6, D12). It reads only: the resurrect file, its
// pane_contents.tar.gz, and (with AttachLive) the launch recordings of the
// matching live panes. Nothing is extracted to disk and nothing it reads is
// ever modified: `tmux_resurrect_*` and resurrect's `last` belong to
// resurrect (§12.4 observable behaviours).
//
// Every shape research/resurrect-behaviours.md §1 found on disk is handled
// by a named rule (normal, realigned, collapsed, continuation, tail tags,
// `\ ` escape, two layout encodings, a 2–3 field state row), each with a
// fixture-driven test.
package importer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MadAppGang/moox/internal/engine/snapshot"
)

var (
	// ErrNotResurrect: the input is not a resurrect TSV (a moox JSON starts
	// with '{'). `moox import` exits 4 (§12.4).
	ErrNotResurrect = errors.New("importer: not a tmux-resurrect snapshot")
	// ErrNoResurrect: a directory holds no resurrect snapshot that imports.
	// It wraps ErrNotResurrect (exit 4 either way).
	ErrNoResurrect = fmt.Errorf("%w: no importable snapshot in the directory", ErrNotResurrect)
)

// Runner runs one tmux exec against the LIVE server (AttachLive only).
// tmuxtarget.Target is the real one.
type Runner interface {
	Run(ctx context.Context, args ...string) ([]byte, error)
}

// Source says what to import.
type Source struct {
	// From is a resurrect FILE, or a DIR whose `last` is resolved (PR-C16).
	From string
	// Contents is the pane-contents archive; default
	// <dir of the resolved file>/pane_contents.tar.gz. A missing default is
	// not a warning (contents are optional); a missing explicit one is.
	Contents string
	// TrustContents attaches Contents without the pairing checks (stale,
	// newer layout, unlinked directory), with a CONTENTS-TRUSTED warning:
	// the caller vouches that the archive belongs to this snapshot.
	TrustContents bool
	// Recordings is the launch-recordings directory read with AttachLive.
	Recordings string
	// AttachLive: for rows with no ;CLAUDE_CMD= recording, read launch/<N>
	// of the live pane at the same S:W.P (read-only list-panes).
	AttachLive bool
	// Home expands a leading "~" in a path and is the "$HOME" of the B40
	// title heal. "" disables both.
	Home string
	// Producer is the snapshot's producer, e.g. "moox 0.1.0".
	Producer string
}

// Report is what the import found (§6.6). Every count is of the RESOLVED
// file; Warnings name the row key and a reason, and may carry session names
// and paths (the V4 evidence file prints only their kinds).
type Report struct {
	// SourceFile is the resurrect file actually imported (a dir's `last`
	// resolved, or the H3 fallback), and SourceMtime its mtime: --publish
	// refuses to supersede a newer `source: "save"` snapshot (PR-H23).
	SourceFile  string
	SourceMtime time.Time
	// Rows are accepted record lines (pane, window, state, grouped_session).
	Rows                                int
	Panes, Windows, Sessions, Sids      int
	Continuations, Realigned, Collapsed int
	HealedTitles                        int
	// Dropped: pane rows naming no window, and windows left with no pane.
	Dropped int
	// Recordings: launch lines attached from the live server (AttachLive).
	Recordings   int
	Warnings     []string
	ContentsUsed bool
	// ContentsPanes: panes whose contents were attached.
	ContentsPanes int
}

// Result is one import: the snapshot, its contents entries keyed by each
// pane's saved_id (empty when no archive was used; the writer names and
// hashes the file), and the report.
type Result struct {
	Snapshot *snapshot.Snapshot
	Contents map[string][]byte
	Report   Report
}

func (r *Report) warnf(format string, a ...any) {
	r.Warnings = append(r.Warnings, fmt.Sprintf(format, a...))
}

// Import reads src and builds the snapshot. live is used only with
// src.AttachLive (nil otherwise). The snapshot is Validated; an invalid one
// is returned as an error wrapping snapshot.ErrInvalid, never as a result.
func Import(ctx context.Context, src Source, live Runner) (*Result, error) {
	if src.From == "" {
		return nil, errors.New("importer: no input (--from)")
	}
	if src.AttachLive && live == nil {
		return nil, errors.New("importer: --attach-live needs a live server")
	}
	st, err := os.Stat(src.From)
	if err != nil {
		return nil, fmt.Errorf("importer: %w", err)
	}
	if !st.IsDir() {
		return importFile(ctx, src, src.From, live)
	}
	return importDir(ctx, src, live)
}

func importFile(ctx context.Context, src Source, file string, live Runner) (*Result, error) {
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("importer: %w", err)
	}
	st, err := os.Stat(file)
	if err != nil {
		return nil, fmt.Errorf("importer: %w", err)
	}
	res, err := build(src, data)
	if err != nil {
		return nil, err
	}
	res.Report.SourceFile, res.Report.SourceMtime = file, st.ModTime()
	// created_at is when resurrect captured the workspace: the file's mtime.
	res.Snapshot.CreatedAt = st.ModTime().Format(time.RFC3339)
	attachContents(src, filepath.Dir(file), st.ModTime(), data, res)
	if src.AttachLive {
		if err := attachLive(ctx, src, live, res); err != nil {
			return nil, err
		}
	}
	if err := snapshot.Validate(res.Snapshot); err != nil {
		return nil, err
	}
	return res, nil
}

// importDir resolves resurrect's `last` (a relative symlink or a regular
// file, which continuity sometimes leaves). If it dangles, is missing, names
// a moox file, or does not import, the newest `tmux_resurrect_*.txt` that
// imports is taken instead (H3). `moox-*` names are never considered, so the
// §11 rollback always reaches the resurrect snapshot.
func importDir(ctx context.Context, src Source, live Runner) (*Result, error) {
	dir := src.From
	var notes []string
	last := filepath.Join(dir, "last")
	target := ""
	if lst, err := os.Lstat(last); err == nil {
		switch {
		case lst.Mode()&fs.ModeSymlink != 0:
			ln, err := os.Readlink(last)
			if err == nil {
				if !filepath.IsAbs(ln) {
					ln = filepath.Join(dir, ln)
				}
				target = ln
			}
		case lst.Mode().IsRegular():
			target = last
		}
	}
	if target != "" && strings.HasPrefix(filepath.Base(target), "moox-") {
		notes = append(notes, "LAST-NAMES-MOOX: resurrect's last names a moox file; ignored")
		target = ""
	}
	if target != "" {
		if _, err := os.Stat(target); err != nil {
			notes = append(notes, "LAST-DANGLING: resurrect's last does not resolve; newest valid snapshot taken (H3)")
			target = ""
		}
	}
	if target != "" {
		res, err := importFile(ctx, src, target, live)
		if err == nil {
			res.Report.Warnings = append(notes, res.Report.Warnings...)
			return res, nil
		}
		notes = append(notes, fmt.Sprintf("LAST-INVALID: %v; newest valid snapshot taken (H3)", err))
	} else if len(notes) == 0 {
		notes = append(notes, "LAST-MISSING: no resurrect last; newest valid snapshot taken (H3)")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("importer: %w", err)
	}
	var cands []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, "tmux_resurrect_") && strings.HasSuffix(n, ".txt") && e.Type().IsRegular() {
			cands = append(cands, n)
		}
	}
	// The name carries the local timestamp (YYYYMMDDTHHMMSS), so the
	// lexical order is the time order.
	sort.Sort(sort.Reverse(sort.StringSlice(cands)))
	for _, n := range cands {
		p := filepath.Join(dir, n)
		if target != "" && p == target {
			continue
		}
		res, err := importFile(ctx, src, p, live)
		if err == nil {
			res.Report.Warnings = append(notes, res.Report.Warnings...)
			return res, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNoResurrect, dir)
}
