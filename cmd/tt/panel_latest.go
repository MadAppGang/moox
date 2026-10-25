package main

// panel_latest.go reads the LATEST ASSISTANT TEXT of a session for the lines
// drawn under its row (FR9, architecture revision 4) — "what is it doing right
// now", in the session's own words.
//
// # Why the panel reads it, and why nothing else ever sees it
//
// §5.4 says only numbers and bounded identifiers leave a transcript: the
// observer's transcriptfs adapter decodes no message content, and
// TranscriptFacts has no field that could carry it. That rule stays true.
// This reader is a narrowly scoped, documented exception that lives OUTSIDE
// the observer: the panel — the sidebar, the TUI's ^o column and
// `tt panel-render`, all in this process, on this machine — opens the
// transcript at TranscriptFacts.Path itself and draws the text straight into
// its own rows. The text is never written into observer.Session or
// TranscriptFacts, so it cannot reach the wire codec, the socket, MCP or
// `tt sessions --json`: those carry Sessions, and a Session never holds it.
// TestTheLatestTextNeverReachesTheWireOrTheJSON pins that.
//
// What it reads is bounded the same way the facts pass is:
//   - the last latestTailBytes of the file, never the whole transcript;
//   - the NEWEST main-thread assistant record that has text — a subagent's
//     sidechain record is not the session's own voice;
//   - that record's `text` content blocks only. A tool_use block (its input), a
//     thinking block, a tool result and every user record are never decoded
//     into text;
//   - sanitised (sanitizeText: no control character can reach the terminal)
//     and cut to its last latestKeepBytes, which is more than three lines of
//     the widest panel.
//
// It is cached per path by size and mtime, so a frame costs one os.Stat per
// row and a read only when the transcript moved. It arms no timer: it runs
// where the panel already renders. Any error — a missing file, a permission,
// a torn line — is "no text", never a message on the panel.

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"

	"github.com/MadAppGang/moox/internal/panelfmt"
)

const (
	// latestTailBytes is how much of a transcript's end is read. The newest
	// assistant text is almost always in the last few records; a tail that
	// holds only tool traffic draws no text rather than reading further.
	latestTailBytes = 64 << 10
	// latestKeepBytes is how much of the text is kept: its END, since the
	// panel draws the last lines. 4 KiB is three lines of a 1,000-column panel.
	latestKeepBytes = 4 << 10
	// panelTextLines is the most lines drawn under a row (FR9); panel.conf's
	// text_lines may ask for fewer.
	panelTextLines = panelfmt.MaxTextLines
	// panelTextIndent is the columns between the gutter cell and the text: the
	// text starts at column 5, under the status code's letters.
	panelTextIndent = 4
	// The transcript keys and values the reader looks at.
	latestTypeKey      = "type"
	latestSidechainKey = "isSidechain"
	latestMessageKey   = "message"
	latestContentKey   = "content"
	latestTextKey      = "text"
	latestAssistant    = "assistant"
	latestTextBlock    = "text"
)

// markdownMarks are the markdown marks that read as noise in three short
// lines of plain text: code backticks and bold stars. The words between them
// are kept.
var markdownMarks = strings.NewReplacer("`", "", "**", "")

// latestNeedle is the cheap pre-filter a line must pass before it is decoded:
// no line without it can be an assistant record.
var latestNeedle = []byte(`"` + latestAssistant + `"`)

// latestEntry is one transcript's cached text.
type latestEntry struct {
	size, mtime int64
	text        string
	// used is the sweep generation that last asked for it.
	used uint64
}

// latestCache is the panel's per-path cache of latest texts. It lives on the
// panelMemo, so it survives bubbletea's copies of the model; the mutex keeps a
// test that renders one memo from two goroutines honest.
type latestCache struct {
	mu      sync.Mutex
	gen     uint64
	entries map[string]latestEntry
	// read is the reader, a seam for the cache test to count reads.
	read func(path string) string
}

func newLatestCache() *latestCache {
	return &latestCache{entries: map[string]latestEntry{}, read: readLatestAssistantText}
}

// text is the latest assistant text of the transcript at path, or "".
func (c *latestCache) text(path string) string {
	if c == nil || path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return ""
	}
	size, mtime := info.Size(), info.ModTime().UnixNano()
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[path]; ok && e.size == size && e.mtime == mtime {
		e.used = c.gen
		c.entries[path] = e
		return e.text
	}
	text := c.read(path)
	c.entries[path] = latestEntry{size: size, mtime: mtime, text: text, used: c.gen}
	return text
}

// sweep starts a frame: entries no row asked for during the previous frame
// are dropped, so the cache holds only the transcripts on screen.
func (c *latestCache) sweep() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for path, e := range c.entries {
		if e.used < c.gen {
			delete(c.entries, path)
		}
	}
	c.gen++
}

// readLatestAssistantText is the newest main-thread assistant text in the
// last latestTailBytes of the transcript at path, sanitised and cut to its
// last latestKeepBytes; "" on any error or when the tail holds none.
func readLatestAssistantText(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return ""
	}
	off := max(info.Size()-latestTailBytes, 0)
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return ""
	}
	tail, err := io.ReadAll(io.LimitReader(f, latestTailBytes))
	if err != nil {
		return ""
	}
	lines := bytes.Split(tail, []byte{'\n'})
	if off > 0 && len(lines) > 0 {
		lines = lines[1:] // the first line was cut by the tail
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if text := assistantText(lines[i]); text != "" {
			return keepTail(text, latestKeepBytes)
		}
	}
	return ""
}

// assistantText is the sanitised text of one transcript line when it is a
// main-thread assistant record with text content, else "". Only the keys on
// the way to a text block are decoded; a block of any other type is skipped
// without decoding its payload.
func assistantText(line []byte) string {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' || !bytes.Contains(line, latestNeedle) {
		return ""
	}
	var rec map[string]json.RawMessage
	if json.Unmarshal(line, &rec) != nil {
		return ""
	}
	var typ string
	if json.Unmarshal(rec[latestTypeKey], &typ) != nil || typ != latestAssistant {
		return ""
	}
	var side bool
	if raw, ok := rec[latestSidechainKey]; ok && json.Unmarshal(raw, &side) == nil && side {
		return ""
	}
	var msg map[string]json.RawMessage
	if json.Unmarshal(rec[latestMessageKey], &msg) != nil {
		return ""
	}
	content := msg[latestContentKey]
	var whole string
	if json.Unmarshal(content, &whole) == nil {
		return sanitizeText(markdownMarks.Replace(whole))
	}
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		var bt string
		if json.Unmarshal(b[latestTypeKey], &bt) != nil || bt != latestTextBlock {
			continue
		}
		var text string
		if json.Unmarshal(b[latestTextKey], &text) == nil {
			if t := sanitizeText(markdownMarks.Replace(text)); t != "" {
				parts = append(parts, t)
			}
		}
	}
	return strings.Join(parts, " ")
}

// keepTail is the last n bytes of text, starting at a word: the panel draws
// the text's last lines, and a first word cut in half would read as a typo.
func keepTail(text string, n int) string {
	if len(text) <= n {
		return text
	}
	text = text[len(text)-n:]
	if i := strings.IndexByte(text, ' '); i >= 0 && i+1 < len(text) {
		return text[i+1:]
	}
	return strings.ToValidUTF8(text, "")
}

// wrapText word-wraps plain text into lines of at most limit display columns
// (a word wider than a line is broken), with no blank lines.
func wrapText(text string, limit int) []string {
	if limit < 1 || text == "" {
		return nil
	}
	var out []string
	for _, l := range strings.Split(ansi.Wrap(text, limit, ""), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, ansi.Truncate(l, limit, ""))
		}
	}
	return out
}

// lastLines is the last k lines of text wrapped to limit: the end of what a
// session said is what it is doing now.
func lastLines(text string, limit, k int) []string {
	lines := wrapText(text, limit)
	if len(lines) > k {
		lines = lines[len(lines)-k:]
	}
	return lines
}

// firstLines is the first k lines of text wrapped to limit, the last one
// ending in "…" when text was left over: a question is read from its start.
func firstLines(text string, limit, k int) []string {
	lines := wrapText(text, limit)
	if len(lines) <= k {
		return lines
	}
	lines = lines[:k:k]
	last := lines[k-1]
	if ansi.StringWidth(last)+1 > limit {
		last = strings.TrimRight(ansi.Truncate(last, limit-1, ""), " ")
	}
	lines[k-1] = last + "…"
	return lines
}
