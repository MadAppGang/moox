# `registry/` fixtures — `~/.claude/sessions/<pid>.json`

Marking which fixtures are REAL captures and which are SYNTHETIC is not
bookkeeping. It is the difference between "we tested the permission-prompt path"
and "we tested our *guess* at the permission-prompt path".

## The reference instant

Every timestamp here is derived from one stated instant:

```
T_ref = 2026-09-18T02:40:11.204Z = 1789699211204 epoch millis
```

Tests inject that instant as `now`, so every age assertion is exact arithmetic
rather than a tolerance window. Nothing here is relative to `time.Now()`.

The registry writes timestamps as **epoch milliseconds**, not RFC3339 — job
records and transcripts use RFC3339. The adapter converts once, on ingest.

## The measured shape these are built from

Read from the live registry on 2026-09-18: **33 records, 34 `.key` siblings**.
Every record carried the same **19 universal keys**:

```
pid  sessionId  cwd  startedAt  procStart  version  peerProtocol  peerFeatures
kind  entrypoint  pidDomain  tmux  messagingSocketPath  name  nameSource
nameSince  updatedAt  status  statusUpdatedAt
```

plus **2 optional** ones: `waitingFor` (0 of 33 — nothing was `waiting`) and
`formerNames` (2 of 33). Key **order varies between records**, which is why
every field is read by name.

Three keys the fixtures carry and the reader deliberately ignores: `procStart`,
`peerProtocol`/`peerFeatures`, `nameSince`, `entrypoint`, `pidDomain`, `kind`,
`updatedAt`. They are kept in the fixtures so the files are honest captures, and
an unread key must never cause a parse failure.

> **`formerNames` is a list of OBJECTS, not of strings.** Measured:
> `[{"name":"claudish-monitor","until":1789696384602,"sessionId":"371ebab5-…"}]`.
> The architecture describes it as a list of names and the wire field is
> `[]string`, so the adapter accepts **both** shapes and reduces the object form
> to its names. A `[]string`-only decoder errors on the real data, and an
> adapter that read that error as "record unreadable" would drop exactly the
> sessions that have been renamed.

## Inventory

| File | REAL / SYNTHETIC | What it is for |
|---|---|---|
| `live-idle.json` | REAL shape | the baseline record; every field assertion |
| `live-busy.json` | REAL shape | `busy` (3 of 33 live) |
| `live-shell.json` | REAL shape | `shell`; never observed live, but the value is in fact 1's enumeration and the mapping is trivial |
| `live-waiting-input-needed.json` | REAL shape | AC2. `"input needed"` is the **only** `waitingFor` value ever observed (1920 samples) |
| `live-waiting-permission-prompt.json` | **SYNTHETIC** | AC2. `"permission prompt"` is named in fact 1 and has **never been seen live**. This fixture is our guess at the shape; only the `waitingFor` string is guessed, the rest is a real record |
| `waiting-unknown-reason.json` | REAL shape | `waiting` with no `waitingFor` → `WaitReasonUnknown` |
| `dead-pid.json` | REAL shape, invented pid | AC1. Dead pids cannot be relied on live — a reboot leaves 33/33 alive |
| `idle-3-days.json` | REAL shape | AC3(a). `statusUpdatedAt` exactly 3 days before `T_ref`; `blockedForMs` must be `null` |
| `waiting-3-days.json` | REAL shape | AC3(b). Both `statusAgeMs` and `blockedForMs` ≈ 3 days |
| `busy-31-minutes.json` | REAL shape | §3.6's `status-suspect`, which fires at 30 minutes and never changes `status` |
| `no-status-updated-at.json` | **SYNTHETIC** | AC3(a)'s null branch. No live record has ever lacked `statusUpdatedAt`; the criterion requires the null case to be reachable and named |
| `unknown-status.json` | **SYNTHETIC** | `status: "reticulating"` → `unknown` + `rawStatus` verbatim. B7: this is an observed set, not a closed enum |
| `key-order-shuffled.json` | REAL shape, reordered | fact 12. Same content as a normal record with the keys emitted in a different order |
| `missing-optional.json` | REAL shape | neither optional key present |
| `no-tmux-field.json` | **SYNTHETIC** | B6 warns `tmux` is not structurally guaranteed. Not observed (33/33 carry it), so the absence is synthetic — and the liveness rule for it (`proc-only`) is therefore untested against reality |
| `with-former-names.json` | **REAL shape, real object form** | the two-element object form, redacted names |
| `formernames-strings.json` | **SYNTHETIC** | the plain-string form the architecture assumed. Kept so both decoders stay exercised |
| `torn.json` | **SYNTHETIC** | §6.5. A record truncated mid-object at 60% of its length, which is what an in-place rewrite leaves behind. Not valid JSON, on purpose |
| `duplicate-sessionid-a.json`, `-b.json` | **SYNTHETIC** | M8. One `sessionId`, two pids (1000 and 2000). Presence of `sessionId` was measured; **uniqueness was not** |
| `no-session-id.json` | **SYNTHETIC** | a record with no join key → parse error, never an invented id |
| `background-process.json` | REAL capture (2026-09-23), name invented | the registry is not interactive-only: a background session's own process registers with `"kind":"bg"` and the job it serves in `jobId`, and no `tmux` key. The join merges it into that job (`join_twins.go`) |
| `version-2.1.263.json` | REAL shape, upstream's pin | §8.3 item 2. Upstream agentbar pins CC 2.1.263; this machine runs 2.1.273–2.1.275 and live records report **three versions simultaneously**, so `version` is per-session and heterogeneous. Kept as a *regression* fixture so a format change surfaces as a diff, not a break |
| `56526.….key` | REAL name, dummy content | the sibling the registry keeps beside each record. **Must be counted and never opened.** Its content is deliberately not JSON, so a reader that opened it would also raise a parse error — but "never touched" is the stronger property, and a counting `fs.FS` asserts it |

## Privacy

Registry records carry no prompt text — no `intent`, no `output`, no message
`content`. Every `cwd` and `name` here is either a real public path or invented.
`TestNoFixtureCarriesUserContent` asserts no file in this directory contains a
field name on the privacy denylist, so a fixture pasted in from a
prompt-bearing source (a transcript, a job record) fails the suite.

## What these fixtures deliberately do NOT cover

- **Two records for the same pid.** Structurally impossible: the filename is the
  pid.
- **A `waiting` session with a `waitingFor` value we have never seen.** Covered
  by `waiting-unknown-reason.json`'s path, which is the same code path.
- **A record written by a Claude version newer than 2.1.275.** Unknowable. The
  `unknown-status.json` pass-through rule is the mechanism that keeps such a
  record readable, and it is tested.
