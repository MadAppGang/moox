package saver

// requests.go answers the two kinds of request other processes send the
// daemon (§5.5, §6.4a). Each request is one global option named by a
// 16-hex nonce, and each answer goes to that requester's OWN option and
// channel, so a late waiter still finds its signal (a `wait-for -S` with no
// waiter blocked is kept) and no requester can consume another's (Q6).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MadAppGang/moox/internal/engine"
	"github.com/MadAppGang/moox/internal/engine/snapshot"
	"github.com/MadAppGang/moox/internal/tmuxtarget"
)

// Option-name prefixes and channels of the request protocol (§12.4).
const (
	SaveReqPrefix    = "@moox-save-req-"
	SaveResPrefix    = "@moox-save-res-"
	SavedChanPrefix  = "moox-saved-"
	RestoreReqPrefix = "@moox-restore-req-"
	RestoreResPrefix = "@moox-restore-res-"
	RestoreAckPrefix = "moox-restore-ack-"
	RestoredPrefix   = "moox-restored-"

	// FieldSep separates a request's fields; the free-text field is last.
	FieldSep = "\x1f"
	// resultTTL: result options older than this belong to dead requesters
	// and are unset by the next answer (§5.5 step 2).
	resultTTL = 300 * time.Second
)

// requestTimeout bounds each tmux exec of the request protocol.
const requestTimeout = 10 * time.Second

func (m *machine) run(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(m.ctx), requestTimeout)
	defer cancel()
	return m.tmux.Run(ctx, args...)
}

// readOptions lists every global option whose name has one of the prefixes
// (one `show-options -g`), then reads their raw values with ONE
// display-message (a #{@name} is never re-expanded, and display-message's
// strftime pass runs over the template only). An option that went away in
// between reads as "".
func (m *machine) readOptions(prefixes ...string) (map[string]string, error) {
	out, err := m.run("show-options", "-g")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		name, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) && len(name) > len(p) {
				names = append(names, name)
				break
			}
		}
	}
	vals := map[string]string{}
	if len(names) == 0 {
		return vals, nil
	}
	sort.Strings(names)
	tmpl := make([]string, 0, len(names))
	for _, n := range names {
		tmpl = append(tmpl, "#{"+n+"}")
	}
	out, err = m.run("display-message", "-p", strings.Join(tmpl, "\x1e"))
	if err != nil {
		return nil, err
	}
	f := strings.Split(strings.TrimRight(string(out), "\n"), "\x1e")
	if len(f) != len(names) {
		// A value holds the separator (or a newline): read each option on
		// its own, so one odd value cannot wedge every later request
		// (G1-M8).
		m.logf("REQ-READ %d request options returned %d fields; reading them one by one", len(names), len(f))
		for _, n := range names {
			v, err := m.run("show-options", "-gqv", n)
			if err != nil {
				return nil, err
			}
			vals[n] = strings.TrimSuffix(string(v), "\n")
		}
		return vals, nil
	}
	for i, n := range names {
		vals[n] = f[i]
	}
	return vals, nil
}

// oneLineField drops what readOptions' batch read cannot carry: newlines and
// the \x1e batch separator.
var oneLineField = strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ", "\x1e", " ")

// ---------------------------------------------------------------------------
// save requests (§5.5)
// ---------------------------------------------------------------------------

// SaveRequest is one parsed @moox-save-req-<nonce>.
type SaveRequest struct {
	Nonce  string
	Epoch  int64
	Force  bool
	Reason string
}

// ParseSaveRequest parses "<epoch>\x1f<flags>\x1f<reason>" (R3-H2: the
// reason last, so it may hold spaces and separators).
func ParseSaveRequest(nonce, v string) (SaveRequest, bool) {
	f := strings.SplitN(v, FieldSep, 3)
	if len(f) != 3 {
		return SaveRequest{}, false
	}
	ep, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return SaveRequest{}, false
	}
	return SaveRequest{Nonce: nonce, Epoch: ep, Force: strings.Contains(f[1], "f"), Reason: f[2]}, true
}

// FormatSaveRequest is the requester's value.
func FormatSaveRequest(epoch int64, force bool, reason string) string {
	flags := ""
	if force {
		flags = "f"
	}
	return strconv.FormatInt(epoch, 10) + FieldSep + flags + FieldSep + oneLineField.Replace(reason)
}

// onSaveRequest is SAVE-REQ (§5.3): every pending request is collected and
// answered by ONE save with contents (Force if any request asked for it: a
// concurrent --force is the user's newer, explicit intent). prefix C-s
// signals with no request option and gets the same save, unanswered.
func (m *machine) onSaveRequest() {
	vals, err := m.readOptions(SaveReqPrefix, SaveResPrefix)
	if err != nil {
		m.logf("SAVE-REQ read: %v", err)
		vals = map[string]string{}
	}
	var reqs []SaveRequest
	var stale []string
	now := m.clk.Now()
	for name, v := range vals {
		switch {
		case strings.HasPrefix(name, SaveReqPrefix):
			nonce := strings.TrimPrefix(name, SaveReqPrefix)
			r, ok := ParseSaveRequest(nonce, v)
			if !ok {
				m.logf("SAVE-REQ %s does not parse: %q (saved for anyway)", nonce, v)
				r = SaveRequest{Nonce: nonce, Epoch: now.Unix()}
			}
			reqs = append(reqs, r)
		case strings.HasPrefix(name, SaveResPrefix):
			ep, _, _ := strings.Cut(v, " ")
			if n, err := strconv.ParseInt(ep, 10, 64); err != nil || now.Sub(time.Unix(n, 0)) > resultTTL {
				stale = append(stale, name)
			}
		}
	}
	sort.Slice(reqs, func(i, j int) bool { return reqs[i].Nonce < reqs[j].Nonce })
	sort.Strings(stale)
	if m.restoring {
		m.logf("SAVE-REQ %d request(s) while restoring: answered restoring, nothing saved", len(reqs))
		m.answerSaves(reqs, "restoring", stale)
		return
	}
	force := false
	var reasons []string
	for _, r := range reqs {
		force = force || r.Force
		if r.Reason != "" {
			reasons = append(reasons, r.Reason)
		}
	}
	reason := "request"
	switch {
	case len(reqs) == 0:
		reason = "key"
	case len(reasons) > 0:
		reason = "request: " + strings.Join(reasons, ", ")
	}
	m.stop(&m.debounce)
	_, result := m.save(reason, true, force)
	m.answerSaves(reqs, result, stale)
}

// answerSaves is §5.5 step 2's one chained call: unset each request, set its
// result, unset results of dead requesters, then signal each requester's
// own channel once.
func (m *machine) answerSaves(reqs []SaveRequest, result string, stale []string) {
	if len(reqs) == 0 && len(stale) == 0 {
		return
	}
	epoch := strconv.FormatInt(m.clk.Now().Unix(), 10)
	var cmds [][]string
	for _, r := range reqs {
		cmds = append(cmds,
			[]string{"set-option", "-gu", SaveReqPrefix + r.Nonce},
			[]string{"set-option", "-g", SaveResPrefix + r.Nonce, epoch + " " + result})
	}
	for _, s := range stale {
		cmds = append(cmds, []string{"set-option", "-gu", s})
	}
	for _, r := range reqs {
		cmds = append(cmds, []string{"wait-for", "-S", SavedChanPrefix + r.Nonce})
	}
	if _, err := m.run(tmuxtarget.Chain(cmds...)...); err != nil {
		m.logf("SAVE-REQ answer %q to %d request(s): %v", result, len(reqs), err)
	}
}

// ---------------------------------------------------------------------------
// restore requests (§6.4a)
// ---------------------------------------------------------------------------

// RestoreRequest is one parsed @moox-restore-req-<nonce>.
type RestoreRequest struct {
	Nonce string
	Pid   int
	Epoch int64
	Stub  bool
	Path  string
}

// ParseRestoreRequest parses "<pid>\x1f<epoch>\x1f<stub 0|1>\x1f<absolute
// path>" (the path last, so it may hold spaces, R2-H4).
func ParseRestoreRequest(nonce, v string) (RestoreRequest, bool) {
	f := strings.SplitN(v, FieldSep, 4)
	if len(f) != 4 {
		return RestoreRequest{}, false
	}
	pid, err1 := strconv.Atoi(f[0])
	ep, err2 := strconv.ParseInt(f[1], 10, 64)
	if err1 != nil || err2 != nil || (f[2] != "0" && f[2] != "1") || !strings.HasPrefix(f[3], "/") {
		return RestoreRequest{}, false
	}
	return RestoreRequest{Nonce: nonce, Pid: pid, Epoch: ep, Stub: f[2] == "1", Path: f[3]}, true
}

// FormatRestoreRequest is the requester's value.
func FormatRestoreRequest(pid int, epoch int64, stub bool, path string) string {
	s := "0"
	if stub {
		s = "1"
	}
	return strconv.Itoa(pid) + FieldSep + strconv.FormatInt(epoch, 10) + FieldSep + s + FieldSep + path
}

// onRestoreRequest is RESTORE-REQ: every pending request is read and unset;
// while restoring all are refused busy-restoring; otherwise the OLDEST
// valid one starts if ManualPreconditions still hold, and the rest are
// refused busy-restoring. Each is answered on its own channel.
func (m *machine) onRestoreRequest() {
	vals, err := m.readOptions(RestoreReqPrefix)
	if err != nil {
		m.logf("RESTORE-REQ read: %v", err)
		return
	}
	var reqs []RestoreRequest
	for name, v := range vals {
		nonce := strings.TrimPrefix(name, RestoreReqPrefix)
		r, ok := ParseRestoreRequest(nonce, v)
		if !ok {
			m.logf("RESTORE-REQ %s does not parse: %q", nonce, v)
			m.answerRestore(RestoreRequest{Nonce: nonce}, "refused invalid", "")
			continue
		}
		reqs = append(reqs, r)
	}
	sort.Slice(reqs, func(i, j int) bool {
		if reqs[i].Epoch != reqs[j].Epoch {
			return reqs[i].Epoch < reqs[j].Epoch
		}
		return reqs[i].Nonce < reqs[j].Nonce
	})
	for _, r := range reqs {
		if m.restoring {
			m.answerRestore(r, "refused busy-restoring", "")
			continue
		}
		if !m.alive(r.Pid) {
			m.logf("RESTORE-REQ %s: requester pid %d is gone", r.Nonce, r.Pid)
			m.answerRestore(r, "refused invalid", "")
			continue
		}
		if err := validSnapshot(r.Path); err != nil {
			m.logf("RESTORE-REQ %s: %v", r.Nonce, err)
			m.answerRestore(r, "refused invalid", "")
			continue
		}
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		err := m.c.Engine.ManualPreconditions(ctx)
		cancel()
		if err != nil {
			m.logf("RESTORE-REQ %s refused: %v", r.Nonce, err)
			m.answerRestore(r, "refused "+preconditionWord(err), "")
			continue
		}
		value := fmt.Sprintf("%s:%d:%d", engine.KindRestore, os.Getpid(), m.clk.Now().Unix())
		m.answerRestore(r, "started", value)
		m.startRequested(r, value)
	}
}

func preconditionWord(err error) string {
	switch {
	case errors.Is(err, engine.ErrNotActive):
		return "not-active"
	case errors.Is(err, engine.ErrRestoring):
		return "busy-restoring"
	}
	return "populated: " + oneLine(err.Error())
}

// maxRequestedSnapshot bounds the requester's file this single-threaded
// loop reads (statedir's own bound for a snapshot document).
const maxRequestedSnapshot = 8 << 20

func validSnapshot(path string) error {
	// read-only: the requester's absolute path. A FIFO or device would block
	// the event loop (every save stops) and a huge file would be read whole,
	// so only a bounded regular file is opened (G1-M3).
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("saver: %s is not a regular file", path)
	}
	if fi.Size() > maxRequestedSnapshot {
		return fmt.Errorf("saver: %s is %d bytes, over the %d-byte bound", path, fi.Size(), maxRequestedSnapshot)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	b, err := io.ReadAll(io.LimitReader(f, maxRequestedSnapshot+1))
	_ = f.Close()
	if err != nil {
		return err
	}
	s, err := snapshot.Decode(b)
	if err != nil {
		return err
	}
	return snapshot.Validate(s)
}

// answerRestore is one chained call: unset the request, (for a start) set
// @moox-restoring restore:<daemonpid>:<epoch> — after ManualPreconditions
// read it unset, before anything is created — set the result and signal the
// requester's own channel.
func (m *machine) answerRestore(r RestoreRequest, result, restoring string) {
	cmds := [][]string{{"set-option", "-gu", RestoreReqPrefix + r.Nonce}}
	if restoring != "" {
		cmds = append(cmds, []string{"set-option", "-g", "@moox-restoring", restoring})
	}
	cmds = append(cmds,
		[]string{"set-option", "-g", RestoreResPrefix + r.Nonce, result},
		[]string{"wait-for", "-S", RestoreAckPrefix + r.Nonce})
	if _, err := m.run(tmuxtarget.Chain(cmds...)...); err != nil {
		m.logf("RESTORE-REQ answer %q to %s: %v", result, r.Nonce, err)
	}
}

// startRequested runs a requested restore in the restore goroutine. A
// Restore that fails before creating anything clears the @moox-restoring
// value the answer set (conditionally) and wakes the requester's
// moox-restored-<nonce> with @moox-restore-res-<nonce> "failed <reason>".
func (m *machine) startRequested(r RestoreRequest, restoring string) {
	m.beginRestore()
	go func() {
		end := restoreEnd{kind: engine.KindRestore}
		defer func() { m.restoreDone <- end }()
		res, err := m.c.Engine.Restore(m.ctx, engine.RestoreRequest{Path: r.Path, Kind: engine.KindRestore, Stub: r.Stub,
			Nonce: r.Nonce})
		if err != nil {
			m.logf("RESTORE %s from %s failed: %v", r.Nonce, r.Path, err)
			_, _ = m.run(tmuxtarget.Chain(
				engine.ClearRestoringArgs(restoring),
				[]string{"set-option", "-g", RestoreResPrefix + r.Nonce, "failed " + oneLine(err.Error())},
				[]string{"wait-for", "-S", RestoredPrefix + r.Nonce},
			)...)
			end.result = "failed"
			return
		}
		m.logf("RESTORE %s created %d of %d rows from %s; BOOT %s; waiting for the LAUNCH verdict", r.Nonce,
			len(res.Created), len(res.Plan.Rows), res.Source, res.Boot.Result)
		end.result = m.awaitLaunch(res)
	}()
}
