# `testdata/jobs/` — the background-job fixtures

One directory per fixture, each holding a depth-1 `state.json`, mirroring
`~/.claude/jobs/`. Read by `jobsfs` and by the background half of the join.

**Every fixture is derived from a REAL record's key set and shape.** The 19
universal keys plus the optionals are present and in a deliberately shuffled
order (key order is not stable on the real tree — measured fact 12). What is
**not** real in any of them:

- **Session ids, names, `detail`, `needs` and `linkScanPath` are rewritten.**
  `linkScanPath` points at `/fixtures/projects/...` so the set is
  self-contained and no assertion depends on the machine it runs on.
- **`intent`, `output`, `suggestedReply`, `respawnFlags` and `providerEnv` are
  removed entirely.** The first three carry the user's own prompt, the agent's
  assistant messages and a draft reply written for the user;
  `fixtures_privacy_test.go` asserts those key names appear nowhere under
  `testdata/`. Redacted here means **the key is gone**, not that the value was
  replaced — an absent key is checkable, a replaced value is somebody's
  judgement call.
- Timestamps are chosen against §5.2's single reference instant
  `2026-09-18T05:20:11.204Z` (`tRef`), so every age in the suite is exact
  arithmetic with no tolerance.

`jobsfs` never opens anything except `<dir>/state.json` at depth 1, which is
asserted by counting opens rather than by inspecting the answer.

## The fixtures

| Fixture | REAL / SYNTHETIC | What it is for |
|---|---|---|
| `pins.json` | **REAL** (contents `[]`) | A root **FILE**, not a job directory. 15 entries / 14 directories / 7 records is M3's correction to measured fact 13. Must never be enumerated as a job and never opened. |
| `blocked-with-needs/` | **REAL shape** (`97030e53`) | `blocked` + `needs` + `detail` + `tempo: "blocked"`, transcript present, and its newest timestamp 101 ms after `updatedAt` — the measured agreement. Carries a **`timeline.jsonl` on purpose**, whose last entry is the state RUN START three hours earlier: revision 1 derived `statusSince` from that and over-reported every blocked duration permanently. A test asserts the field ignores it. |
| `blocked-no-needs-tempo-idle/` | **REAL shape** (`ea780079`) | `state: blocked` with `tempo: "idle"` → §3.5 rule 3, `live: null`. Also the one real record carrying **`children`**, and they are an array of OBJECTS describing produced ARTIFACTS (a PR), not child session ids. The href here is synthetic. |
| `blocked-beyond-retention/` | **REAL shape** (`7a324a6d`) | `linkScanPath` target **absent** and the record older than the retention window → §3.5 rule 4, `live: null`. The project directory exists and holds an unrelated neighbour, so this is the "only this session's file was reaped" shape. |
| `reaped-mid-work/` | **REAL shape** (`d3ed2cc8`), **state changed** | `reapedMidWorkAt` present → §3.5 rule 2, `live: false`. **The real record is also `failed`, so rule 1 answers first and rule 2 is covered only by luck.** This fixture's state is `working` so rule 2 is the rule under test — a record reaped while non-terminal would otherwise read `live: true, attention: true` with the evidence two keys away. |
| `working-fresh/` | SYNTHETIC | `working` with a transcript whose newest timestamp is one minute before `tRef`. The only fixture that can reach `live: true`, and only with a fresh vendor listing (P4). |
| `working-transcript-stale/` | SYNTHETIC | Record **fresh** (`updatedAt` one minute before `tRef`), transcript 40 minutes stale → §3.5 rule 5. A record-based rule answers "fine" and a transcript-based one answers "stale"; they cannot both be right. **THE M14 CASE**: it must stay in the DEFAULT set. |
| `done/`, `failed/` | SYNTHETIC | Terminal → `live: false`, excluded by default, present with `includeDead` (AC1's background half). `failed/` also names **no transcript at all**, which is the `transcriptCollected: false` case. |
| `record-later-than-activity/` | **REAL shape** (`093c8e29`) | The record was written 20 days after the thread went quiet → `statusSinceLowerBound` + `status-since-record-later-than-activity` with a `deltaMs` of 20 days. **AC3(c)'s named case.** |
| `shortid-mismatch/` | SYNTHETIC | `daemonShort` ≠ the directory name → advisory, and the record is still used. |
| `linkscan-outside-root/` | SYNTHETIC | `linkScanPath` escapes the projects root via `../../..` → refused, with no glob fallback attempted. A record is untrusted input. |
| `dup-a/`, `dup-b/` | SYNTHETIC | M8: two directories, one `sessionId`. First in directory order wins; the loser is discarded, not merged. |
| `cross-kind/` | SYNTHETIC | M8: carries `registry/live-idle.json`'s `sessionId`, so one id appears as both kinds. Two sessions are emitted, never merged. |
| `unknown-state/` | SYNTHETIC | `state: "reticulating"` → `StatusUnknown` with the literal kept, §3.5 rule 7. Its `updatedAt` is deliberately **inside** the retention window: rule 4 does not look at the status at all and would otherwise match first. |
| `no-state-json/tmp/` | **REAL shape** | 7 of the 14 real directories look like this, the oldest dating from 4 June. Not a parse error. **Armed anyway** (C1, P5). |
| `missing-state-json-transient/` | **REAL shape** | The directory exists and the record is briefly absent — §6.5's window on this tree. |
| `torn/state.json` | SYNTHETIC | Truncated mid-object. A parse failure means TORN, not gone: the session is carried forward, and the neighbours are untouched. Placed in a **directory** rather than at the root as §9.1 sketched it — a `state.json` at the jobs root is a non-directory entry and would be ignored entirely, testing nothing. |
| `no-session-id/` | SYNTHETIC | No join key → a counted parse error, never an invented id. The directory name is an 8-char prefix and a UUID cannot be reconstructed from it. |
| `decoy-nested/tmp/a/b/c/.claude/state.json` | **SYNTHETIC ADVERSARIAL** | M13. A `state.json` at **depth 6** carrying both `state` and `sessionId`, so it would parse into a plausible-looking background agent. The real tree has two such decoys, at depths 5 and 8 under `be9663df/tmp/`. A `filepath.WalkDir` reader reports 9 agents on the real machine, two of them fabricated. **MUST NEVER BE READ**, asserted by counting opens. |

## Not here, and why

`no-state-json-then-record/` and `terminal-then-resumed/` are **scripted
sequences**, not static fixtures — an empty directory into which a record
appears, and a `done` record replaced by a `blocked` one. They cannot be
expressed as files on disk, so P5's end-to-end suite builds them in a temp
directory instead. Terminal jobs really do resume: 17 terminal → non-terminal
transitions measured across 6 of 7 timelines.
