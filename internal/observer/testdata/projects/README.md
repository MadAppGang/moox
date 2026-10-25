# `testdata/projects/` — the transcript fixtures

Mirrors `~/.claude/projects/<slug>/<sessionId>.jsonl`. Read by `transcriptfs`.

This tree now serves BOTH reads §7.3 keeps apart, and which fixture belongs to
which is not cosmetic:

- **the TIMING half (P3)** — the 64 KiB tail that scans backwards for the newest
  record carrying a `timestamp`, a **correctness** input to `statusSince`'s
  cross-check and to §3.5 rules 4-5. Fixtures: `proj-f`, `proj-g`, the two
  REAL-shape job transcripts, and the two deliberate ABSENCES.
- **the ENRICHMENT half (P6)** — the 200 KiB tail that reads `agentColor`,
  `isApiErrorMessage` and the subagent count. Fixtures: `proj-a` through
  `proj-e` plus `proj-a-collide`.

§7.3 insists the two reads stay separate concerns with separate bounds and
separate cache keys. Nothing in the enrichment set is read by the timing pass or
the other way round.

## Redaction — the key is gone, not the value

Every record here is the REAL shape of a transcript record with **every
content-bearing key removed**: no `message`, no `content`, no `text`, no
`lastPrompt`. What is kept is the scaffolding that makes the shape real —
`type`, `uuid`, `sessionId`, `sessionKind`, `timestamp`, `version`, `cwd`,
`userType` — because the one thing the timing pass reads is `timestamp`, and
the one thing it has to get right is **which record** carries one.

`fixtures_privacy_test.go` asserts those key names appear nowhere under
`testdata/`. These files are committed to a repository with a remote.

## The record types with no `timestamp`

Eight were measured on the real tree: `ai-title`, `agent-name`, `mode`,
`permission-mode`, `atis-latch`, `file-history-snapshot`, `last-prompt`,
`cost-state`. **On 4 of the 5 real job transcripts the FINAL LINE IS ONE OF
THEM**, so "read the last line and take its timestamp" returns nothing on 80%
of the population. The newest timestamped record sits 1 to 3 records and 747 to
1 880 bytes back from EOF; the worst case measured anywhere on the machine is
**1 912 B**, which is what makes 64 KiB a 33x guard rather than a tuned number.

## The fixtures

| Fixture | REAL / SYNTHETIC | What it is for |
|---|---|---|
| `-home-user-proj-f/ffffffff-…jsonl` | **REAL shape** | §0.4d's case: the last **three** records carry no timestamp (`cost-state`, `file-history-snapshot`, `last-prompt`). The answer is three records back. A last-line reader fails this and nothing else in the suite would notice. |
| `-home-user-proj-g/99999999-…jsonl` | **SYNTHETIC** | 78 KB of untimestamped trailing records — deliberately **larger than the 64 KiB production tail**, so the `activity-since-truncated` guard is reachable at the production bound as well as at an injected 2 KB one. It is the only file in the set for which that is true. |
| `-home-user-dotfiles/97030e53-…jsonl` | **REAL shape** (`97030e53`) | `blocked-with-needs`'s transcript. Its newest timestamped record is a `system` entry at `03:15:47.420Z`, **101 ms after** the record's `updatedAt` — the real measured agreement — and the file ends with two untimestamped records. |
| `-home-user-dotfiles/unrelated-neighbour-…jsonl` | **SYNTHETIC** | Lives in the SAME slug directory as `blocked-beyond-retention`'s reaped target. It is what a glob fallback would wrongly return, so "no glob fallback is attempted" is asserted by counting opens on this file. The real dotfiles slug holds 31 such neighbours. |
| `-home-user-dev-orbit-orbitmini/093c8e29-…jsonl` | **REAL shape** (`093c8e29`) | Went quiet on 2026-08-19 while its record was written on 2026-09-08 → AC3(c)'s `statusSinceLowerBound` case, `deltaMs` 20 days. |
| `-home-user-mag-authflow-authflow-cloud/d3ed2cc8-…jsonl` | **REAL shape** (`d3ed2cc8`) | Carries records **3 m 08 s AFTER** the daemon's `reapedMidWorkAt`, because the conversation was resumed interactively. The transcript can outlive the daemon; neither timestamp means "alive". |
| `-home-user-proj-h/11111111-…jsonl` | SYNTHETIC | `working-fresh`'s transcript, newest timestamp one minute before `tRef`. |
| `-home-user-proj-h/33333333-…jsonl` | SYNTHETIC | `done`'s transcript, so a terminal record still has a located target. |
| `-home-user-proj-i/22222222-…jsonl` | SYNTHETIC | `working-transcript-stale`'s transcript, 40 minutes old against a 15-minute threshold → §3.5 rule 5. |

## The P6 enrichment fixtures

All SYNTHETIC in their bytes and REAL in their shape: every record is a real
record type with every content-bearing key removed, and each fixture is built so
that **each wrong implementation produces a different, nameable answer**.

| Fixture | REAL / SYNTHETIC | What it is for |
|---|---|---|
| `-home-user-proj-a/aaaaaaaa-…jsonl` | SYNTHETIC (real shape) | The ordinary case: an `agent-color` record inside the window (`purple`), plus two `toolUseId`-bearing records. Its `aaaaaaaa-…/subagents/` directory is **AC4**. |
| `…/aaaaaaaa-…/subagents/` | SYNTHETIC | **Three** `*.meta.json` files — two `spawnDepth:1` whose `toolUseId` appears in the transcript, one **`spawnDepth:2`** whose `toolUseId` appears NOWHERE in it. Also two `agent-*.jsonl` siblings, which a count of directory entries would double-count. By file: 3. Transcript-derived: 2. |
| `-home-user-proj-a-collide/c01110ed-…jsonl` | SYNTHETIC | **AC7.** An UNRELATED session's transcript in the directory a cwd→slug derivation resolves `proj-a`'s session to (`proj_a` and `proj-a` produce the same slug — the measured 4.14 % that cannot be fixed). Its colour is `yellow`, so reading it is visible in the answer as well as in the open count. It must never be opened. |
| `d0b1e000-…jsonl`, in **both** `proj-a` and `proj-a-collide` | SYNTHETIC | **AC7's ambiguity clause.** One session id, two slug directories → 2 glob matches → `transcript: null` + `transcript-ambiguous`, and **neither file is opened**. Both carry a colour, so a reader that picked one would look right. |
| `-home-user-proj-b/bbbbbbbb-…jsonl` | SYNTHETIC | **AC12.** 11 594 bytes with its `agent-color` record (`cyan`) on the SECOND line. At an injected 2 KiB bound: `colorFound:false`, `agentColor:""`, `truncated:true`, **no error**. At the production 200 KiB bound: `cyan`. One fixture, both sides of the bound. |
| `-home-user-proj-c/cccccccc-…jsonl` | SYNTHETIC | **AC6**, and every wrong answer is a different colour: `blue` = an earlier `agent-color` record (first-wins), `red` = an `agent-name` record's bare `color` key, `pink` = an `assistant` record's bare `color` key, `orange` = the LAST line, which carries `agentColor` with `type:"assistant"` and `slug:"agent-color"` (the substring-grep answer; a real grep overcounted by 72 % on pid 98693). The right answer is **`green`**. |
| `-home-user-proj-d/dddddddd-…jsonl` | SYNTHETIC | **AC5.** Two RECOVERED `is_error` records and one `isApiErrorMessage:false`. `apiErrors` must be **0**. |
| `-home-user-proj-e/eeeeeeee-…jsonl` | SYNTHETIC | **AC5.** Two `isApiErrorMessage:true` records (with the real sibling keys `error` and `entrypoint`) **plus** a recovered `is_error`. `apiErrors` must be exactly **2**, not 3. |

### One redaction that changes a shape, stated because it matters

In a real transcript `is_error` sits at `message.content[].is_error`, inside a
`tool_result` block — and `message` and `content` are on the privacy denylist,
so they are gone from every fixture. The two fixtures above therefore **hoist
`is_error` to the top level of the record**. That makes the assertion STRICTER
than reality rather than weaker: both a substring scan for `is_error` and a
top-level key check fail these fixtures, and the production reader passes them
because it asks for `isApiErrorMessage` and nothing else.

The same applies to `toolUseId` in `proj-a`, which the real transcript nests at
`message.content[].id`.

### Measured while building P6, on the live machine

`-home-user-dev-orbit-acmetechx-data-engine-demo/c874de20-…`, 17 meta files:

    16 x spawnDepth 1  -> each toolUseId appears 3-6 times in the parent transcript
     1 x spawnDepth 2  -> its toolUseId appears ZERO times

A transcript parser answers 16 where the truth is 17, with a number that looks
entirely reasonable. Machine-wide the undercount is ~7.2 %. Today's tree: 193
subagent directories, 1 647 meta files — fact 10 recorded 196/1 556, round 2
193/1 503, §8.3 item 6 193/1 757. **The method is the fixed thing, not the
number**, and no test asserts a machine-wide total.

## Deliberately ABSENT, and the absence is the fixture

Two `linkScanPath` targets have no file here, reproducing the two real
absences, which have **different shapes**:

- `-home-user-dotfiles/7a324a6d-….jsonl` — the project directory **exists**
  (it holds the neighbour above) and only this session's transcript is gone.
  The real one was reaped by the vendor's 30-day cleanup at 31.8 days.
- `-home-user-mag-claudish--claude-worktrees-resume-message/4d09bc19-….jsonl`
  — the **whole slug directory** is gone, because the real worktree was
  deleted. 39.1 days old.

Both must read as not-found, and neither may be an error: a missing transcript
is §3.5 rule 4's evidence, not a failure.

## The retention floor is NOT derived from this tree

`transcriptfs.DeriveFloor` globs `*/*.jsonl` — **non-recursive, normative**. Its
test builds its own `fstest.MapFS` with explicit `ModTime`s rather than using
these fixtures, because a floor measured over files whose mtimes are whatever
`git checkout` set would assert nothing. Live measurement, 2026-09-18:

    projects/*/*.jsonl        3 181 files   oldest  30.11 d   past 30 d:   2
    projects/**/*.jsonl       4 981 files   oldest 105.68 d   past 30 d: 335

The vendor does not reap `subagents/` at all, so a recursive re-derivation
returns 105 days and silently disables rule 4 for three months.
