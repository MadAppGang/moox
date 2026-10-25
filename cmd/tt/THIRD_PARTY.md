# Third-party notices — `moox/cmd/tt` (tt, part of moox)

This file exists because §8.2 of the observer architecture seeded the
`internal/observer/adapters/` tree from **`ssnxd/agentbar`**, an MIT-licensed
Go project, and MIT's one condition is that its copyright and permission notice
travel with any copy or substantial portion of the software.

It records three things, in this order: what the upstream is, exactly how much
of it ended up here, and the licence text in full.

---

## 1. Upstream

| | |
|---|---|
| Project | **ssnxd/agentbar** |
| URL | https://github.com/ssnxd/agentbar |
| Licence | **MIT** (`LICENSE`, SPDX `MIT`) |
| Copyright | `Copyright (c) 2026 Suraj (ssnxd)` |
| Pinned commit | **`4da67f85fc5987ff8a6afeeb86e873d323f616b0`** — `ui: status-first cards with subagent rows, numbered jump, quiet waiting, padding`, committed 2026-09-10T09:54:17Z |
| Pin verified | 2026-09-18, against `api.github.com/repos/ssnxd/agentbar` (this commit is the repository's `main` HEAD; the repository's last push is the same commit) |
| Dependencies taken | **none.** Both seeded upstream files are stdlib-only |

**Why this project and not a library.** Every surveyed Go project that reads
Claude Code's session registry keeps its reader under `internal/`, which the Go
tool forbids importing across modules — a language guarantee, not a convention.
The one exported Go package that would have been importable,
`agentre-hub/agentre`'s `pkg/claudecode`, is **GPL-3.0**. `dotfiles` carries
**no LICENSE file at all**, so it is "all rights reserved" in a public
repository; linking a GPL-3.0 package would oblige relicensing `tt` under
GPL-3.0, which is not this repository's call to make inside a feature branch.
MIT is permissive, so seeding from agentbar costs exactly this file.

> Raised deliberately, and out of scope here: *"public repo with no LICENSE"* is
> what blocked the GPL option and will block the next one. Whether `dotfiles`
> gets a LICENSE is the repository owner's decision.

---

## 2. What was actually taken, file by file

The honest answer is **less than §8.2 planned**, and the difference is recorded
rather than smoothed over. The upstream sources were not available in the
environment the adapters were written in (P1/P3/P6), so those phases worked from
§8.2's *description* of the upstream — its quoted regular expression, its cache
key shape and its path layout — and from this machine's own measured records.
The upstream files were fetched at P10, at the pinned commit above, to make this
notice accurate rather than assumed.

| Upstream file | Lands at | What is here |
|---|---|---|
| `internal/registry/registry.go` | `internal/observer/adapters/registryfs/record.go` | **One line is character-identical**: the colon-safe anchored tmux reference pattern `` `^(.+):(@\d+)\.(%\d+)$` `` (upstream `tmuxRe`, here `tmuxRefPattern`). Session names may contain colons, so a naive `SplitN` is wrong, and this is the expression that gets it right. The surrounding parse is **not** copied — see "changes" below |
| `internal/registry/registry.go` | `internal/observer/adapters/proclive/proclive.go` | The **`syscall.Kill(pid, 0)` liveness idea**, not its code: upstream returns a bare bool, this returns `(bool, error)` with ESRCH, EPERM and "could not answer" kept distinguishable |
| `internal/transcript/transcript.go` | `internal/observer/adapters/transcriptfs/` (`transcriptfs.go`, `facts.go`, `timing.go`) | The **bounded-tail-plus-`(size, mtime)`-cache design**. Upstream's `chunk = 64 * 1024` and its `cached{size, mod, info}` map are the shape; no upstream statement is reproduced verbatim |
| `internal/transcript/agentmeta.go` | `internal/observer/adapters/transcriptfs/subagents.go` | The **path knowledge only**: `<transcript-without-.jsonl>/subagents/agent-<id>.meta.json`. Upstream reads one named file to answer "what is this agent"; we count the files to answer "how many were there" |
| `internal/registry/registry_test.go` | — | **Not taken.** The fixtures here are this machine's own records, re-pinned (see change 2) |
| `internal/daemon/`, `internal/hooks/`, `internal/tmuxctl/`, `internal/state/`, the whole UI | — | **Deliberately not taken.** `hooks/` installs a Claude hook, which is a **write to `~/.claude/`** and this feature's hard constraint 1; `daemon/` polls on a 1 s ticker, which the NFR and AC9 forbid; `tmuxctl/` writes tmux state; the UI is bubbletea **v2** (`charm.land/bubbletea/v2`) and `tt` is on v1.3.10 |
| — | `internal/observer/adapters/jobsfs/` | **Nothing. Written from scratch.** No surveyed project reads `~/.claude/jobs/` at all |

**Net: one regular-expression literal is byte-identical; everything else in this
tree is independently written from the same measurements.** That is still a
derivation, and MIT's notice condition is satisfied here regardless of how small
the overlap turned out to be — under-attributing is the failure mode worth
avoiding, not over-attributing.

Each derived file carries a header comment naming its upstream file and pointing
at this document.

## 3. What we changed, and why

1. **The 1 s ticker is gone.** Upstream polls (`Interval = time.Second`); this
   observer is driven by a kqueue `Watcher` port and does no periodic work at
   all. AC9 ("no timer, no poll loop") is enforced by an AST audit over the
   whole module.
2. **The fixture is re-pinned.** Upstream pins Claude Code **2.1.263**; this
   machine runs **2.1.275** and its live registry records report **≥3 `version`
   values simultaneously**, so `version` is per-session and heterogeneous. The
   2.1.263 shape is kept as a *regression* fixture so a format change surfaces
   as a diff instead of a break.
3. **No json struct tags outside `wire`.** Upstream decodes into a tagged
   `Entry`. Here, records are decoded by explicit key-**name** lookup, because
   registry key order is not stable and because one set of tags in one package
   is what makes the CLI, MCP, socket and TUI surfaces byte-identical.
4. **The colour tail is 200 KB, not 64 KB**, validated across all 35 transcripts
   that carry an `agent-color` record (0 misses, largest file 30.7 MB); a
   separate **64 KB timing tail** scans backward for the newest record that
   carries a `timestamp`, skipping the eight record types measured to carry
   none. Never a full scan — the machine's transcripts total 630 MB.
5. **`agentColor` is read from `type == "agent-color"` records, last-wins**, not
   by substring match: a grep for `agentColor` overcounts by 72 % on a measured
   session.
6. **API errors are counted from `isApiErrorMessage`, never `is_error`.**
   Machine-wide, `is_error` appears 8 243 times across 1 803 transcripts
   (overwhelmingly *recovered* tool failures) against 1 103 across 961 for
   `isApiErrorMessage` — using `is_error` would be wrong by ~7.5×.
7. **Subagents are counted by file, not parsed from the transcript.** A nested
   `spawnDepth: 2` agent leaves no record in its parent's transcript, so
   transcript-derived counts undercount by ~7.2 %. An absent directory yields
   `subagents: null, subagentsKnown: false`, which stays distinct from a
   genuine `0`.
8. **Transcripts are located by glob over an injectable `fs.FS`**, never by a
   cwd→slug derivation (measured 95.86 % agreement over 356 directories, and
   unfixable). More than one match is `null` plus a `transcript-ambiguous`
   advisory, never a guess.
9. **tmux sigils are kept.** The registry field is
   `#{session_name}:#{window_id}.#{pane_id}` and `@`/`%` are part of the
   identifiers; stripping them yields ids tmux will not match, and it fails
   *silently*.
10. **`kill(pid, 0)` reports three outcomes, not two** (see the table above), so
    an errored gate can never be folded into "dead".
11. **The tail reader gained a second locator, `OpenPath`**, for a background
    job's `linkScanPath`, which is validated against the projects root because a
    job record is untrusted input.
12. **Nothing here writes to `~/.claude/`, ever**, which is the constraint that
    ruled `hooks/` out. `~/.local/state/tt/` is the only directory `tt` writes,
    and an AST audit enforces that every write call in the module lives in one
    of exactly two packages.

---

## 4. The MIT licence, in full

Reproduced verbatim from `LICENSE` at the pinned commit.

```
MIT License

Copyright (c) 2026 Suraj (ssnxd)

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```

## 5. Other projects read, and not copied

Read as specification only — no code, no assets, and nothing in this table
imposes a notice obligation, but naming them is how the next reader knows where
the shapes came from:

| Project | Licence | What it contributed |
|---|---|---|
| `dob323/session-kit` | MIT (Python) | the doctor's *pattern* — one live example of each format, `ok`/`warn`/`fail` per field — and its warning about a history that "quietly stops working while every command still exits 0" |
| `simoncoombes/agentpane` | MIT | a second opinion on registry parsing |
| `dpws/berth` | MIT | its `jsonl.Tailer` was compared against the bounded tail here |
| `agentre-hub/agentre` | **GPL-3.0** | **rejected as a dependency** — see §1 |
