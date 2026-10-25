# `agents/` — `claude agents --json` fixtures

Every file here is marked REAL, DERIVED or SYNTHETIC. The distinction is the
difference between "we tested the `waiting` path" and "we tested our *guess* at
the `waiting` path", which is why §9.1 asks for it.

| File | Provenance | What it is for |
|---|---|---|
| `real-39.json` | **REAL shape, synthetic values** — captured 2026-09-18 15:43, then every `cwd` and `name` replaced by an invented one (one mapping shared by every file here) before the tree went public | Today's listing: a flat ARRAY of 39 entries — 36 interactive (`pid` + `status`) and 3 background (`id` + `state`, no `pid`). The shape every decode assumption is checked against. |
| `real-37-with-waitingfor.json` | **DERIVED** from the 2026-09-17 research capture (35 entries), with one interactive entry set to `status: "waiting"` + `waitingFor: "input needed"`, and every `cwd` and `name` replaced like `real-39.json`'s | §0.3 measured that field on 1 of 34 entries — the only one whose status was `waiting`. No live session is `waiting` today, so it cannot be re-captured; every other field is the real capture, untouched. |
| `background-only.json` | **REAL shape** — the three pid-less entries of `real-39.json`, verbatim (so with its synthetic values) | A registry-only observer is blind to these. They are why the join has two sources. |
| `interactive-no-registry.json` | **DERIVED** — one real interactive entry with an invented pid, session id and name | The listing's ONE exclusive contribution: existence for an interactive session with no registry file. |
| `cross-kind-sessionid.json` | **DERIVED** — two real entries rewritten to share one session id | M8's cross-kind case: one id as both kinds, never merged. |
| `empty.json` | **SYNTHETIC** | `[]`. "Claude is not running" is a fact, and it must not read as a failure. |
| `malformed.json` | **SYNTHETIC** | Not JSON. Must produce a FAILED listing (`ok: false`), never an empty successful one — an empty listing with `ok: true` is positive evidence of death for every background job on the machine. |
| `hangs` | **SYNTHETIC**, executable | A child that never exits. Proves the spawn timeout kills it and that the previous listing survives. |
| `needs-live-cwd` | **SYNTHETIC**, executable | Fails like the real CLI when its working directory was deleted, and answers `[]` otherwise. Proves the spawn does not inherit the observer's cwd. |

## Measured field union (39/39 entries, 2026-09-18)

```
cwd        string   39/39
kind       string   39/39   "interactive" | "background"
name       string   39/39
sessionId  string   39/39   the join key
startedAt  NUMBER   39/39   epoch MILLISECONDS, not RFC3339
pid        number   36/39   interactive only
status     string   36/39   interactive only
id         string    3/39   background only, the 8-char short id
state      string    3/39   background only
```

`state` and `status` are DIFFERENT fields and are never conflated: background
entries carry `state`, interactive ones carry `status`.

Nothing here carries a privacy-denylisted key (`intent`, `output`,
`suggestedReply`, `lastPrompt`, `message`, `content`, `text`) — the listing
contains no message content to begin with, and the fixture privacy test asserts
it over the whole tree.
