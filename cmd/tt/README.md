# tt — tmux session manager

A modal terminal UI (k9s / lazygit style) for managing tmux sessions:
list, filter, attach, create, rename, and kill — without leaving the keyboard.

Sessions are listed **most-recently-attached first**, so the session you last
worked in is at the top and `enter` jumps straight back to it. Just start
typing to search — no `/` first. Failed actions (a tmux error, or a duplicate
session name) surface as a footer message rather than failing silently.

Built with [bubbletea](https://github.com/charmbracelet/bubbletea),
[bubbles](https://github.com/charmbracelet/bubbles) (list),
and [huh](https://github.com/charmbracelet/huh) (prompts).

## Keys

| Key                 | Action                                             |
|---------------------|----------------------------------------------------|
| *any letter*        | search — just type, no `/` needed                  |
| `enter`             | attach to the highlighted session                  |
| `↑`/`↓`, `^p`/`^n`  | move the highlight (works while searching)         |
| `^t`                | new session — prompts for a name                   |
| `^r`                | rename the highlighted session                     |
| `^x`                | kill the highlighted session — asks to confirm     |
| `^o`                | show/hide the Claude session panel                 |
| `^g`                | with the panel open: group by project → status → tmux → colour |
| `esc`               | clear the search, or quit when there is none       |
| `^c`                | quit                                               |

Inside tmux, `enter` switches the client; outside tmux it attaches directly.

The verbs are ctrl-chords because plain letters belong to the search box. They
work **while you are searching**, on the highlighted match — so the flow is
type a few letters, then `enter` to attach or `^x` to kill, without a separate
step to "accept" the search first. `^t` inherits whatever you typed as the new
session's name, which makes "search for it, find nothing, create it" one move.
`/` still opens an empty search if that is the habit in your fingers.

Creating a session attaches to it immediately rather than dropping back to the
list — naming a session is asking to be in it.

`^a` is deliberately unbound: it is the tmux prefix, so it would never reach
`tt` where `tt` actually runs.

## The Claude session panel — `^o`

`^o` opens a column beside the session list showing what every Claude Code
session on this machine is doing. `^g` (or `g` in a sidebar) changes how it
groups them, in a cycle, and in a sidebar `p`, `t`, `m` and `c` (either case)
go straight to one:

```
project   one group per git repository — the default
status    NEEDS YOU · ACTIVE · IDLE · UNCLEAR
tmux      one group per tmux session
colour    one group per /color, in spectrum order
```

```
   [P]roject │ S[t]atus │ T[m]ux │ [C]olour
──━━━━━━━━━━━─────────────────────────────────
░░░░▒▒▒▓▓  2  NEED  1  RUN  2  IDL  ▓▓▒▒▒▒░░░░


───────────────  NEEDS YOU  2  ───────────────
▌ BLK   3h41m    migrate-db              612k
▌    waiting 41m · confirm the migration plan
▌    alpha
▌ ASK   3h13m    wire-ui                 248k
▌    waiting 13m · input-needed
▌    alpha

─────────────────  beta  2  ──────────────────
  IDL   3h14m    badge-tone              431k
     The badge colours now come from the
     design sheet; every pair clears its
     contrast floor in both themes.
▌ RUN   3h02m    chip-fit                194k
▌    Running go test ./... in moox — 21
▌    packages, the panel ones first.

─────────────────  gamma  1  ─────────────────
  IDL   6h00m    no-usage-yet               —
     Ready when you are.


═══  j/k  move  ↵  go  /  filter  f  only  ═══
```

(A synthetic snapshot through `tt panel-render --profile=ascii`, so without
its colour. Every frame in this section comes from the harness below.)

**The header** is the grouping as tabs, the one in use filled with the accent
and underlined by the thick stretch of the rule beneath it; in a sidebar each
tab shows its key in brackets. Under it, one strip counts the whole fleet —
**NEED** (its number on solid red), **RUN**, **IDL** and **???** — drawing
only the counts that are not zero, with a fade either side. Every group then
opens with a centred title that carries its row count.

In every grouping but status, the sessions that need you — waiting on a
prompt, or blocked on a question — are lifted out of their groups into a
**NEEDS YOU** band pinned under the header, longest wait first. Under a band
row are up to three dim lines of **how long and why** it waits, wrapped, and
one more naming its home group when there is room. Under every other row are up to three dim lines of **the
latest thing the session said** — the end of its newest reply — so the panel
shows what each session is doing right now, not only its status. The session's
`/color` bar runs down through those lines.

**The footer** is the panel's last line, pinned to the bottom row: the keys of
the surface it is on — `j/k` `↵` `/` `f` in a sidebar (`g` still cycles the
grouping, but the tabs already show a key for each one), `^g` in the TUI (where
plain letters belong to the search box) —
between two ══ rules. A note that something is wrong (no observer, a degraded
watch, a stale source, a problem in `panel.conf`) sits dim above it; where the
data came from is not drawn.

**Light or dark follows the terminal, live.** `TERM_THEME` (or, without it,
the terminal's answer to an OSC 11 query) decides the first frame; after that
tt asks the terminal to report every light/dark switch (DEC mode 2031, the
same reports the zsh prompt and Claude Code use; inside tmux, tmux relays the
window's client theme) and redraws in the new theme at once, the sidebar and
the TUI alike. The reports are switched off again when tt exits.

**A sidebar lights up when it holds the focus.** Focused, its chrome is lit:
the active tab is filled with the accent, the thick ━ under it and the ══ either
side of the keys are in the accent too, so the panel is bracketed top and
bottom. Click or switch to another pane and it comes to rest. The active tab
keeps its fill, lettered in the accent, while the other tabs lose theirs and
dim, so the grouping in use is the one block on the line. The ━ falls back to
the rule colour. Because no key reaches the pane, no shortcut is shown: the
tabs drop their `[P]` brackets, and the footer becomes a ══ rule around one dim
word, `connected`, while the sidebar is attached to the observer. It does not
show a time: the observer sends only changes, so over a quiet fleet a time would
read as stale when the feed is fine. Offline, the word goes and the panel's own
note says what is wrong. When a filter is on, the rule names what it keeps
first (`claudish · RUN · connected`). The rows do not move. tmux passes focus changes on (`focus-events on`), and the sidebar asks
once at start which state it is in. The TUI column is always lit.

**When the list does not fit, it scrolls.** The header and the footer stay
put, the rows between them move, and the top-right corner of the tabs line
says where you are: `▼ 38%` at the top (more below, 38% seen), `▲▼ 61%` in
the middle, `▲ 100%` at the bottom. In a sidebar the window follows the
cursor (below); a new grouping starts at its top. Nothing is cut and no line
sums up hidden rows. The band comes first, so at the top of the list what
needs you is always in view. (The tabs shorten to `[P]roj …` to make room for
the corner on a 46-column sidebar.)

**A cursor picks a session, and `enter` goes to it**, in a sidebar. The
highlighted row's first line is a solid selection bar in the active tab's
colours, bold, with a `❯` in its gutter: its status, runtime, name and ctx all
take those colours. The row's other lines sit on a tint of the same accent. `j` and `k` (or `↓` and `↑`) move it over the
rows in the order they are drawn, across sections, and stop at either end. The
rows scroll only when the cursor would leave the window, so the window follows
it. Moving onto a section's first row brings that section's title into view,
and the first row scrolls back to the top, where the band is. `enter` switches
your tmux client to that session's pane: its session, window and pane at once
(`tmux switch-client -t %N`). A background job has no pane: `enter` opens it
in a popup over the window, `claude attach <id>` in the job's folder (`Ctrl+Z`
closes the popup and the job keeps running). A job that cannot be attached —
ended, its conversation gone, its folder deleted — opens nothing and the footer
says why. `space` opens the highlighted session in a popup instead
of switching to it: a background job with `claude attach`, even when it has a
pane of its own; a session in a pane as a second view of that pane, through a
temporary tmux session grouped with its own, so no other client leaves its
window (like `enter`, it makes the pane the active one in its window).
`prefix space` closes either popup (so does `prefix d`); outside a popup it
still cycles layouts. Any other session with no tmux
pane has nowhere to go, and `enter` does nothing. A click on a row selects it; a second click on the selected row
goes there, as `enter` does. The cursor follows its session, not a line, so it
stays put while rows reorder; if its session leaves the list, it lands on the
row that took its place. It is drawn only while the sidebar holds the focus.
`tt panel-render` draws no cursor, since nothing there moves it.

**`/` filters** the rows, in a sidebar: type, and only the sessions whose
name, tmux session or folder holds the text stay (the prompt takes every key
while it is open, `q` and `g` included). `enter` keeps the filter and gives the
keys back, `esc` clears it — and with a filter kept, `esc` clears it before it
closes anything. The header's counts stay the whole fleet's.

**`f` shows only a section, a state, or both**, in a sidebar. `f` opens a
picker: every section title leads with its key in brackets — `[1] claudish`,
`[1]`–`[9]`, `[0]`, then letters — and every word in the count strip shows its
letter the way the tabs do: `[N]EED`, `[R]UN`, `[I]DL`, `[u]???`. The keys are
drawn in the title's and the badge's own colours. In the picker a section's
key shows only that section, and a state's letter only the rows in that state,
in whatever sections hold them. A pick closes the picker; `f` again adds the
other filter — `f 2 f r` is the second section's running sessions — and the
same key again turns its filter off. `f` (or `enter`, or `esc`) closes it
without picking, and while it is open it takes every key but `j`/`k`, which
close it and move the cursor. `esc` outside it clears every filter, `/` text
included; **`esc` never closes the sidebar** — `q` does. The footer says what is shown (`f  claudish ·
RUN`). The header's counts stay the whole fleet's, with the state shown marked
`▸`. These filters combine with `/` too.

**The mouse.** A sidebar holds the mouse, so tmux passes it the clicks and the
wheel instead of putting the pane in copy mode: a click on a tab switches the
grouping, a click on a count in the strip (`NEED`, `RUN`, `IDL`, `???`) shows
only that state — a second click turns it off — a click on a row puts the
cursor there, and the wheel scrolls the list. tmux still focuses the pane on a
click; selecting text in the sidebar by dragging is what it gives up.

**Too narrow to draw.** Below 36 columns a sidebar cannot fit a row, so it
says `make me bigger` instead, over a `widen  w` button: `w` or a click on it
resizes the pane back to the sidebar's width (`@tt_obs_width`, 46 when unset).

- **project** is the repository, not the directory: every linked worktree —
  `.claude/worktrees/*` included — belongs to the repository it was checked
  out from. On this machine 69% of sessions run in a worktree, so grouping by
  directory makes 39 groups of mostly one session; by repository, 18–20. The
  repository is found by reading `.git` and the `commondir` it points to, the
  way herdr, Claude Code and Codex do — never by running git.
- **tmux** groups by the session's tmux session. Sessions with no pane come
  last: interactive ones under `no tmux`, background ones under `background`.
- **colour** follows the spectrum (red, orange, yellow, green, cyan, blue,
  purple, pink), with `no colour` last. It is as useful as your `/color` habit.
- Groups keep a **stable order** — by name — and rows inside a group are by
  name too. Lists that reshuffle as statuses change are the most common
  complaint against this kind of tool, and once the band holds what needs you
  nothing is gained by it.
- Groups of one session fold into **other**, a dim line under the row
  naming its group.

The choice is saved, once, for every surface: pressing `^g` in the TUI, or `g`,
`p`, `t`, `m` or `c` in any sidebar, regroups every sidebar and the TUI's panel — on their next
update, which is within a second while any session is running and a minute at
most when all are idle. It lives in `~/.local/state/tt/panel-mode` (honouring
`XDG_STATE_HOME`), one word.

### Reading a row

```
▌ ASK   3h13m    wire-ui                 248k
│ └┬┘   └─┬─┘    └─ name ─────────────┘  └┬─┘
│  │      │                               └ context, in tokens
│  │      └ runtime: since the session started
│  └ status code
└ gutter: the session's /color
```

- **The status badge** opens every row: a three-letter code on a badge of its
  own, 5 columns. One space after it, the session's **runtime** sits on a
  second badge, right-aligned in 7 columns, so every name starts in the same
  column whatever the status or the runtime.

  | code | status | code | status |
  |---|---|---|---|
  | `RUN` | busy | `IDL` | idle |
  | `WRK` | working | `SH` | shell |
  | `ASK` | waiting on you | `???` | unknown, or a status this tt has never seen |
  | `BLK` | blocked on a question | `END` | done |
  | | | `ERR` | failed |

  The runtime is how long the session has been running, from its start time,
  in at most five columns: `42s`, `17m`, `3h12m` under ten hours, `13h` under a
  day, `6d02h` under ten days, then `12d` (capped at `999d`). `—` means the
  session reported no start time. How long it has been *in* its status is on
  the why line of a row that needs you, below.
- **Loud or soft.** A session that needs you gets **loud** badges: the code and
  the runtime both on a solid fill with a contrasting ink — violet for `ASK`
  (and any other attention), coral for `BLK`. It follows the session, not the
  band: an attention session whose liveness is unknown sits in its home group
  and its badges are still loud. Every other code is **soft but coloured**: the
  status's hue on a sunk tint of the same hue — green for `RUN`/`WRK`, blue for
  `IDL`, teal for `SH`, grey for `???`, rose for `ERR`, grey for `END` — and
  its runtime sits on one neutral grey fill, so it reads as a
  value, not a second status. A solid fill on a row always means "act now";
  nothing else is solid. Every text colour clears 4.5:1 on its own fill, in
  both themes.
- **The name** is the session's name, else its short id. A session with a
  `/color` shows it on a soft badge of that colour — its tint on the hue sunk
  toward the page — and the `▌` gutter at the left edge in the full hue; one
  without a colour gets a blank gutter and a plain name. The name absorbs the
  slack and is the first thing cut: the badges and the count never are.
- **The context** is what the session is carrying right now, in tokens (`850`,
  `31k`, `248k`, `1.2M`), on a badge against the right edge, coloured by
  absolute size — **green below 200k**, **honey from 200k to 400k**, **red
  above 400k** — on a soft tint of that colour. Red turns into a solid fill
  only on a row that needs you, so a big context alone never outshouts what
  needs you. `—` means the transcript has no usage to count yet. The edges are
  `ctx_warn` and `ctx_alert` in `panel.conf`.
- **The bar** (`▰▰▱▱▱`, five cells) is not drawn by default. It is the count
  against the model's context window, in the count's colour: opus-5, sonnet-5
  and fable-5 have 1M, other `claude-*` models 200k, and past the window it is
  simply full. To add it back, end both rows with `{ctx}` instead of
  `{ctx:nobar}` in `panel.conf`:

  ```ini
  row      = "{gutter}{status:code} {runtime:badge}  {name:badge}{>}{ctx}"
  attn_row = "{gutter}{status:code} {runtime:badge}  {name:badge}{>}{ctx}"
  ```

  The name gives up six columns for it. A model tt has no window for — say a
  non-Claude one — gets the count and no bar (the bar's columns stay blank, so
  the counts still line up); `window` rules in `panel.conf` add or change
  windows.
- **The lines under a row** are dim and indented under the code, up to three
  (`text_lines` in `panel.conf`), and they are the first thing to give way
  when the panel is short. A row that needs you says how long it has waited
  and why — `waiting 13m · input-needed`, or what a blocked agent asked, from
  its start — and then, when there is room, its home group. Any other row shows the **last** three
  lines of its session's newest reply: its text only — never a tool call, a
  tool's output, a thinking block, a subagent's turn or anything you typed —
  sanitised, with markdown's backticks and bold stars dropped. A row whose
  liveness could not be established and has nothing to say says `live?` and
  names the advisory that explains why. A row folded into `other` names its
  group on one more line. The `/color` bar runs down through all of them. No tmux pane id and no background job id is drawn:
  they are internal handles, and the `{pane}`, `{kind}` and `{id}` tokens are
  there for a `panel.conf` that wants them; so are the model (`{model}`),
  subagents (`{sub}`) and API errors (`{apierr}`), on a `meta` line.
- **Prefer emoji, or the chip?** `{status:icon}` draws the emoji row of the
  second rebuild (🔄 ⏳ ⛔ 💤 🐚 ❓ ✅, with a plain age beside it),
  `{status:chip}` the filled two-part chip of the first (` WAIT ` ` 13m `), and
  `{status:badge}` code and runtime joined in one split badge (the third). Each
  is a two-line `panel.conf`, below.

The colour, the counts, the context and the model come from the transcripts,
which the panel reads through the same cached enrichment pass `tt sessions`
uses (an attached sidebar asks `tt observe` for them instead).

**The latest text stays in the panel.** Only numbers and short identifiers
ever leave a transcript through tt's observer — `tt sessions --json`, the
socket and `tt mcp` carry no message text, and that has not changed. The lines
under a row are read by the panel itself (the sidebar, the TUI's column and
`tt panel-render`, on this machine), from the last 64 KiB of the transcript
the session's record points to, and drawn straight into its rows; they are
never written back into a session record. Each transcript is read again only
when its size or mtime moved.

### When it does not fit

The panel is never taller than the terminal, and it never folds a group:
every group shows every one of its rows. The header and the footer stay; when
every row does not fit, what is drawn under the rows gives way first:

1. the blank line between groups, then the lines of latest text under the rows
   outside the band — three, two, one, none, every row at once;
2. band rows shorten, 4 lines → 3 → 2 (the row and the start of why it waits);
3. then the list scrolls (above): nothing is cut, nothing is summed up, and the
   corner says there is more;
4. and only if the band alone does not fit in the window at two lines do its
   rows drop to one — then the header and the footer give up their blank
   lines, and last of all everything but the tabs.

Two lines — the row and why it waits — is the band's floor while anything else
can still give way, and the band is never cut. A band row grows back only when
there is room to spare, so the panel does not flicker as sessions start and
stop asking for you. However short the surface, the panel keeps the grouping
you chose.

**Ended sessions are not shown.** A session that finished (`done`, `failed`)
or whose Claude process or pane is gone has nothing left to do, so the panel
leaves it out entirely; `tt sessions --json --all` still lists it.

The durations are computed **while you are looking**: the panel runs its own
clock, so "waiting 14m" becomes 15m, and a runtime of 3h12m becomes 3h13m, with
no new data and no server involvement. It wakes only when a displayed age or
runtime can change — every second for one under a minute, every minute after
that. Closing the panel cancels that
clock and the subscription behind it, so a closed panel costs nothing.

The panel reads the same state `tt sessions --json` reports, plus the tail of
each transcript for the latest text. It is read-only towards Claude Code: the
one file it writes is its own saved grouping, in tt's state directory, and it
starts nothing — including `tt observe`, which nothing in tt ever auto-starts.
It reads `panel.conf` and never writes it.

### How a row is drawn — `panel.conf`

Every row is drawn from a template, and the templates live in one optional
file:

```
$XDG_CONFIG_HOME/tt/panel.conf      when XDG_CONFIG_HOME is set (and absolute)
~/.config/tt/panel.conf             otherwise
```

With no file, the built-in format below is used and nothing is said. tt never
creates, fixes or rewrites the file — copy the default out of this README and
edit that. An edit applies on the panel's next update, which is within a second
while any session is running and a minute at most when all are idle: each
update costs one `stat`, and the file is read again only when its size or mtime
moved. No timer is added for it. `tt panel-render` (below) shows an edit at
once.

This is the built-in format, byte for byte (a test holds the two together):

```ini
# ~/.config/tt/panel.conf — how each row of the tt session panel is drawn.
# Edits apply on the panel's next update; `tt panel-render --check` lints this file.
# {token[:option[:option]]}   {{ and }} are literal braces   {>} is the spring (fills the gap)
# A token with nothing to say vanishes together with the separator before it.

row       = "{gutter}{status:code} {runtime:badge}  {name:badge}{>}{ctx:nobar}"
meta      = "     {group}"

attn_row  = "{gutter}{status:code} {runtime:badge}  {name:badge}{>}{ctx:nobar}"
attn_why  = "{waiting} · {why}"
attn_meta = "     {group}"

# Dim lines under a row, 0 to 3, indented under the code and carrying the row's
# /color bar: a band row's attn_why (why it waits), any other row the latest
# thing its session said. meta and attn_meta add one more line after them when
# there is room; the built-in ones name a band row's home group, and the real
# group of a row folded into "other". For more, try
# meta = "     {group} · {model:short} · {sub} · {apierr}".
text_lines = 3

# Context count colour: green below ctx_warn, honey up to ctx_alert, red above it.
ctx_warn  = 200k
ctx_alert = 400k

# Context window per model, for the bar ({ctx} draws the count and the bar,
# {ctxbar} the bar alone). The first rule whose pattern (path.Match, on the
# model id) matches wins; rules in your own file are tried before these.
window claude-opus-5*   = 1M
window claude-sonnet-5* = 1M
window claude-fable-5*  = 1M
window claude-*         = 200k

# {status:code} is the status code, {runtime:badge} the time since start. To add
# the context bar back, end both rows with {ctx} instead of {ctx:nobar}.
# {status:badge} joins code and runtime in one split badge. {status:icon} draws
# an emoji instead, 1 or 2 columns each (no U+FE0F, no joined emoji), set per
# status with icon_busy icon_work icon_waiting icon_blocked icon_idle icon_shell
# icon_unclear icon_ended, for example icon_idle = "·". {status:chip} is the
# first rebuild's chip.
```

**The keys.** Every one is optional; a key you leave out keeps its default, so
a file can change just `meta`.

| key | draws | may be `""` |
|---|---|---|
| `row` | the first line of every row outside the band | no |
| `meta` | one line after the row's latest text, when there is room (built in: the group of a row folded into `other`) | yes — no such line |
| `attn_row` | the first line of a NEEDS YOU row | no |
| `attn_why` | why it waits: the lines under a band row, wrapped to `text_lines` | yes |
| `attn_meta` | one line after a band row's why, when a line is left over (built in: its home group) | yes |
| `text_lines` | how many dim lines are drawn under a row, `0` to `3` (default `3`) | — |
| `ctx_warn` | where the context count turns from green to honey (default `200k`) | — |
| `ctx_alert` | where it turns red (default `400k`) | — |
| `window <pattern>` | the context window of the models whose id matches the pattern, for the bar; any number of rules, the first match wins, yours before the built-in four | — |
| `icon_busy`, `icon_work`, `icon_waiting`, `icon_blocked`, `icon_idle`, `icon_shell`, `icon_unclear`, `icon_ended` | the status icon `{status:icon}` draws (defaults 🔄 🔄 ⏳ ⛔ 💤 🐚 ❓ ✅) | — |

A line is `key = "template"` or `key = 'template'`: double quotes take `\"`,
`\\` and `\t`, single quotes are raw. A `\t` is drawn as four spaces, not a
raw TAB, because a TAB has no fixed width on screen and the row's width math
needs one. `#` starts a comment outside quotes. A
size is a number with an optional `k` or `m` — `150k`, `1.5m`, `400000`. The
counts at the edges are honey: green is below `ctx_warn`, red is above
`ctx_alert`. Both must lie between 1k and 100M and `ctx_warn` must be the
smaller. A window rule is `window <pattern> = <size>`, the pattern unquoted
(no spaces) in Go's `path.Match` syntax — `*`, `?`, `[a-z]` — matched against
the whole model id, and the size between 1k and 100M: `window gpt-* = 400k`.
An icon is a quoted string one or two columns wide — an emoji, or a
plain glyph such as `"·"`, which is padded to two columns so names still line
up. A string of any other width, or one holding U+FE0F (the emoji variation
selector) or U+200D (the joiner of emoji sequences such as 👩‍💻), is a soft
problem: the note names it, and that status keeps its built-in icon. A key
given twice keeps the later one. The file may be up to 64 KiB, a line up to
1 KiB.

**The template.** Literal text, and tokens in braces: `{name}`,
`{name:colour}`, `{model:short:drop=1}`. `{{` and `}}` are literal braces.

- **`{>}` is the spring.** It fills whatever the line has spare, so what follows
  it sits against the right edge — that is how the ctx badges line up. One per
  template.
- **Empty tokens vanish, and take their separator with them.** A session with
  no pane and no subagents draws `input-needed`, not `input-needed ·  · `. A
  literal between two tokens is drawn only when both sides have something to
  say, a literal before the first token only when some token is drawn, one
  after the last only when the last is. The spring splits a line in two and
  each half is tidied on its own. A line whose every token is empty is not
  drawn at all, so an idle session with nothing to report stays one line.
- **Brackets belong to the token they touch.** An opening `(`, `[` or `{{`
  right before a token, and the closing one right after it, are drawn exactly
  when that token is: `{wait} ({pane})` draws `input-needed` with no pane,
  `(%9)` with no wait, and `input-needed (%9)` with both — never a stray `%9)`.
  `{a} ({b}) · {c}` without b draws `a · c`.
- **Nothing is ever wider than the panel.** When a line is too long, the
  shrinkable tokens give way first: the name (it keeps at least 12 columns),
  then the group, project and tmux labels (6), then the model (4). Then tokens
  with a `drop=` rank are removed, lowest rank first, separator and all, then
  names are cut below their minimum, and as a last resort the line itself is
  cut. Widths are display columns: `名前` is four and a cut never splits one.
- **Session text is sanitised** before it is drawn: tabs, newlines and every
  control character become a space, runs of spaces collapse. A name or a need
  cannot move the cursor, set a title or break the layout.
- **The lines under a row are wrapped by the panel,** not by the template:
  `attn_why` is drawn as plain dim text, word-wrapped to `text_lines` lines
  from its start, with `…` where it was cut. (`{why:wrap=2}` is still accepted,
  and draws on one line.)

**The tokens.** Every token also takes the common options: `plain` (no
colour), `dim` (the dim note colour), `min=N` and `max=N` (columns),
`shrink=N` (0 is rigid; lower ranks give way first) and `drop=N` (0 never
drops; lower ranks go first).

| token | draws | its own options | empty when |
|---|---|---|---|
| `{>}` | the spring | — | never |
| `gutter` | `▌` in the session's /color, else a blank. Always one column | `char=X` (another one-cell glyph) | never |
| `status` | the status. Bare `{status}` is the chip of the first rebuild: the word and the age on two fills, loud (bold, the age on a solid violet or coral fill) when the session needs you | `code` (the built-in row's: ` RUN ` on a badge of its own, 5 columns; solid violet or coral when the session needs you, a soft tint of the status's hue otherwise), `badge` (` RUN ` and the runtime on two fills, 12 columns; loud when the session needs you, soft in the status's hue otherwise), `icon` (an emoji, 2 columns, no colour or fill), `chip` (the default), `quiet` (the chip, code or badge never loud), `word` (the whole word, no chip; bold coral when the session needs you), `noage` (the chip's label half only) | never |
| `runtime` | how long the session has been running, `3h12m`, as slate text | `badge` (the built-in row's: on a badge, right-aligned in 7 columns, `—` with no start time; solid violet or coral when the session needs you, a neutral grey fill otherwise), `quiet` (the badge never loud) | no start time (never with `badge`) |
| `age` | time in this status, `2h05m`, as plain text: slate, or bold violet (coral for blocked) when the session needs you | `short` (`2h`, right-aligned in 3 columns), `quiet` (always slate) | never (`—` when unknown) |
| `waiting` | `waiting 13m`, the time in this status, in the loud hue — it opens the built-in why line | — | the session does not need you, or the time is unknown |
| `name` | the session's name, else its short id | `badge` (on a soft badge of its /color, a lead cell always; the built-in), `colour` (alias `color`, `tint`: the soft session tint as text), `bold` | never |
| `group` | the row's home group, on a band row or one folded into "other" | — | no label, or the name already says it |
| `project` | the repository the session's directory belongs to | — | no directory |
| `tmux` | its tmux session | — | no tmux pane |
| `pane` | its pane, `%107` — an internal id, not in the built-in rows | `full` (the whole target) | no tmux pane |
| `colour` (alias `color`) | the /color's name, in its hue | — | no colour |
| `ctx` | the count badge (6 columns; `—` when unknown), a gap and the bar against the model's window: 12 columns, the bar's blank when the window is not known | `nobar` (the badge alone — the built-in row's), `plain` (the count in its colour, no fill, no bar), `hide` (empty when unknown) | only with `hide` |
| `ctxbar` | the bar alone, `▰▰▱▱▱` | — | no count, or no window for the model |
| `tokens` | the same count, bare, `136k` | — | unknown |
| `model` | the model id, `claude-opus-5-5` | `short` (`opus-5-5`) | no model |
| `compact` | compactions seen in the transcript's tail, `⟳2` | — | none |
| `wait` | the wait reason, `input-needed` | — | not waiting on anything |
| `needs` | what a blocked agent needs | `wrap[=N]` | not blocked, or nothing said |
| `why` | `needs` when there is one, else `wait` | `wrap[=N]` | neither |
| `sub` | subagents, `2 sub` | `n` (the number only) | none, or not known |
| `apierr` | API errors in the transcript, `2 api err` | `n` | none |
| `live` | `live? <advisory>` when liveness is unknown | — | liveness known |
| `kind` | `bg` for a background agent | — | interactive |
| `id` | the background agent's short id, `be9663df` — an internal id, not in the built-in rows | — | none |

**When the file is wrong,** the panel says so on a dim line of its own above
the footer, naming the first problem as `panel.conf:line:column`. Two kinds:

- **Fatal** — an unclosed `{`, a stray `}`, a second `{>}`, an unterminated
  string, an unknown key, a bad size, `ctx_warn` not below `ctx_alert`, a
  `window` rule with no pattern, a bad pattern or a bad size, an empty
  `row`: the whole file is set aside and the built-in format draws, with
  `— using the built-in format` on the note.
- **Soft** — an unknown token or option, a number out of range, a key given
  twice, an icon of the wrong width or with a selector or joiner in it: the
  file is used, and the offending token draws in place, in red, as `{foo?}`
  (an unusable icon draws its built-in one instead).

A one-line panel, for a short window — the why moves up beside the name, the
lines under a row go, and the ctx edges move out:

```ini
# One line per session: no lines under a row, and the why beside the name.
row        = "{gutter}{status:code} {runtime:badge}  {name:badge}{>}{model:short:drop=1} {ctx:nobar}"
meta       = ""

attn_row   = "{gutter}{status:code} {runtime:badge}  {name:badge:bold} · {why}{>}{ctx:nobar}"
attn_why   = ""
attn_meta  = ""
text_lines = 0

# Long sessions are the norm here: warn later.
ctx_warn   = 300k
ctx_alert  = 600k
```

```
   [P]roject │ S[t]atus │ T[m]ux │ [C]olour
──━━━━━━━━━━━─────────────────────────────────
░░░░▒▒▒▓▓  2  NEED  1  RUN  2  IDL  ▓▓▒▒▒▒░░░░


───────────────  NEEDS YOU  2  ───────────────
▌ BLK   3h41m    migrate-db  · confirm…  612k
▌ ASK   3h13m    wire-ui  · input-need…  248k

─────────────────  beta  2  ──────────────────
  IDL   3h14m    badge-tone    opus-5-5  431k
▌ RUN   3h02m    chip-fit      opus-5-5  194k

─────────────────  gamma  1  ─────────────────
  IDL   6h00m    no-usage-yet               —


═══  j/k  move  ↵  go  /  filter  f  only  ═══
```

`{model:short:drop=1}` is the first thing to go when a name needs the room.

A window for a model tt does not know, the emoji row of the second rebuild,
the split badge of the third, or the chip of the first — each is a few lines:

```ini
# A local model with a 128k window gets a bar too; this rule is tried first.
window gpt-oss-* = 128k

# The emoji row: an icon and the time in the status, no badge.
row      = "{gutter}{status:icon} {age:short}  {name:colour}{>}{ctx}"
attn_row = "{gutter}{status:icon} {age:short}  {name:colour}{>}{ctx}"
# Plain glyphs instead of emoji: one column each, padded to two.
icon_idle = "○"
icon_busy = "●"

# Or the split badge of the third: code and runtime on one badge, with the bar:
# row      = "{gutter}{status:badge} {name:badge}{>}{ctx}"
# attn_row = "{gutter}{status:badge} {name:badge}{>}{ctx}"

# Or the filled status chip of the first rebuild, age inside it:
# row      = "{gutter}{status:chip} {name:colour}{>}{ctx:nobar}"
# attn_row = "{gutter}{status:chip} {name:colour}{>}{ctx:nobar}"
```

### Previewing it — `tt panel-render`

`tt panel-render` draws the panel once, to stdout, as ANSI: the same renderer,
the same `panel.conf`, no terminal and no tmux. It is how to see a
`panel.conf` edit before a sidebar picks it up, and how the screenshots of the
panel are made.

The quickest look, from the repo root, needs no install at all: `task tt:demo`
builds tt from this checkout into a temp dir, snapshots this machine's sessions
and draws every grouping in turn (`MODE=status`, `WIDTH=60`,
`HEIGHT=40` and `CONFIG=path` narrow it down; the theme follows `$TERM_THEME`).

```sh
tt sessions --json --all > snap.json                  # save what the machine sees now
tt panel-render --snapshot=snap.json                  # draw it as a 46×59 sidebar
tt panel-render --snapshot=snap.json --theme=light --mode=status --height=40
tt panel-render --snapshot=- --config=./try.conf < snap.json
tt panel-render --live --profile=ascii                # ask the running tt observe instead
tt panel-render --check                               # lint panel.conf
```

It is deterministic on purpose: the theme and the colour profile are **forced**
from flags rather than asked of a terminal (`--theme` defaults to
`$TERM_THEME`, else dark; `--profile` to truecolor, with `256`, `16` and
`ascii` for the other cases), and ages are drawn against the snapshot's own
`observedAt` unless `--now` says otherwise. The same snapshot, flags and file
give the same bytes. `--width` and `--height` default to the sidebar's 46×59,
and the panel gives way to fit them exactly as it does on screen (`--height=0`
draws every line). `--mode` and `--surface=pane|tui` do what `g` and the two
surfaces do; `--ended` is still accepted and does nothing. `--live` dials the running `tt observe` for one
enriched snapshot, the way a sidebar does; with no server it says so and
starts nothing.

`--check` draws nothing. It prints every problem in the file as
`path:line:column: message` — one an editor can jump to — then a verdict, and
exits `0` when the panel will use the file: it is clean, absent, or has only
soft problems (each draws in place as `{token?}`). It exits `1` on any fatal
problem, which means the panel is drawing the built-in format. The input and
drawing flags (`--snapshot`, `--width`, `--theme` and the rest) are accepted
and ignored, so `--check` can be added to any `panel-render` command line.
An unknown flag is still a usage error, exit `2`.

### Where the context count comes from

The ctx badge is `input + cache_creation + cache_read` of the newest
main-thread assistant message in the transcript's last 200 KB — the tail tt
already reads for the colour, so it costs no extra I/O. Output tokens are not
context and are not counted; synthetic and sidechain records are skipped. After
a compaction with no newer message, the count is the compaction's own
`postTokens`. Only numbers and a bounded model id leave the transcript, never a
line of content.

**It is as fresh as the session's last registry write.** tt watches the
session registry, not the interactive transcripts, so a session's count is
re-read when its registry record is written rather than on every assistant
message. During one long turn the badge can lag the transcript; it catches up
the next time the record is written, and at the latest when the turn ends and
the status changes.

**After installing a new tt, restart `tt observe`** (`tt observe --stop`, then
start it again, or press the sidebar's button). Sidebars draw what the running
server sends them, and a server started from an older binary sends no context
count, so every badge reads `—` until it runs the new one. Old and new builds
talk to each other safely in both directions; they just cannot say what the
older one never read.

### When the answers look wrong — `tt sessions --doctor`

Everything else in tt degrades quietly on purpose: an unknown status is passed
through, a missing directory is a fact rather than an error, and a source that
cannot be read turns liveness into `null` instead of failing your command. That
is right for something you run forty times a day, and it has one failure mode —
a format changes under you and every command still exits 0.

The doctor is the counterweight, and the only surface here whose job is to
shout:

```
$ tt sessions --doctor
tt doctor — 2026-09-18T11:00:12.026Z
22 checks in 5005 ms: 18 ok, 2 warn, 0 fail, 2 skipped

[WARN] registry-keys  (§0.1, §9.5, every interactive registry record still carries the 19 keys this build reads)
        the format grew keys this build does not read: parkedJobId — nothing breaks,
        but the record now says more than tt reports
        · 34 records under /home/user/.claude/sessions
        · NEW optional key "parkedJobId" on 1/34 records
```

It reads one live example of each format Claude Code writes — the session
registry, the job records and their timelines, a transcript tail, the subagent
meta files, `claude agents --json` — and reports per assumption whether it still
holds, with the numbers it decided on. The verdicts are deliberately four:

| | |
|---|---|
| `ok` | the assumption holds |
| `warn` | it does not, and a feature **degrades** |
| `fail` | it does not, and a feature is **broken** |
| `skip` | the check could not run at all — which is never reported as a pass |

Exit codes are `3` for warn and `4` for fail, distinct from `tt sessions`'s `1`
(nothing could be read) and `2` (usage), so a script can gate on it. `--json`
gives the same report as a document. It is read-only, it takes a few hundred
milliseconds, and it spawns `claude agents --json` exactly once. The same
diagnosis is available to a model as the MCP `doctor` tool.

### What the MCP server hands other agents — `tt mcp`

`tt mcp` serves the same snapshot to an MCP client over stdio — four read-only
tools, registered once:

```
claude mcp add tt -- tt mcp
```

Its audience is the thing to keep in mind: every Claude session on this machine
can reach it, so anything it publishes about one session is published to all the
others. Transcripts were never in scope — a transcript yields only `path`,
`sizeBytes`, `agentColor`, an API-error count, a subagent count, the context
token count, a bounded model id and a compaction count, never a line of
content — but two **job** fields were: `needs` and `detail`, the
free-text strings a background agent writes about what it is stuck on. A
measured live value of `needs` was:

```
open new terminal and test `ck` command; report if it works or breaks
```

which is an instruction you typed, restated. It comes out of the same job record
whose `intent`, `output` and `suggestedReply` tt refuses to read at all.

So the line is drawn by **audience rather than by field**:

| surface | `needs` / `detail` |
|---|---|
| `tt sessions`, `tt sessions --json`, the `^o` panel, `tt observe --pane`, the socket | in full — it is your own machine, and you are the reader |
| `tt mcp` | `[withheld: 63 chars]` — presence and length, no sentence |

An agent asking "is anyone blocked, and on what?" still gets the answer it acts
on: status, `waitReason`, how long it has been blocked, liveness, the tmux pane,
the cwd, and the fact that a need exists and how long it is. What it does not
get is the sentence you wrote.

One flag reverses it, for the whole server:

```
claude mcp add tt -- tt mcp --publish-job-text
```

`tt mcp` prints which mode is in force on startup, and the choice affects no
other surface.

## The observer in tmux — a sidebar per window, and a status-line counter

Two surfaces, both opt-in, both fed by one `tt observe` server:

- **a sidebar**: `tt observe --pane`, the same rows as the `^o` panel in a
  46-column pane at the right edge of a window. Toggle it by hand in one window,
  or let `@tt_obs_scope` put one in every window automatically;
- **a status-line counter**: `3 waiting · 1 error` in every window of every
  client. It costs no pane and no process, and it is empty when nothing needs
  you.

tt writes nothing to your `~/.tmux.conf`. Everything below is lines you add.

### 1. Run the observer

```
tt observe
```

It runs until you stop it (`tt observe --stop`). Run it in a pane of its own:
tmux-claude-continuity brings a running command back after a reboot, so it
returns with the rest of your workspace.

**Or press the button.** With no server, a sidebar says `observer not running`
and draws a start button under it. `s` presses it, so does `enter`, and so does
a click on it. The server it starts is
detached (`setsid`) and is the machine's, not that pane's: it keeps running when
the sidebar that started it is closed, and it serves every other window and the
status line. A refusal — usually another server already listening — is reported
in the pane: the ordinary one is "one is already listening", read from the pid
file rather than from the child, whose stderr goes to /dev/null on purpose —
a pipe held by the sidebar would kill the server with SIGPIPE the first time it
wrote a line after that pane closed.

A waiting sidebar is not polling: it waits on a directory watch of the
observer's socket directory, and attaches the moment the server binds. When the
server stops it says so, draws the button again, and waits.

### 2. The tmux block

moox loads this block for you: it is `share/moox/conf/tt.conf`, sourced from
`active.conf` on every server that takes part in moox continuity. Below it is
byte for byte (a test fails if the two ever differ), for reading, or for adding
to a tmux config that does not use moox. `@tt_obs_scope` ships as `all`: every
window wide enough gets a sidebar the first time you look at it. Set it to
`off` — or press `prefix C-o` once — if you would rather have none.

```tmux
# >>> tt observer >>>
# The tt session observer as a tmux sidebar: `tt observe --pane` in a
# 46-column pane at the right edge of a window. Documented in the moox README
# (tt section), and the tt gate sources THIS block, byte for byte, into an
# isolated tmux server (-f /dev/null, private -L) and tests it there. moox
# sources this file from active.conf, so only a server that takes part in moox
# continuity gets the sidebar hooks (D7).
#
# Where the sidebar appears automatically. prefix C-o writes this option, for
# the whole server: the panel is ONE thing, and the toggle turns it on or off
# everywhere rather than in the window you happen to be in.
#   off       nowhere — prefix C-o turns it back on
#   all       every window, the first time it is shown wide enough
#   sessions  only windows of the sessions named, exactly, in @tt_obs_sessions
#             (space-separated: set -g @tt_obs_sessions "dotfiles tt")
set -g @tt_obs_scope all
set -g @tt_obs_sessions ""
# Columns of the sidebar, and columns your own panes keep: a window narrower
# than @tt_obs_width + 1 + @tt_obs_min_width (127 by default) gets NO sidebar,
# and loses its automatic one if it shrinks below that. tmux treats a split
# width as a request and squeezes the other pane instead of refusing, so this
# check is the only thing standing between a narrow window and a 1-column pane.
set -g @tt_obs_width 46
set -g @tt_obs_min_width 80

# The predicates, as formats expanded with E: when a hook fires. Nothing here
# forks a shell: if-shell -F evaluates a format and runs no command.
#   is       this pane is an observer: its renderer claimed it (@tt_obs), or it
#            was split to run one and the renderer has not started yet — tmux
#            records the start command synchronously, which is what makes two
#            hooks firing for one event create ONE pane, not two
#   auto     an observer the hooks created (--auto), the only kind they remove
#   present  some pane in this window is an observer
#   fits     the window is wide enough (see above)
#   in_scope @tt_obs_scope covers this window's session. For `sessions` the
#            session's name must EQUAL one of @tt_obs_sessions' space-separated
#            words. tmux has no exact list-membership test, so it is a glob
#            with the name escaped by #{q:} (measured: * ? [ and space each get
#            a backslash): `al*` is not "alpha", `w[1]` matches itself, and a
#            name containing a space is never in scope — it cannot be listed
#            in a space-separated list without also matching two words
#   want     in scope and wide enough. There is no per-window dismissal: the
#            toggle is global (see prefix C-o below), so an automatic sidebar
#            you kill by hand comes back the next time the window is shown
#   hold     a restore may be running: the moox daemon says so
#            (@moox-restoring, set for the whole restore, RR9), or the server
#            holds exactly one pane, or a lone one-pane session named "0" (the
#            bootstrap shapes of a cold start before the daemon has set the
#            flag). An observer created then would land before the saved
#            layouts are applied and make select-layout fail for the window.
set -g @tt_obs_is       '#{&&:#{!=:#{pane_dead},1},#{||:#{==:#{@tt_obs},1},#{m:*tt observe --pane*,#{pane_start_command}}}}'
set -g @tt_obs_auto     '#{&&:#{!=:#{pane_dead},1},#{m:*tt observe --pane --auto*,#{pane_start_command}}}'
set -g @tt_obs_present  '#{P:#{?#{E:@tt_obs_is},1,}}'
set -g @tt_obs_fits     '#{e|>=:#{window_width},#{e|+:#{@tt_obs_width},#{e|+:#{@tt_obs_min_width},1}}}'
set -g @tt_obs_in_scope '#{||:#{==:#{@tt_obs_scope},all},#{&&:#{==:#{@tt_obs_scope},sessions},#{&&:#{!:#{m:* *,#{session_name}}},#{m:* #{q:session_name} *, #{@tt_obs_sessions} }}}}'
set -g @tt_obs_want     '#{&&:#{E:@tt_obs_in_scope},#{E:@tt_obs_fits}}'
set -g @tt_obs_hold     '#{||:#{!=:#{@moox-restoring},},#{||:#{==:#{S:#{W:#{P:x}}},x},#{S:#{?#{&&:#{==:#{session_name},0},#{==:#{W:#{P:x}},x}},1,}}}}'

# One reconcile, run by the hooks below and runnable by hand
# (`tmux tt-obs-reconcile`): give this window an automatic observer if it wants
# one and has none, or remove the automatic one if it no longer wants it.
#   -l '#{@tt_obs_width}'  split-window expands a format in -l (measured, tmux
#                          3.7c). Unquoted, `#` starts a COMMENT and eats the
#                          rest of the line, which is what broke the first
#                          design ("-l expects an argument").
#   /bin/sh -c '…; exit'   the renderer runs as a CHILD of a small shell
#                          (`; exit` keeps sh from exec-ing it away, and ends
#                          the pane when tt exits); a save sees `tt observe
#                          --pane` as the pane's command and never restores it.
#   \${MOOX_BIN:+…}tt      the tt binary moox ships: split-window does not
#                          format-expand its command, so the path is the
#                          gate-exported $MOOX_BIN, kept literal here (`\$`) for
#                          the pane's /bin/sh; with no MOOX_BIN, `tt` on PATH.
#   {right}                a full-height -f split is the rightmost pane, so it
#                          names the automatic observer; kill-pane runs only if
#                          that pane's start command says it is one.
set -s command-alias[90] 'tt-obs-reconcile=if-shell -F "#{?#{E:@tt_obs_hold},0,1}" { if-shell -F "#{E:@tt_obs_want}" { if-shell -F "#{==:#{E:@tt_obs_present},}" { split-window -h -f -d -l "#{@tt_obs_width}" /bin/sh -c "\${MOOX_BIN:+\$MOOX_BIN/}tt observe --pane --auto; exit" } } { if-shell -F -t ":.{right}" "#{E:@tt_obs_auto}" { kill-pane -t ":.{right}" } } }'

# A sidebar is a VIEW, and a view must never be the reason a window stays open.
# When the last pane of your own work ends, the window has to close exactly as
# it would have without a sidebar in it — that it did not was the second half of
# the reported bug.
#
# MEASURED on tmux 3.7c, against an isolated server (-f /dev/null, private -L):
#   pane-exited          fires AFTER the pane is gone, and its FORMAT context is
#                        the surviving pane — but `:.` in a COMMAND resolves to
#                        the session's CURRENT window, so a pane that exits in a
#                        window nobody is looking at is invisible to it (probe:
#                        two such windows kept their sidebar)
#   window-pane-changed  fires for the affected window in every case measured —
#                        `exit` and kill-pane, current window and not — and `:.`
#                        resolves THERE. It is the one that does the work
#   window-layout-changed / after-kill-pane
#                        both resolve `:.` to the session's current window, so
#                        neither can see the window whose pane just went
# Both are hooked: window-pane-changed misses the case where the pane that ends
# is not the active one, and pane-exited covers it whenever the window is
# current. Between them the only surviving hole is a window you are NOT looking
# at whose focus was parked in the sidebar.
#
# The predicate is what makes a mis-resolved `:.` harmless: the pane it is about
# to kill must be an AUTOMATIC observer itself, and every pane in that pane's
# window must be an observer of some kind, so the worst a wrong target can do is
# nothing at all. Measured: a current window holding two of the user's panes
# kept both while sidebar-only windows elsewhere were closed.
#
# @tt_obs_moving is the one exception, and it is the sidebar's own flag: while
# it re-docks itself after a layout it is parked for a moment in a window of its
# own (tt observe --pane sets the flag, breaks out, re-applies the layout to the
# panes left behind, and joins back). A guard whose job is "a window with
# nothing but a sidebar in it should close" would close exactly that window.
#
# `auto`, not `is`, on the pane being killed: an automatic sidebar only ever
# exists BESIDE your work, so a window with nothing left but one is finished. A
# manual `tt observe --pane` may well BE the point of its window (a window whose
# only pane is one survives, measured), and the same rule already governs which
# observers the reconcile retires.
set -s command-alias[91] 'tt-obs-lastpane=if-shell -F -t ":.{right}" "#{&&:#{!=:#{@tt_obs_moving},1},#{&&:#{E:@tt_obs_auto},#{==:#{P:#{?#{E:@tt_obs_is},,x}},}}}" { kill-pane -t ":.{right}" }'

# The toggle: turn the panel on or off EVERYWHERE. The panel is one thing, so
# it writes the global @tt_obs_scope and acts on this window at once; every
# other window follows through the same reconcile the next time it is shown,
# switched to or resized. Closing the sidebar in one window and finding it
# still sitting in the other 35 was the reported bug this replaces.
#
# @tt_obs_scope_prev remembers a `sessions` scope across an off/on pair, so the
# toggle never silently rewrites the setting to `all` (set -gF expands the
# format in the value — measured, 3.7c; plain `set` would store it literally).
# Turning it on obeys the width rule like the hooks do, so a narrow window
# still gets nothing.
#
# It is an alias because TWO keys run it. bind-key expands a command alias at
# BIND time (measured: list-keys shows the resolved commands), so one
# definition gives both keys the same command list and neither can drift.
set -s command-alias[92] 'tt-obs-toggle=if-shell -F "#{==:#{@tt_obs_scope},off}" { set-option -gF @tt_obs_scope "#{?@tt_obs_scope_prev,#{@tt_obs_scope_prev},all}" ; if-shell -F "#{&&:#{E:@tt_obs_want},#{==:#{E:@tt_obs_present},}}" { split-window -h -f -d -l "#{@tt_obs_width}" /bin/sh -c "\${MOOX_BIN:+\$MOOX_BIN/}tt observe --pane --auto; exit" } } { set-option -gF @tt_obs_scope_prev "#{@tt_obs_scope}" ; set-option -g @tt_obs_scope off ; if-shell -F -t ":.{right}" "#{E:@tt_obs_is}" { kill-pane -t ":.{right}" } }'

# A layout was applied: say so, in an option the sidebar reads.
#
# The sidebar cannot tell a layout from a window resize by size alone, and the
# difference decides what it does: a window that grew just needs the sidebar's
# width back, while a layout needs the user's OWN panes laid out among
# themselves and the sidebar re-docked beside them. (Measured: after
# even-horizontal the sidebar is already at the right edge, merely too wide, so
# "am I out of place" does not catch it — the case that shipped 57/46/115.)
#
# This is the one thing only tmux knows, so tmux says it. The hooks write a
# window option; `tt observe --pane` reads it on its next redraw, does the work
# and clears it. No shell, no fork: set-option is the whole hook.
# It only writes the option when nobody else holds it, and that guard is what
# keeps the sidebar from re-docking twice: the re-dock applies a layout itself,
# which fires this very hook, so before it starts it claims the option with a
# value of its own and clears it at the end. Measured — without the claim the
# gate saw the sidebar leave the window a second time on every layout.
set -s command-alias[93] 'tt-obs-relayout=if-shell -F "#{==:#{@tt_obs_relayout},}" { set-option -w @tt_obs_relayout 1 }'

# prefix Space cycles layouts, and `next-layout` is the ONE layout command with
# no after-hook (measured: "invalid option: after-next-layout", while
# after-select-layout is valid and fires). `select-layout -n` does the same
# thing and does fire it, so that is what the key runs.
# In a tt popup (a throwaway session named tt-popup-<pid>, cmd/tt
# panel_cursor.go) it closes the popup instead: that session shares the real
# windows, so cycling there would re-lay the window behind it.
set -s command-alias[94] 'tt-popup-space=if-shell -F "#{m:tt-popup-*,#{session_name}}" { detach-client } { select-layout -n }'
bind-key Space tt-popup-space

# prefix E spreads the panes evenly (select-layout -E, tmux's default binding,
# kept). It is not a preset, so the after-select-layout hook cannot tell it
# from one, and the sidebar's re-dock would re-apply the last preset instead
# and hand the sidebar's columns to its neighbour (47 / 93 / 46). The flag
# tells the re-dock to spread again over the user's panes; tt clears it.
bind-key E { set-option -w @tt_obs_spread 1 ; select-layout -E }

# prefix g — "glance". On this config every other lowercase letter is taken
# (measured: a-z are all bound except e and g), so g is the short key and C-o
# stays for the fingers that learned it.
bind-key g   tt-obs-toggle
bind-key C-o tt-obs-toggle

# The creation hooks fire when a window is SHOWN (switched to, attached to,
# resized), never when one is merely created. That is deliberate: a restore
# creates every window detached and applies the saved layout, and a pane
# appearing in the middle of that would break it. A window gets its observer
# the first time you look at it. Slot [104] is tt's fixed index on every hook
# name (moox's slot table, active.conf): it replaces itself on a re-source and
# never touches another subsystem's entry on the same hook.
set-hook -g 'session-window-changed[104]' tt-obs-reconcile
set-hook -g 'client-session-changed[104]' tt-obs-reconcile
set-hook -g 'client-attached[104]'        tt-obs-reconcile
set-hook -g 'window-resized[104]'         tt-obs-reconcile
set-hook -g 'pane-exited[104]'            tt-obs-lastpane
set-hook -g 'window-pane-changed[104]'    tt-obs-lastpane
set-hook -g 'after-select-layout[104]'    tt-obs-relayout
# <<< tt observer <<<
```

A sidebar is never restored: the moox save sees `tt observe --pane` as the
pane's command and leaves it out, and the hooks put a fresh one in each window.

And the counter's placeholder at the front of `status-right` (moox's
`conf/status.conf` starts with it; in a config of your own, keep what you
already have after it):

```tmux
set -g status-right "#{?@tt_status,#{E:@tt_status} ,}…your existing status-right…"
```

Then reload (`prefix r`). The hooks sit in tt's fixed slot `[104]`, so
sourcing the file again replaces them instead of adding them twice. `tt observe` reads the
status-line placeholder once, at startup: restart it after changing
`status-right`.

### 3. Choose a scope

| `@tt_obs_scope` | automatic sidebar |
|---|---|
| `off` | none. Toggle one by hand (below). |
| `all` | every window, the first time it is shown wide enough |
| `sessions` | only windows of the sessions named in `@tt_obs_sessions` (space-separated, matched exactly) |

Edit the line and re-source, or try it live with `tmux set -g @tt_obs_scope
all`.

**Naming sessions.** `set -g @tt_obs_sessions "dotfiles tt"` lists two
sessions, separated by spaces. Each name must equal a session's name exactly
and case-sensitively. Glob characters are not wildcards: `work-*` names a
session called `work-*`, not every session starting `work-`, and a session
called `al*` or `alph?` is not in scope when `alpha` is listed. tmux has no
exact list-membership test, so the block matches with a glob and escapes the
session's name with `#{q:}` first (tmux 3.7c backslash-escapes `*`, `?`, `[`
and space, measured). A session whose name contains a space cannot be listed,
because it would also match two neighbouring names. It is never in scope, so
split one in there by hand: `tt observe --pane`.

**When a sidebar appears.** The hooks fire when a window is *shown*: you switch
to it, attach to it, or its size changes. A new window, a new session, a window
you linked or moved in, and a client attaching all end with exactly one sidebar,
however many hooks the event fires. A window gets its sidebar the first time
you look at it; one you never look at costs nothing. They deliberately do not
fire when a window is merely created: tmux-resurrect creates every window
detached and splits in its saved panes by index, and a pane that appeared
halfway through would take a saved pane's slot.

**The sidebar keeps its place.** Two things move it, and tmux has no concept
that would stop either: there is no pinned pane, no layout that excludes one,
and no option that says "leave this pane alone".

- A window that **grows** scales every pane proportionally — 46 columns in a
  120-column window is 92 at 260.
- A **layout** (`prefix Space`, `prefix E`, `prefix M-1`…`M-7`) lays out every
  pane in the window, sidebar included, so it lands in the grid with your work.

So the renderer puts itself back, and the two cases need different repairs.

**A window that grew** just took columns, so the sidebar resizes and everyone
keeps their proportions.

**A layout** arranged your panes around a sidebar that should not have been in
the arrangement, so the sidebar leaves and the layout is applied again without
it:

```
set -w @tt_obs_relayout 2   claim the marker, so this does not trigger itself
set -p @tt_obs_moving 1     the last-pane guard must not close the window it parks in
break-pane -d               out of the window, into one of its own
select-layout               re-apply the window's last preset to the panes left
join-pane -h -f -l 46       back as the full-height column at the right edge
set -wu / -pu               release both
```

That is the difference between `57 / 46 / 115` and `86 / 46 / 86` for an
even-horizontal window: joining alone gives the columns the sidebar drops to
whichever pane it lands beside. Four separate tmux calls, not one command list,
because a failing command aborts the rest of a list and the join must always
run.

**tmux tells the sidebar which case it is**, because it cannot be inferred from
a size: `after-select-layout` sets `@tt_obs_relayout` on the window (`prefix
Space` runs `select-layout -n` rather than `next-layout`, since that is the one
layout command with no after-hook). The renderer reads the marker on its next
redraw and clears it when it is done.

All of this is in the renderer rather than a hook because a hook cannot name
the pane it is about: `resize-pane -x` does not expand formats, and `:.{right}`
is not the sidebar once a layout has moved it. The renderer knows its own id.

A resize is asked for once per wrong width — a window that will not give up the
columns is not fought with — while a re-dock is never withheld. A window too
narrow for the width rule is left alone entirely (the sidebar is retired there
anyway), and a sidebar you split in yourself is yours, place and width
included.

**Narrow windows get none.** A window narrower than `@tt_obs_width + 1 +
@tt_obs_min_width` (46 + 1 + 80 = 127 columns) gets no automatic sidebar, and a
window that shrinks below that loses it. tmux treats a split's width as a
request and squeezes the *other* pane instead of refusing, so without this
check a 50-column window would have kept its sidebar and crushed your own pane
to a column or two. The width setting is live: set `@tt_obs_width 30` and new
sidebars are 30 columns.

**The toggle is global.** The panel is one thing, so the toggle turns it on or
off *everywhere*: it writes `@tt_obs_scope` for the whole server and acts on
the window you are in at once. Two keys run it — `prefix g` (short; on this
config every other lowercase letter is already bound, `e` and `g` were the only
two free) and `prefix C-o`. They are one command alias bound twice, so they
cannot drift. (With this repo's config the prefix is `C-a`, so it is `C-a g`.) `q` in an automatic sidebar does exactly the same thing —
closing the panel is closing the panel, however you close it. Turning it off
and on again keeps a `sessions` scope: the toggle remembers it in
`@tt_obs_scope_prev` rather than rewriting your setting to `all`.

There is no per-window dismissal. An automatic sidebar you kill by hand
(`prefix x`, or a mouse) comes back the next time that window is shown; use
`C-o` to mean "not right now". A sidebar you split in yourself with `tt observe
--pane` is yours, and neither the scope nor the toggle removes it.

**Turning it off.** The window you are in loses its sidebar immediately; every
other window loses its own the next time it is shown, switched to or resized,
through the same reconcile that created it. `tmux tt-obs-reconcile` does the
current window on demand.

**A sidebar never holds a window open.** When the last pane of your own work
ends — you type `exit`, or Claude quits — the window closes as it would have
without a sidebar in it: the guard on `pane-exited` and `window-pane-changed`
kills a sidebar that is the only thing left in its window. It can only ever
kill an *automatic* sidebar in a window where every pane is an observer, so a
window with any work still in it is never touched, and a window whose only pane
is a sidebar you split in yourself stays exactly where it is. The one case it misses is
a window you are *not* looking at whose focus was parked in the sidebar when
your last pane ended; visiting the window and switching panes clears it.

**Inside a sidebar.** `g` changes the grouping (saved, and followed by every
sidebar); `p`, `t`, `m` and `c`, in either case, go straight to project,
status, tmux or colour — the letter each tab shows in brackets. `j` and `k`
move the cursor, and `enter` goes to the highlighted session. `/` filters the
list (`enter` keeps, `esc` clears), `f` shows only a section or a state, `w`
widens a sidebar too narrow to draw, and `r` asks for a re-read. `s` (or
`enter`, or a click) starts the observer when there is none, and `q` closes the
panel — everywhere, as above.

**Right after a server starts.** No automatic sidebar is created while the
server holds a single pane, or a lone one-pane session named `0`. Those are
tmux-resurrect's own "restoring from scratch" markers, and both boot paths on
this machine start exactly that shape before they restore. A sidebar created
in that moment would stop resurrect from recognising a fresh boot, and leave
session `0` behind after every reboot.

### After a reboot

Sidebars are not restored; fresh ones are created instead. A saved sidebar
comes back as a plain shell, and the `@resurrect-processes` line types `exit`
into it, so it closes; the hooks then add a new sidebar as each window is
shown. Without that line, every reboot would leave one extra 46-column shell
per window.

With `@resurrect-capture-pane-contents 'on'`, resurrect saves what each sidebar
last showed, and a blocked background agent's row includes what it is blocked
on. That text lands in `~/.local/share/tmux/resurrect/` alongside every other
pane's contents.

### The status-line counter

`tt observe` writes `@tt_status` when the counts it would show change, and at
no other time: one `tmux set-option` per real change, and none while nothing
changes. No timer, and no `#(…)` that tmux would fork on every redraw. Setting
the option is enough to repaint every attached client (tmux 3.7c, measured), so
nothing else is sent. With no `@tt_status` placeholder in your status line, `tt
observe` sends tmux no write at all (`--status-line=always|never` override the
detection). A clean stop unsets it. A crash leaves the last value until
`tmux set -gu @tt_status`, and `tt sessions --doctor` flags it.

It shows a robot, then three badges, and a badge is **a glyph and a number on a
colour**:

| badge | counts | light | dark | glyph |
|---|---|---|---|---|
| amber | sessions waiting for you | `#d79921` | `#fab387` | `` bell |
| red | failed sessions and API errors | `#cc241d` | `#ff5f5f` | `` warning |
| grey | liveness unknown | `#bdae93` | `#585b70` | `` question |

Bold, one space of padding inside each fill, abutting with no separator.

**The colours are tt's, not your palette's** — and that is a correction, not a
preference. The first version used `@thm_peach` and `@thm_red`, and on this
machine's light theme that made waiting a burnt orange that reads as alarm and
errors a pale wash that reads as nothing: exactly backwards. Your palette is
not at fault; `theme.sh` says so itself — its light chip fills are deliberately
"soft washes carrying dark text", and peach is one of three entries tuned as a
*stroke* colour and "never as a chip fill". Semantic weight cannot be borrowed
from slots tuned for another job. The values above still sit inside your two
palettes (the light amber and grey are your `@thm_yellow` and `@thm_overlay_0`,
the light red is the Gruvbox red your theme derives from, and the dark red is
the sidebar's own error colour), so the bar stays coherent.

They follow your theme **live**: each colour is emitted as a tmux conditional
on the markers your theme script publishes, so `prefix T` repaints the badges
immediately without tt being told. When `@theme_dark` is set (the per-client
predicate `theme.sh` publishes: 1 or 0 for the screen drawing the bar), every
screen attached to the server paints its own badges, light on a light screen
and dark on a dark one. Without it the conditional falls back to `@theme_mode`,
one mode for the whole server, and with neither the bar's own background is
measured for luminance instead. The ink is the better of `#11111b` and
`#ffffff` by WCAG contrast against each fill, and every shipped pair clears
4.5:1.

The row is led by `󰚩`, in the bar's own foreground rather than on a
fill — it marks the counts as Claude's, and a label is not a state. It appears
only when a badge does, so an idle fleet leaves the bar alone.

Every glyph is overridable, for a font without it. An option you never set
keeps the default; one set to a blank removes the glyph entirely:

```tmux
set -g @tt_icon_claude  " "   # no robot, just the badges
set -g @tt_icon_waiting "!"
set -g @tt_icon_error   "x"
set -g @tt_icon_unclear "?"
```

With nothing to paint with at all, the badges fall back to words — `3 waiting ·
1 error` — because a bare number with no fill and no glyph says nothing.

### What it costs

Measured on the isolated test server with `scope=all` across 8 windows, the
server watching this machine's real `~/.claude`:

| | processes | RSS | descriptors |
|---|---|---|---|
| `tt observe` (the one watch) | 1 | 16.7 MiB | 87, 69 of them the watch on `~/.claude` |
| each sidebar: `tt observe --pane --auto` | 1 | 11.3–11.7 MiB | 10, none on `~/.claude`, 1 socket to the server |
| each sidebar's `/bin/sh` parent | 1 | 2.4 MiB | 3 |
| **8 windows, total** | **17** | **128 MiB** | **191** |

Idle for 20 seconds, each sidebar used at most 10 ms of CPU (the `ps`
resolution), and the server none. The sidebars never watch anything
themselves: one sidebar that did would hold 69 descriptors on `~/.claude`, and
eight of them 552. The `/bin/sh` parent is there so that resurrect can see and
name the sidebar's process (the `exit` mapping above depends on it).

Each sidebar asks the server for an enriched stream, which carries the colour
and the counts. The server enriches a sidebar's first snapshot when it
attaches, and after that only the sessions that change. The facts are cached
per transcript (keyed by path, size and mtime), so only the first sidebar reads
the transcripts, and each later one costs a lookup per session. Measured: the
server had used 0.32 s of CPU once eight sidebars were attached, and it used
none over the next 20 idle seconds.

### What is verified

`moox/cmd/tt/tmuxlive_gate_test.go` runs 22 cases against an **isolated** tmux
server (`-f /dev/null`, a private `-L` socket), sourcing the block above
straight out of `tmux/.tmux.conf`. It covers both toggles, pressed on a real
client attached from a pty. It covers each creation route ending with one
sidebar, the multi-hook events, a re-fired window gaining none, narrow windows,
the width setting, scope off leaving no orphans, and the fresh-server hold. It
covers `scope = sessions`: sidebars in the listed sessions only, retired when
a session leaves the list, and exact matching for names with `*`, `?`, `[` or
a space in them. It covers the `^o` panel as tmux actually paints it: both
titles on the first line and the panel's column where the layout puts it. It
covers two reboot-and-restore rounds with tmux-resurrect's own `save.sh` and
`restore.sh` (every pane back in its saved slot, layouts applied, one sidebar
per window), and the status line following real state changes. To run it:

```
cd moox/cmd/tt && TT_TMUXLIVE=1 go test -tags tmuxlive -run TestLive -v .
```

## Theme

Colors adapt to the terminal background — light and dark both read correctly,
detected automatically with no configuration.

`TERM_THEME=light` or `TERM_THEME=dark` in the environment is an explicit
statement and beats detection outright. It is the shared variable this repo's
tmux `theme.sh` publishes into the tmux global environment on every apply, and
`madbench` and `magmux` honour the same name. It exists because detection has a
hole nothing else can fill: inside tmux, OSC 11 is answered by tmux from what
its attached client told it, and a client that never answers a colour query
(an SSH terminal on a tablet, measured) leaves tmux silent, so every probe read "dark" on
a light screen. Any other value, including `auto`, is ignored. `tt
panel-render` takes it as its default theme too, and `--theme` overrides it.

Detection asks the terminal for its actual background color with an OSC 11
escape sequence, once, before the TUI starts. It has to run first because the
reply arrives on stdin, and bubbletea's input loop would otherwise eat it.

The part worth knowing about, if this ever needs touching: termenv refuses to
send that query whenever `TERM` names a multiplexer, on the grounds that a
multiplexer can be attached to several terminals at once. It then falls back to
`COLORFGBG`, and finally to a hardcoded black. Since `tt` runs inside tmux
essentially always, and this repo's tmux config deliberately unsets
`COLORFGBG`, both fallbacks were dead and every terminal was reported as dark —
measured, the fallback said `#000000` where the terminal's real background is
`#f9f6da`. So `tt` asks anyway, claiming an `xterm-256color` TERM for the
duration of the question: tmux answers OSC 11 itself from what it knows of the
attached client, in well under a millisecond, even from a pane that is not the
visible one.

The query is skipped when nothing could answer it — inside tmux with no client
attached — because termenv would then wait out a five-second timeout that
cannot be lowered. Such a pane is one nobody is looking at, so it falls back to
dark and starts instantly.

## Install

Built and installed to `~/go/bin` (which must be on your `PATH`):

```sh
task tt                                              # wired Taskfile path — run from anywhere in the repo
cd moox/cmd/tt && go install .                       # install local/uncommitted changes
go install github.com/MadAppGang/moox/cmd/tt@latest  # install the pushed version from any machine
```

`go install .` is directory-sensitive — it must run inside `moox/cmd/tt/` (that's
why `task tt` sets `dir:` to it). The `@latest` form is location-independent but
installs the last **pushed** commit, not your working tree.

Requires Go and tmux.
