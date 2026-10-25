#!/bin/sh
# Two-palette light/dark theming for the tmux chrome, per SCREEN.
#
#   theme.sh dark | light     pin a palette for EVERY screen (the menu uses this)
#   theme.sh toggle [CLIENT]  flip what CLIENT shows by pinning the opposite of
#                             its mode (the menu passes the screen that pressed
#                             it); with no CLIENT, the opposite of the default
#   theme.sh forget           drop the pin; every screen follows its terminal
#   theme.sh screen HOST light|dark|auto
#                             set the mode of every screen that comes from
#                             HOST (a tailnet name, an IP, or `local` for the
#                             server machine's own windows); auto removes the
#                             setting. For screens that cannot report: mosh,
#                             Blink. A remote machine can push it:
#                             ssh <server host> theme.sh screen …
#   theme.sh screen this MODE [CLIENT]
#                             the same for CLIENT's host (default: the client
#                             tmux names; the menu bakes HOST in instead)
#   theme.sh sync             recompute every screen's mode and publish it, and
#                             paint the windows whose screens disagree
#   theme.sh follow           old names of sync, kept for hooks that still call
#   theme.sh refresh [SESS]   them and for the Omarchy hook (SESS is ignored)
#   theme.sh status           one line per screen: TTY HOST MODE SOURCE
#   theme.sh detect           print the server-wide default, "light" or "dark"
#   theme.sh auto             config load: re-derive the default, retire the
#                             old hooks, reinstall the chrome, then sync
#   theme.sh menu [CLIENT]    the prefix+T picker, opened on CLIENT
#
# One tmux server is watched from several screens at once — iTerm windows on
# the server machine, foot on a laptop, a tablet — and they need not agree: a cream
# terminal can sit beside a dark one on the same session. So every screen (a
# tmux client) draws its own status bar, window list, pane borders, menus and
# popups in the palette of the terminal it is shown on, at the same moment,
# from one set of options. `sync` decides each screen's mode and publishes the
# answer as ONE global option, rewritten whole on every change because tty
# names are reused by the next terminal that opens them:
#
#   @theme_screens " /dev/ttys064=light /dev/ttys085=dark "
#
# Every colour tmux draws is a format that looks the drawing client up in that
# string (see "Why the chrome is formats" below), so nothing runs at draw time
# and a screen changes the moment the string does.
#
# Pane CONTENTS cannot be per screen — tmux keeps one grid per pane and sends
# every screen the same cells — so while the screens viewing a window
# disagree, that window's panes are painted in the palette of the screen last
# used there (see "Pane contents" below), and while they agree nothing is
# painted at all.
#
# Every sync also publishes TERM_THEME=light|dark into the environment new
# panes inherit: the global copy follows the server default, and every
# attached session gets its own copy (plus COLORFGBG) — the mode its screens
# agree on, or, while they disagree, the mode its panes are painted in. It is
# the shared variable tt, madbench and magmux honour ahead of their own probe.
#
# theme.sh never prints tmux's own errors and never calls `tmux run-shell`.
# The only things it ever kills are its own: the `tailscale status` it started,
# when that overruns its two seconds (see "Which host a screen is on") or its
# pass is interrupted, and that lookup's watchdog. `kill -0` only asks whether
# a lock's owner is alive.
#
# ---------------------------------------------------------------------------
# How a screen's mode is decided: the first rule that answers wins
#
#   pin      ~/.config/tmux/theme.pinned, a deliberate human choice (the menu,
#            toggle, `theme.sh light`), holds for every screen
#   host     ~/.config/tmux/screens, one `HOST MODE` line per host, set with
#            `theme.sh screen` or the menu's "This screen" rows
#   bg       the GLOBAL window-style background, else the GLOBAL
#            window-active-style one, judged by luminance (see below)
#   report   the screen's own light/dark report, #{client_theme}
#   default  `detect`: the pin, else the last default, else the screen used
#            most recently, else light
#
# The host rule is for screens that cannot report at all. mosh answers the
# colour queries itself and passes no theme report through; Blink answers OSC
# 11 but never 996/2031, and #{client_theme} is set only by a 997 report, so
# no command can set it. Such a screen is declared instead, once per machine,
# and the declaration is remembered in the file across restarts.
#
#
# The bg rule exists for Omarchy. omarchy-theme-set-tmux writes the theme's
# colours into the GLOBAL window-style and window-active-style on every theme
# switch, and foot there labels itself dark whatever the theme: its 997 report
# names the active colour SECTION, and Omarchy recolours running windows
# without ever switching it. A global bg paints every pane on every screen the
# same, so the chrome has to match what the screen really shows, and the
# colour is judged ahead of the label, as zsh/term-theme.zsh does.
#
# Only the GLOBAL value counts. A window's or a pane's own style, an app's
# OSC 11 colour, and this feature's own paint never decide a mode. That is
# why #{pane_bg} is not read: tmux folds every one of those into it, so a
# window someone styled bg=#123456 would have flipped the chrome of every
# screen that looked at it. With the stock config the global window-style is
# `default`, so the rule never fires.
#
# ---------------------------------------------------------------------------
# Which host a screen is on
#
# The tmux client of a remote screen inherits SSH_CONNECTION from its login:
# ssh sets it, tssh does too, and mosh-server keeps the one its ssh bootstrap
# had. Its first field is the screen's IP. No SSH_CONNECTION means `local`.
# The variable is read from the client process itself (#{client_pid}): from
# /proc/PID/environ on Linux, from `ps eww` on macOS. That works for tmux on
# macOS 27, but not for every process: the kernel hides the environment of
# Apple's own platform binaries (/bin/sleep shows argv only, measured), and
# Homebrew's tmux is not one. A `ps` line with no PATH= or HOME= on it is
# such a hidden environment, not an empty one: it says nothing, so it is never
# taken, or cached, as `local`. Only the first field ever leaves the pipe that
# reads it: the rest of that environment holds kcenv secrets, and is never
# stored, logged or kept in a variable.
#
# An IP becomes a name through `tailscale status`, run at most once per pass
# for every new remote screen together and given two seconds: a hung
# tailscale must not hold up the bar, and its pass holds the sync lock. The
# answer is cached in @theme_hosts as tty:pid=host, so a screen costs a lookup
# once, not per pass. Only a RESOLVED answer is cached — a tailnet name, or
# `local` from an environment that could be read. When the lookup fails or
# times out, the screen is shown under its IP and asked about again on the
# next pass: caching the fallback would pin a mosh screen to an IP its
# `omarchy dark` line can never match.
#
# "No name" is an answer too, and it is remembered — as a MISS, never as a
# name — in @theme_nohost (tty:pid=IP@UNTIL), for NOHOST_TTL seconds. Without
# it a screen from the LAN, `ssh localhost` or any IP the tailnet does not list
# ran `tailscale status` on every pass: about half a second each through the
# app's CLI wrapper (measured), on every window switch, under the lock. A miss
# is recorded when the lookup answered without that IP, when no tailscale is
# installed, and without asking at all for an address outside the tailnet's
# 100.64.0.0/10 (`tailscale status` lists nothing else). A lookup that failed
# or timed out records nothing, so it is asked again on the next pass, as
# above. The miss runs out after NOHOST_TTL, and one that claims to last
# longer (a hand-made entry, a clock step) is not believed, so a name the
# tailnet gains later is found within a minute.
#
# ---------------------------------------------------------------------------
# Why the chrome is formats, not colours
#
# Catppuccin resolves every colour it draws through `#{E:@thm_*}`, and the
# formats it stores keep those references UNEXPANDED, so they are resolved at
# draw time — with the client being drawn. So each @thm_<name> holds
#
#   #{?#{E:@theme_dark},<dark hex>,<light hex>}
#
# and @theme_dark is a predicate over #{client_tty}, @theme_screens and
# @theme_default: the one bar format expands cream on one screen and Mocha on
# the next. Verified with two nested clients of one session, which drew
# different bars, borders and window lists at the same moment.
#
# Two consequences:
#   - Everything that reads @thm_* must read it through E:. `show -gv @thm_bg`
#     returns the format text now, not a colour, and so does a bare
#     #{@thm_bg}; the two Catppuccin window-number pins in conf/theme.conf (and the
#     same two lines in chrome_cmds) exist for exactly that reason.
#   - The style options Catppuccin bakes to a literal at load (`set -gF`:
#     status-style, the pane borders, popups, menus, …) are rewritten here as
#     per-client formats over #{E:@thm_*}, once per config load (chrome_cmds,
#     guarded by @theme_chrome). Options tmux draws with NO client at hand —
#     the command prompt, copy-mode — get one theme-neutral literal instead.
#
# Setting @catppuccin_flavor and re-sourcing the plugin still does NOT work:
# the palette files declare colours with `set -ogq`, and -o refuses to
# overwrite an option that already exists, so on a live server every line
# no-ops. The same -o is what lets the conditional @thm_* survive a reload.
#
# ---------------------------------------------------------------------------
# Pane contents: painted while the screens disagree
#
# A program in a pane draws one grid, and tmux sends the same cells to every
# screen. Only the colours a program leaves to the terminal — the default fg
# and bg, the 16 ANSI slots — come out in each screen's own palette, which is
# exactly what goes wrong when a cream and a dark terminal watch the same
# window: Gruvbox Light's ink turns up on Mocha, or the reverse. So:
#
#   - The screens viewing a window agree (or there is one): nothing is
#     painted. Each terminal keeps its own colours, as before this existed.
#   - They disagree: every pane of the window is painted in the palette of
#     the window's LEAD (term_palette_light / term_palette_dark): its own
#     window-style, window-active-style, pane-colours[0..15] and cursor-colour.
#     tmux then turns default and ANSI colours into fixed RGB for every
#     screen, so the pane looks the same, and readable, on both.
#
# The lead is tmux's own `latest` client for the window: the screen that last
# typed there, or attached, or switched to it — whoever was using it. Four
# hooks in conf/theme.conf record it as NAME:PID in the window option @theme_lead
# at every point where tmux sets `latest` and fires a hook; a bare tty name
# would be inherited by the next terminal that opens that pty, the pid is
# not. A record is trusted only while a current viewer has exactly that
# NAME:PID; otherwise the most recently active viewer leads. So walking from
# the cream screen to the dark one and typing turns the window dark, and back.
# The exception is a window a SCRIPT switched to or opened (tt, tmux-fzf, an
# agent's `tmux select-window`): the record names the most recently active
# screen while tmux's `latest` stays where it was, and typing on that other
# screen fires nothing until the recorded one types there (see the
# @theme_cmd_id note in conf/theme.conf; T3a pins it).
#
# Paint is PANE-level, never window or global. A user's own window-level or
# global style never blocks a paint and is never written or unset: unpaint
# removes only this file's pane values, and theirs shows again. A pane that
# carries its OWN pane-level style or colours (select-pane -P, set -p) is left
# unpainted on every screen — the one place a reader may see the wrong
# palette, because painting it would destroy a value someone chose.
#
# Ownership is the recorded value, nothing else. The pane option
# @theme_painted records exactly the strings written. A mode change writes the
# marker as "<new>+<old>" first and "<new>" last, so a batch cut short (a
# killed runner, a pane gone mid-pass) leaves only recorded values behind and
# the next pass finishes it. Before anything is repainted or unpainted, the
# live pane-level values are compared with that record. A value that is not
# in it is someone else's: the pane "steps aside" — only this file's values
# and the marker are unset — and is never painted again while that value
# stays. Two residuals are accepted, because tmux offers neither provenance
# nor compare-and-set: a foreign write of the very string recorded counts as
# ours, and a foreign pane-level write that lands in the milliseconds between
# a pass's reads and its batch can be overwritten or unset.
#
# The record is read for what it SAYS, not compared with today's palettes:
# each part names its mode and every value it wrote. So a pane painted before
# an edit of term_palette_light/_dark is still recognised as ours, and is
# repainted in the new palette or unpainted, never stranded as "foreign" in
# the old one. A part counts as a record only in the exact shape this file
# writes (mode;style;style;colour;16 colours, in the palettes' alphabet),
# because a record is written back into the transitional marker of a repaint.
#
# An EMPTY pane-level pane-colours (every index unset one by one) is a value
# too: it hides any window-level or global pane-colours from the pane. No
# record holds an empty array, so it is always someone else's.
#
# pane-colours is set as one array and, whenever every entry left is ours,
# removed as one array. Unsetting it index by index (only when a foreign index
# must survive) leaves an EMPTY array entry at pane level, and that empty
# entry hides any window-level or global pane-colours from the pane (measured,
# 3.7c) — a residue this file would have created.
#
# What programs hear. Setting or unsetting a pane's window-style makes tmux
# recompute the theme of every pane that asked for reports (mode 2031: Claude
# Code `auto`, zsh/term-theme.zsh) and push CSI ?997;1n / 2n when it changed:
# a painted pane hears the paint's mode; an unpainted one falls back to the
# background of the first-connected screen that answered tmux's colour query,
# then to the screens' reports if they are unanimous, else to nothing (a
# program then keeps its last theme). One rule outranks the paint: while a
# control-mode client is attached, a pane background it reported with
# `refresh-client -r` decides that pane's 996/997 answers (window.c:1919,
# 1954-1966). Nothing in this setup runs a control client; control clients
# also never vote and never lead, because they never draw.
#
# Only windows some screen DISPLAYS are painted, so the dozens of background
# Claude panes are never repainted, and a painted pane that leaves the screens'
# view is unpainted on the next pass.
#
# ---------------------------------------------------------------------------
# Detection (the `report` and `default` rules)
#
# macOS appearance is NOT usable: this machine reports AppleInterfaceStyle=Dark
# while the terminal profile is Gruvbox Light. The terminal, not the OS, owns
# this setting.
#
# COLORFGBG is NOT usable either, which the earlier version of this file relied
# on. iTerm exports "15;0" for the Light profile exactly as for the Dark one —
# measured 2026-09-27 — because it names palette SLOTS, and the Gruvbox Light
# profile puts its cream in slot 0. It says "dark" on every window here.
#
# The answer comes from tmux itself. Since 3.6 tmux asks every client for its
# background (OSC 11) on attach and subscribes to its theme reports (mode
# 2031), keeps the result in #{client_theme}, and fires client-light-theme /
# client-dark-theme when it changes. tmux does the tty conversation, so none of
# the old worry about racing it for keystrokes applies. It is also PER CLIENT,
# which is what makes per-screen chrome possible at all: the old per-session
# design could only give a session shown on two terminals the theme of the
# one that reported last. Blink on a tablet reports nothing; its screen gets
# the server default.

set -e

# --- which server ----------------------------------------------------------
# Every tmux call in this file goes through tmux() below, and so to ONE
# server: the one whose job runs us ($TMUX: tmux puts it in the environment
# of every run-shell and hook job), else the one moox selected
# ($MOOX_TMUX_SOCKET, a path or an -L name, as `moox setup` passes it). The
# binary is $MOOX_TMUX_BIN when moox chose one, else tmux on PATH. With
# neither variable there is no server to talk to and the call fails without
# running tmux: a bare `tmux` with $TMUX empty would reach the user's DEFAULT
# server (moox setup runs `theme.sh detect` from outside tmux, I-1).
tmux() {
  if [ -n "${TMUX:-}" ]; then
    set -- -S "${TMUX%%,*}" "$@"
  elif [ -n "${MOOX_TMUX_SOCKET:-}" ]; then
    case $MOOX_TMUX_SOCKET in
      */*) set -- -S "$MOOX_TMUX_SOCKET" "$@" ;;
      *)   set -- -L "$MOOX_TMUX_SOCKET" "$@" ;;
    esac
  else
    return 1
  fi
  command "${MOOX_TMUX_BIN:-tmux}" "$@"
}

CFG="${HOME}/.config/tmux"
STATE="$CFG/theme"
# Set only by a deliberate human choice (menu, toggle, explicit light/dark).
# detect() treats it as final, so auto-detection can never overturn it.
PIN="$CFG/theme.pinned"
# Per-host modes (the host rule). Written only by screens_set, under its lock.
SCREENS="$CFG/screens"
# Where tailscale lives when the app is installed without its CLI on PATH.
TAILSCALE_APP=/Applications/Tailscale.app/Contents/MacOS/Tailscale

# The chrome version sync installs. A server whose @theme_chrome differs gets
# the chrome on its next sync; `auto` unsets it on every config load, because
# Catppuccin re-bakes the styles then. The legacy cleanup is NOT tied to it:
# it has its own once-per-server marker (see "legacy" below).
CHROME_VERSION=per-client-1

# How long a "no tailnet name" answer is believed (see "Which host a screen is
# on"): long enough that a nameless screen costs one lookup a minute, not one
# per window switch; short enough that a device the tailnet gains is named
# within a minute.
NOHOST_TTL=60

NL='
'

# --- palettes --------------------------------------------------------------
# Format: one "name value" pair per line.
#
# --- the cc_* entries: Claude session tab colours ---------------------------
#
# The cc_cur_* / cc_dim_* / cc_ink_dim entries at the bottom of each heredoc
# are the per-window Claude session colours drawn by window-status-format.
# Three things about them that are not obvious from the hex values:
#
# 1. They are ALWAYS CHIP FILLS CARRYING TEXT, never one-pixel strokes. So on
#    the light side they follow the tint rule in the palette_light() note
#    below rather than the three saturated exceptions (blue/peach/teal) — a
#    tab name has to stay readable ON them, which a stroke colour never has to
#    support. If you re-tint them, re-check the fill role, not the line role.
#
# 2. They must pass TWO thresholds, and the second is the one that bit during
#    design:
#      a. ink contrast >= 4.5:1   (cur -> @thm_crust, dim -> @thm_cc_ink_dim)
#      b. >= 1.15:1 against the STATUS BAR itself (@thm_mantle)
#    The first light tuning passed (a) everywhere and failed (b) at 1.01:1:
#    pale washes that were ISOLUMINANT with the cream bar, i.e. exactly the
#    old #e3d5a8-message-bar-on-#f2e5bc-status-bar failure recorded further
#    down this file. Every light value here was deepened until (b) held; they
#    now sit at 1.18–1.32:1, the dark ones at 1.90–2.59:1.
#
# 3. They are DELIBERATELY NOT Catppuccin's accents and not Claude Code's own.
#    Reusing @thm_green would make a Claude-green tab indistinguishable from
#    every other green this bar already draws (the session chip, for one), and
#    would couple tab semantics to a palette that is re-tuned for unrelated
#    reasons. A separate cc_ namespace also means Catppuccin's `set -ogq`
#    declarations can never clash with them.
#
# cc_ink_dim INVERTS between the palettes on purpose: dark ink on the pale
# light washes, LIGHT ink on the deep dark fills. That is not an oversight, it
# is the same deliberate inversion the index chip already uses (see the
# window-status-format note in conf/theme.conf).
#
# No code change is needed to add entries here — chrome_cmds turns every name
# that BOTH heredocs define into a per-screen `set -g @thm_<name>`, and the
# format chains in conf/theme.conf resolve @thm_cc_* at DRAW time for the screen
# being drawn, which is why every coloured tab follows each screen for free
# and no per-window work is ever needed.

palette_dark() {
  cat <<'EOF'
bg         #1e1e2e
fg         #cdd6f4
crust      #11111b
mantle     #181825
surface_0  #313244
surface_1  #45475a
surface_2  #585b70
overlay_0  #6c7086
overlay_1  #7f849c
overlay_2  #9399b2
subtext_0  #bac2de
subtext_1  #a6adc8
rosewater  #f5e0dc
flamingo   #f2cdcd
pink       #f5c2e7
mauve      #cba6f7
red        #f38ba8
maroon     #eba0ac
peach      #fab387
yellow     #f9e2af
green      #a6e3a1
teal       #94e2d5
sky        #89dceb
sapphire   #74c7ec
blue       #89b4fa
lavender   #b4befe

cc_ink_dim     #cdd6f4
cc_cur_red     #ff6b81
cc_cur_orange  #ff9f45
cc_cur_yellow  #f5d547
cc_cur_green   #6fdc8c
cc_cur_cyan    #4fd6e0
cc_cur_blue    #6aa8ff
cc_cur_purple  #b98bff
cc_cur_pink    #ff8ad1
cc_dim_red     #7a3644
cc_dim_orange  #7a4a24
cc_dim_yellow  #62511e
cc_dim_green   #2f6640
cc_dim_cyan    #275f6a
cc_dim_blue    #35507f
cc_dim_purple  #523c78
cc_dim_pink    #74385c
EOF
}

# Light palette. Deliberately quiet: most accents are PALE (tints, not the
# saturated Gruvbox brights) so the chips read as soft washes carrying dark
# text, the same way Mocha's pastel chips carry dark text on the dark side.
# Using Gruvbox's saturated accents here — olive #79740e, ochre #b57614 —
# produced heavy blocks that fought the cream background.
#
# THREE ENTRIES BREAK THAT RULE ON PURPOSE — blue, peach and teal are saturated.
# A tint works as a chip background under dark text, but the same tint drawn as
# a one-pixel LINE on a cream background is invisible. Those three are the ones
# used as foreground strokes (pane borders, clock) and never as a chip fill, so
# they are tuned for the opposite job. yellow is saturated for a third reason:
# it is the current-window chip, which has to out-shout the inactive greys.
#
# If you re-tint these, check both roles: chip fills need dark text to pass,
# stroke colours need to be visible against #fbf1c7.

# ---------------------------------------------------------------------------
# Claude session tab colours, LIGHT.
#
# Two tiers, both soft, generated rather than eyeballed: each value is solved
# for a target CONTRAST AGAINST THE BAR (#f2e5bc), not for a fixed lightness.
# That distinction is the whole trick. At a fixed lightness the warm hues
# collide with the cream bar — a first attempt put yellow at 1.00:1, i.e.
# exactly isoluminant and therefore invisible — while the cool hues stayed
# obvious. Solving per hue puts every colour at the same perceived weight.
#
#   active   ~1.22:1 against the bar
#   inactive ~1.09:1 against the bar   (deliberately barely there)
#
# The inactive tier is meant to recede almost to nothing; hue is all it needs
# to carry, because the index chip (amber vs grey) and `bold` already say which
# window is current. If you are tempted to raise it, that is the request it was
# tuned against, not an oversight.
#
# RED IS TAKEN FROM THE STATUSLINE, not generated. #ffaf87 is xterm 216, the
# colour the statusline's own worktree chip lands on, and it was chosen because
# every generated red read as pink at this weight. It is fully saturated
# (sat 1.00) and deeper than its siblings — 1.42:1 against the bar where the
# rest sit at 1.22 — and that is what makes it read as red rather than salmon.
#
# Its hue (20 deg) sits close to orange's 30. They are separated by SATURATION,
# 1.00 against 0.59, not by hue. If red and orange ever start looking alike,
# move orange toward yellow rather than shifting red — red is pinned to the
# statusline value on purpose, so the two surfaces agree.
#
# Ink is @thm_cc_ink_dim (#3c3836) on both tiers, 6.8-8.5:1 against every fill
# — well past WCAG AA. That headroom is exactly what these pale fills buy, and
# it is why the inactive tier can recede this far without the name going grey.
#
# The values below are GENERATED. To retune, solve for a contrast target rather
# than editing hexes by hand, or the warm hues will drift back into the bar.
palette_light() {
  cat <<'EOF'
bg         #fbf1c7
fg         #504945
crust      #3c3836
mantle     #f2e5bc
surface_0  #ece0b8
surface_1  #e3d5a8
surface_2  #d5c4a1
overlay_0  #bdae93
overlay_1  #a89984
overlay_2  #7c6f64
subtext_0  #665c54
subtext_1  #7c6f64
rosewater  #f0d9c4
flamingo   #f2cfc2
pink       #eccfe0
mauve      #dcc6ec
red        #f2c2c2
maroon     #eecccc
peach      #af3a03
yellow     #d79921
green      #d8e3b0
teal       #427b58
sky        #c6dfe8
sapphire   #c4dcea
blue       #076678
lavender   #d3d6f2
cc_ink_dim     #3c3836
cc_cur_red     #ffaf87
cc_cur_orange  #eacbad
cc_cur_yellow  #e1d099
cc_cur_green   #b4dbaf
cc_cur_cyan    #afd8dd
cc_cur_blue    #c0d2ea
cc_cur_purple  #dccae8
cc_cur_pink    #f0c5d7
cc_dim_red     #ffcab0
cc_dim_orange  #f0d8c1
cc_dim_yellow  #e8dcb1
cc_dim_green   #c7e4c3
cc_dim_cyan    #c3e1e5
cc_dim_blue    #d0ddef
cc_dim_purple  #e5d7ee
cc_dim_pink    #f3d4e1
EOF
}

# --- chrome: the per-client formats, installed once per config load --------

# 1 when the client being drawn is dark, 0 when light. A client not listed in
# @theme_screens yet (attached a moment before its sync ran), and a format
# expanded with no client at all (`display -p` without -c), fall back to
# @theme_default. An entry with an empty tty would read `* =dark *` and match
# nothing useful, which is why sync never publishes one.
THEME_DARK='#{?#{m:* #{client_tty}=dark *,#{@theme_screens}},1,#{?#{m:* #{client_tty}=light *,#{@theme_screens}},0,#{==:#{@theme_default},dark}}}'

# chrome_cmds: the chrome as tmux command lines, one write per line, every
# value single-quoted. A sourced file's commands are grouped per LINE, so a
# line that fails stops only itself. Unquoted, `#` would start a comment;
# double-quoted, tmux would expand `$` and `~` in the value. No value here
# contains a single quote.
chrome_cmds() {
  printf "set -g @theme_dark '%s'\n" "$THEME_DARK"

  # Every name BOTH heredocs define becomes one conditional. Lowercase hex is
  # load-bearing: in a format, #D #F #H #I #P #S #T #W are single-letter
  # aliases, so an uppercase #F9E2AF would lose its first digit to
  # window_flags. It is also what tmux prints a colour back as.
  { palette_dark; printf '%s\n' '--'; palette_light; } | LC_ALL=C awk -v q="'" '
    $0 == "--" { light = 1; next }
    NF != 2    { next }
    !light     { dark[$1] = $2; order[++n] = $1; next }
               { lite[$1] = $2 }
    END {
      for (i = 1; i <= n; i++) {
        k = order[i]
        if (k in lite) printf "set -g @thm_%s %s#{?#{E:@theme_dark},%s,%s}%s\n", k, q, dark[k], lite[k], q
      }
    }'

  # --- per-client styles ----------------------------------------------------
  #
  # These are the options Catppuccin bakes to a literal at load time (`set
  # -gF`, which resolves the palette ONCE and stores the hex), so they froze
  # to whichever palette was active during the last full reload. Rewritten
  # as formats over #{E:@thm_*} they resolve at draw time, per client: tmux
  # expands a style option with the drawing client for the status line, the
  # window list, pane borders, menus and popups. The indirection costs about
  # 11 us per colour against 1 us for a literal (measured), ~1 ms per status
  # draw per client; the cheaper fallback is a #,-escaped hex conditional per
  # style, at the price of a second copy of the palette.
  #
  # Borders are strokes, not fills, so they use the saturated stroke colours.
  # Active is blue rather than Catppuccin's lavender: lavender is a pale tint
  # in the light palette and vanishes as a one-pixel line on cream.
  #
  # The window-number pins repeat conf/options.conf's two lines on purpose: with the
  # chrome installed on a server still running the old config (between a merge
  # and the next `prefix r`), the window list then renders correctly too.
  # Catppuccin's own default "#{@thm_overlay_2}" has no E:, and with @thm_*
  # holding a format it would draw the raw format text.
  #
  # --- theme-neutral literals -----------------------------------------------
  #
  # The message line and copy-mode highlight are drawn with NO client context:
  # the prompt applies its style with a NULL format tree (status.c:894-896), so
  # a format there resolves to the default style and loses `fill=`. They get
  # one literal that reads on a cream AND a dark screen: dark ink on amber,
  # 6.6:1, and amber stands out from both a cream bar and a dark one.
  #
  # Message line — the bar you type into for prefix+, prefix+: rename-session
  # and every confirm-before. Deliberately the loudest thing this theme draws:
  # it is the one bar that is WAITING for an answer, so it should not be
  # mistakable for the status bar it replaces. Amber (#d79921, the light
  # palette's own yellow) because the prompt has no client to pick a palette
  # by, and amber is the one accent that reads as a signal on cream AND on
  # Mocha. Every other candidate — sapphire, mauve, green — is a pale tint on
  # the light side and washes out against the cream, which is exactly what the
  # previous surface_1 tan did: a #e3d5a8 bar sitting on an #f2e5bc bar,
  # invisible. bold on the whole line because the text on it is being edited
  # and wants to lead.
  #
  # `fill=` is load-bearing, not decoration. tmux's own defaults carry it —
  # `bg=yellow,fg=black,fill=yellow` — because bg colours only the cells the
  # message actually writes, while fill paints the remainder of the line. Set
  # message-style without a fill and the prompt stops clearing the bar: the
  # window list stays on screen and the prompt is drawn straight over it, so
  # prefix+, and prefix+: leave you typing on top of the tabs.
  #
  # No `align=centre` either, for a related reason. The prompt draws its label
  # through this style's alignment but always puts the typed input at a fixed
  # left offset, so centring throws "(rename-window)" into the middle of the
  # bar while the text being edited appears over on the left. Both were
  # verified by capturing a nested client's status line at tmux 3.7c.
  #
  # Copy-mode selection: dark ink on the same amber, the pairing that stays
  # readable when it overpaints syntax-coloured text. The clock is drawn as
  # large blocks, so a mid blue (3.7:1 on cream, 3.9:1 on Mocha) is enough.
  cat <<'EOF'
set -g status-style 'bg=#{E:@thm_mantle},fg=#{E:@thm_fg}'
set -g pane-border-style 'fg=#{E:@thm_overlay_0}'
set -g pane-active-border-style '#{?pane_in_mode,fg=#{E:@thm_yellow},#{?pane_synchronized,fg=#{E:@thm_peach},fg=#{E:@thm_blue}}}'
set -g popup-style 'bg=#{E:@thm_bg},fg=#{E:@thm_fg}'
set -g popup-border-style 'fg=#{E:@thm_overlay_0}'
set -g menu-style 'bg=#{E:@thm_bg},fg=#{E:@thm_fg}'
set -g menu-selected-style 'fg=#{E:@thm_fg},bold,bg=#{E:@thm_overlay_0}'
set -g menu-border-style 'fg=#{E:@thm_overlay_0}'
set -g window-status-activity-style 'bg=#{E:@thm_lavender},fg=#{E:@thm_crust}'
set -g window-status-bell-style 'bg=#{E:@thm_yellow},fg=#{E:@thm_crust}'
set -g @catppuccin_window_number_color '#{E:@thm_overlay_2}'
set -g @catppuccin_window_current_number_color '#{E:@thm_yellow}'
set -g message-style 'fg=#1d2021,bg=#d79921,fill=#d79921,bold'
set -g message-command-style 'fg=#1d2021,bg=#d79921,fill=#d79921,bold'
set -g mode-style 'fg=#1d2021,bg=#d79921'
set -g clock-mode-colour '#458588'
EOF
}

# --- legacy: what the per-session design left on a server -------------------
#
# The design before this one (7f317de..6e925af) gave each session a palette
# by writing SESSION-level copies of status-style, message-style,
# message-command-style, @theme_mode and every @thm_*, and WINDOW-level
# copies of nine styles on each of its windows. A session or window value
# outranks the global per-client format, so left in place it would keep that
# session single-palette for good.
#
# They are removed ONCE per server — on its first sync, which then sets
# @theme_legacy_done — and only where the live value is one that design could
# have written: a colour from the palettes below, or a style string built
# from them exactly as its apply built it (legacy_values). A config reload
# never repeats it, and a value under one of those names that the old apply
# could not have produced — a production session tinted with its own
# status-style, say — is someone else's, and stays.
#
# The palettes as that design shipped them, "name dark light", byte for byte
# from 6e925af (unchanged since 7f317de; tests T5 and T8 check both). FROZEN:
# they describe what an old server may still hold, not what this file draws.
# Never edit them along with palette_dark / palette_light.
legacy_palette() {
  cat <<'EOF'
bg             #1e1e2e #fbf1c7
fg             #cdd6f4 #504945
crust          #11111b #3c3836
mantle         #181825 #f2e5bc
surface_0      #313244 #ece0b8
surface_1      #45475a #e3d5a8
surface_2      #585b70 #d5c4a1
overlay_0      #6c7086 #bdae93
overlay_1      #7f849c #a89984
overlay_2      #9399b2 #7c6f64
subtext_0      #bac2de #665c54
subtext_1      #a6adc8 #7c6f64
rosewater      #f5e0dc #f0d9c4
flamingo       #f2cdcd #f2cfc2
pink           #f5c2e7 #eccfe0
mauve          #cba6f7 #dcc6ec
red            #f38ba8 #f2c2c2
maroon         #eba0ac #eecccc
peach          #fab387 #af3a03
yellow         #f9e2af #d79921
green          #a6e3a1 #d8e3b0
teal           #94e2d5 #427b58
sky            #89dceb #c6dfe8
sapphire       #74c7ec #c4dcea
blue           #89b4fa #076678
lavender       #b4befe #d3d6f2
cc_ink_dim     #cdd6f4 #3c3836
cc_cur_red     #ff6b81 #ffaf87
cc_cur_orange  #ff9f45 #eacbad
cc_cur_yellow  #f5d547 #e1d099
cc_cur_green   #6fdc8c #b4dbaf
cc_cur_cyan    #4fd6e0 #afd8dd
cc_cur_blue    #6aa8ff #c0d2ea
cc_cur_purple  #b98bff #dccae8
cc_cur_pink    #ff8ad1 #f0c5d7
cc_dim_red     #7a3644 #ffcab0
cc_dim_orange  #7a4a24 #f0d8c1
cc_dim_yellow  #62511e #e8dcb1
cc_dim_green   #2f6640 #c7e4c3
cc_dim_cyan    #275f6a #c3e1e5
cc_dim_blue    #35507f #d0ddef
cc_dim_purple  #523c78 #e5d7ee
cc_dim_pink    #74385c #f3d4e1
EOF
}

# UQ_AWK: uq(v), a value as `show-options` printed it, without its double
# quotes. tmux double-quotes a string holding any of  #';${}%  and then
# escapes only " and \ inside, and single-quotes one holding a space or a ".
# A value that needed a backslash escape, or came single-quoted, holds a
# backslash, a quote or a dollar, which nothing this file writes or knows
# does: it is kept with a mark no known value starts with, so it never
# compares equal. Shared by the two readers of `show-options` (plan_panes,
# plan_legacy); each passes -v qq="'".
UQ_AWK='
  function uq(v) {
    if (length(v) >= 2 && v ~ /^".*"$/) {
      v = substr(v, 2, length(v) - 2)
      return index(v, "\\") ? "\003" v : v
    }
    if (substr(v, 1, 1) == qq) return "\003" v
    return v
  }
'

# plan_legacy: the legacy call's output on stdin — for each session and each
# window, a `::S $N` or `::W @N` header and then its own `show-options`
# listing (session-level, window-level: only what is set THERE), ending in
# ::END. Prints a `set -qu` for every listed value that legacy_values says
# the old apply could have written under that name at that scope, then the
# marker and ::OK — or, when the chain was cut short (a session or window that
# vanished mid-call fails its `show` and ends it, as in call 3), the lines for
# the sections it did answer, no marker, and ::PARTIAL, so the next pass
# finishes the job.
plan_legacy() {
  THEME_LEGACY=$(legacy_palette) LC_ALL=C awk -v qq="'" "$UQ_AWK"'
    function q(s) { return qq s qq }
    function add(s, n, v) { OLD[s, n, v] = 1 }
    # legacy_values: what the old apply() and window_styles() wrote, per
    # palette k (1 dark, 2 light), from the frozen palettes.
    BEGIN {
      n = split(ENVIRON["THEME_LEGACY"], L, "\n")
      for (i = 1; i <= n; i++) {
        if (split(L[i], f, " ") != 3) continue
        for (k = 1; k <= 2; k++) { P[k, f[1]] = f[k + 1]; add("s", "@thm_" f[1], f[k + 1]) }
      }
      for (k = 1; k <= 2; k++) {
        add("s", "status-style", "bg=" P[k, "mantle"] ",fg=" P[k, "fg"])
        m = "fg=" P[k, "crust"] ",bg=" P[k, "yellow"] ",fill=" P[k, "yellow"] ",bold"
        add("s", "message-style", m)
        add("s", "message-command-style", m)
        add("w", "mode-style", "bg=" P[k, "yellow"] ",fg=" P[k, "crust"])
        add("w", "popup-style", "bg=" P[k, "bg"] ",fg=" P[k, "fg"])
        add("w", "popup-border-style", "fg=" P[k, "overlay_0"])
        add("w", "menu-selected-style", "fg=" P[k, "fg"] ",bold,bg=" P[k, "overlay_0"])
        add("w", "pane-border-style", "fg=" P[k, "overlay_0"])
        add("w", "pane-active-border-style", "#{?pane_in_mode,fg=" P[k, "yellow"] ",#{?pane_synchronized,fg=" P[k, "peach"] ",fg=" P[k, "blue"] "}}")
        add("w", "window-status-activity-style", "bg=" P[k, "lavender"] ",fg=" P[k, "crust"])
        add("w", "window-status-bell-style", "bg=" P[k, "yellow"] ",fg=" P[k, "crust"])
        add("w", "clock-mode-colour", P[k, "blue"])
      }
      add("s", "@theme_mode", "light")
      add("s", "@theme_mode", "dark")
    }
    /^::S \$[0-9]+$/ { H[++NH] = substr($0, 5); SC[NH] = "s"; cur = NH; next }
    /^::W @[0-9]+$/  { H[++NH] = substr($0, 5); SC[NH] = "w"; cur = NH; next }
    $0 == "::END"    { ended = 1; cur = 0; next }
    !cur             { next }
    {
      sp = index($0, " ")
      if (!sp) next
      o = substr($0, 1, sp - 1)
      if ((SC[cur], o, uq(substr($0, sp + 1))) in OLD) U[cur] = U[cur] " " o
    }
    END {
      # A section counts only when the next header, or ::END, follows it: a
      # chain cut short ends at the header of the target that failed.
      for (k = 1; k <= NH; k++) {
        if (k == NH && !ended) continue
        t = (SC[k] == "s") ? q(H[k] ":") : q(H[k])
        nn = split(U[k], N, " ")
        for (i = 1; i <= nn; i++) print "set -qu -t " t " " N[i]
      }
      if (ended) { print "set -g @theme_legacy_done 1"; print "::OK" }
      else print "::PARTIAL"
    }'
}

# legacy_cleanup TARGETS: the legacy call for TARGETS ("$N" and "@N", one per
# line, as plan listed them) and its plan. LEGACY = the batch lines; 1 when
# the call did not answer in full, so the pass is owed again.
legacy_cleanup() {
  LEGACY=
  lc_list=$1
  set --
  while read -r lc_t; do
    case $lc_t in
      \$[0-9]*) set -- "$@" display-message -p "::S $lc_t" ';' show-options -t "$lc_t" ';' ;;
      @[0-9]*)  set -- "$@" display-message -p "::W $lc_t" ';' show-options -w -t "$lc_t" ';' ;;
    esac
  done <<EOF
$lc_list
EOF
  LEGACY=$(tmux "$@" display-message -p '::END' 2>/dev/null | plan_legacy)
  case $LEGACY in
    *::OK) LEGACY=${LEGACY%::OK} ;;
    *::PARTIAL) LEGACY=${LEGACY%::PARTIAL}; return 1 ;;
    *) LEGACY=; return 1 ;;
  esac
}

# --- the paint: pane contents while a window's screens disagree -------------
# (see "Pane contents" at the top)

# The paint palettes, one definition each (FR3.4): bg, fg and the 16 ANSI
# slots, "slot colour" per line. Light is iTerm's "Light" profile
# (iterm2/com.googlecode.iterm2.plist), dark is Catppuccin Mocha as kitty
# draws it (kitty/.config/kitty/kitty.conf), so a painted pane looks like the
# terminal its lead uses every day. These are TERMINAL colours, not the
# chrome's: the chrome palettes above are tuned for chips and strokes on a
# bar, these for text on a page. The cursor is painted in the fg, because
# Omarchy sets a global cursor-colour that could vanish against a painted bg.
# Lowercase is load-bearing here too: tmux prints a colour back lowercase,
# and ownership is decided by comparing those strings.
term_palette_light() {
  cat <<'EOF'
bg  #fbf1c7
fg  #3c3836
0   #fbf1c7
1   #cc241d
2   #98971a
3   #d79921
4   #458588
5   #b16286
6   #689d6a
7   #7c6f64
8   #928374
9   #9d0006
10  #79740e
11  #b57614
12  #076678
13  #8f3f71
14  #427b58
15  #3c3836
EOF
}

term_palette_dark() {
  cat <<'EOF'
bg  #1e1e2e
fg  #cdd6f4
0   #45475a
1   #f38ba8
2   #a6e3a1
3   #f9e2af
4   #89b4fa
5   #f5c2e7
6   #94e2d5
7   #bac2de
8   #585b70
9   #f38ba8
10  #a6e3a1
11  #f9e2af
12  #89b4fa
13  #f5c2e7
14  #94e2d5
15  #a6adc8
EOF
}

# PAINT_AWK: the awk both planners share — the records and the batch lines.
#
# paint_init builds, per mode m, from the two palettes (THEME_PAL_light and
# THEME_PAL_dark in the environment): WS[m], the value of both styles;
# CC[m], the cursor; PC[m, i] and PCS[m], the 16 pane-colours and the array
# as tmux prints it, space-joined; and REC[m], the marker record
#
#   MODE;WINDOW-STYLE;WINDOW-ACTIVE-STYLE;CURSOR;PANE-COLOURS
#
# — exactly the strings written, which are exactly what tmux prints back: a
# style verbatim, a colour lowercase #rrggbb, an array in index order.
#
# The line builders read the pane's live PANE-LEVEL values from LMK, LWS,
# LWAS, LCC and LPC (ABSENT when the pane level has none) and write only the
# lines whose value differs, so a new paint costs 5 lines, a repaint 6, a
# repair at most 4. Lines are buffered (emit), not printed: a planner whose
# input turns out incomplete prints none of them. Every target and value is
# single-quoted: unquoted, `#` would start a comment; double-quoted, tmux
# would expand `$` and `~`. Records hold no single quote.
PAINT_AWK='
  function q(s) { return qq s qq }
  function emit(s) { OUT[++NOUT] = s }
  function set_p(p, o, v) { emit("set -p -t " q(p) " " o " " q(v)) }
  function unset_p(p, o) { emit("set -pu -t " q(p) " " o) }
  function paint_init(   k, m, n, L, i, f) {
    ABSENT = "\002"
    for (k = 1; k <= 2; k++) {
      m = (k == 1) ? "light" : "dark"
      n = split(ENVIRON["THEME_PAL_" m], L, "\n")
      for (i = 1; i <= n; i++) if (split(L[i], f, " ") == 2) PAL[m, f[1]] = f[2]
      WS[m] = "bg=" PAL[m, "bg"] ",fg=" PAL[m, "fg"]
      CC[m] = PAL[m, "fg"]
      PCS[m] = ""
      for (i = 0; i < 16; i++) { PC[m, i] = PAL[m, i]; PCS[m] = PCS[m] (i ? " " : "") PAL[m, i] }
      REC[m] = m ";" WS[m] ";" WS[m] ";" CC[m] ";" PCS[m]
    }
  }
  # PAINT an unmarked pane: the marker FIRST, so a batch cut short is still
  # marked and everything it can leave behind is in the record.
  function paint(p, m) {
    set_p(p, "@theme_painted", REC[m])
    set_p(p, "window-style", WS[m])
    set_p(p, "window-active-style", WS[m])
    set_p(p, "cursor-colour", CC[m])
    set_p(p, "pane-colours", PCS[m])
  }
  # rec_ok(s): s has the exact shape of a record this file writes,
  # MODE;WINDOW-STYLE;WINDOW-ACTIVE-STYLE;CURSOR;16 COLOURS, every field in
  # the alphabet the palettes use — whatever palette wrote it (see "Pane
  # contents": ownership survives a palette edit). The alphabet is also what
  # makes a record from a live marker safe to write back inside the single
  # quotes of a batch line: no quote, no ;, no +, no $.
  function rec_ok(s,   f, c, k) {
    if (split(s, f, ";") != 5 || (f[1] != "light" && f[1] != "dark")) return 0
    if (f[2] !~ /^[a-z0-9#=,]+$/ || f[3] !~ /^[a-z0-9#=,]+$/ || f[4] !~ /^[a-z0-9#]+$/ || f[5] !~ /^[a-z0-9# ]+$/) return 0
    if (split(f[5], c, " ") != 16) return 0
    for (k = 1; k <= 16; k++) if (c[k] !~ /^[a-z0-9#]+$/) return 0
    return 1
  }
  # REPAINT to m: while the values change, the marker names the record of m
  # AND every record the pane still holds values of — the other mode, or the
  # same mode in an older palette — and it names m alone once they have.
  function repaint(p, m,   t, n, parts, j) {
    t = REC[m]
    n = split(LMK, parts, "+")
    for (j = 1; j <= n; j++)
      if (parts[j] != REC[m] && rec_ok(parts[j]) && !index("+" t "+", "+" parts[j] "+")) t = t "+" parts[j]
    if (LMK != t) set_p(p, "@theme_painted", t)
    if (LWS != WS[m]) set_p(p, "window-style", WS[m])
    if (LWAS != WS[m]) set_p(p, "window-active-style", WS[m])
    if (LCC != CC[m]) set_p(p, "cursor-colour", CC[m])
    if (LPC != PCS[m]) set_p(p, "pane-colours", PCS[m])
    if (t != REC[m]) set_p(p, "@theme_painted", REC[m])
  }
  # REPAIR a pane whose marker is the record of m and whose present values
  # all belong to m: write back only what is missing.
  function repair(p, m) {
    if (LWS == ABSENT) set_p(p, "window-style", WS[m])
    if (LWAS == ABSENT) set_p(p, "window-active-style", WS[m])
    if (LCC == ABSENT) set_p(p, "cursor-colour", CC[m])
    if (LPC != PCS[m]) set_p(p, "pane-colours", PCS[m])
  }
  # UNPAINT a pane whose every live value is ours: the marker LAST, so a pane
  # still marked still has something to finish. pane-colours goes as one
  # array, which also removes an emptied entry (see "Pane contents").
  function unpaint(p) {
    if (LPC != ABSENT) unset_p(p, "pane-colours")
    if (LCC != ABSENT) unset_p(p, "cursor-colour")
    if (LWAS != ABSENT) unset_p(p, "window-active-style")
    if (LWS != ABSENT) unset_p(p, "window-style")
    unset_p(p, "@theme_painted")
  }
'

# paint_palettes: PAL_LIGHT and PAL_DARK, the two palettes as the planners
# read them.
paint_palettes() {
  PAL_LIGHT=$(term_palette_light)
  PAL_DARK=$(term_palette_dark)
}

# --- detect ----------------------------------------------------------------

# read_mode FILE: RM is FILE's first line when that is light or dark, else ''.
read_mode() {
  RM=
  [ -f "$1" ] || return 0
  IFS= read -r RM 2>/dev/null < "$1" || :
  case $RM in light|dark) ;; *) RM= ;; esac
}

detect() {
  # 1. A PINNED choice is final. Anything you select by hand — the menu, the
  #    toggle, `theme.sh light` — pins, and nothing auto-detected may overrule
  #    it. This is the opposite of the original design, which treated the
  #    terminal as the source of truth and let the next new shell overturn a
  #    manual pick. On this machine that was simply wrong, and it presented as
  #    "I chose light and it went dark by itself".
  read_mode "$PIN"
  if [ -n "$RM" ]; then printf '%s\n' "$RM"; return; fi

  # 2. Last applied mode, so a reload is never a change.
  read_mode "$STATE"
  if [ -n "$RM" ]; then printf '%s\n' "$RM"; return; fi

  # 3. The window most recently in use, as tmux measured it (see Detection).
  m=$(tmux list-clients -F '#{client_activity} #{client_theme}' 2>/dev/null |
    sort -rn | awk '$2 == "light" || $2 == "dark" { print $2; exit }') || m=
  case "$m" in light|dark) printf '%s\n' "$m"; return ;; esac

  # 4. Default. macOS appearance is deliberately NOT consulted: it reports Dark
  #    on this machine while iTerm runs a light profile, so using it as the
  #    fallback made "Re-detect" reliably pick the wrong answer. The terminal
  #    owns this setting and the OS knows nothing about it.
  printf 'light\n'
}

# --- the resolver: shared by sync, status and toggle -----------------------

# call1: every field one pass reads, in ONE tmux invocation, ending in ::END
# so a truncated answer (a server going away mid-call) is never mistaken for
# a complete one. The global styles are read with `show -gwqv`, the global
# value only (see "How a screen's mode is decided"); markers frame them
# because a style value is one bare line.
call1() {
  tmux display-message -p '::G #{@theme_chrome}|#{@theme_default}|#{@theme_mode}|#{@theme_screens}|#{@theme_remote}|#{@theme_hosts}|#{@theme_nohost}|#{@theme_legacy_done}' \; \
    display-message -p '::WS' \; show-options -gwqv window-style \; \
    display-message -p '::WAS' \; show-options -gwqv window-active-style \; \
    list-clients -F '::C #{client_name}|#{client_tty}|#{client_pid}|#{client_control_mode}|#{client_theme}|#{client_activity}|#{client_created}|#{session_id}|#{window_id}|#{@theme_lead}' \; \
    display-message -p '::END'
}

# resolve_inputs: the files the resolver reads: PINMODE, STATEMODE, and
# SCREENS_DATA, the screens file as validated "host mode" lines. A screens
# file that cannot be read counts as empty here: a reader only loses the host
# rule for one pass, where a writer would lose every other host (screens_set).
resolve_inputs() {
  read_mode "$PIN";   PINMODE=$RM
  read_mode "$STATE"; STATEMODE=$RM
  SCREENS_DATA=$(screens_read 2>/dev/null) || SCREENS_DATA=
}

# resolve: call1's output on stdin, the hosts (hosts_for) in $HOSTS and the
# screens file in $SCREENS_DATA; prints
#   H <1 if the chrome must be installed, else 0>
#     <1 if the legacy cleanup has not run on this server, else 0>
#     <attached session ids…>
#   G <the published values, tab-separated, as call1 read them>
#   N <the server default>
#   C <tty> <name> <pid> <host> <mode> <source> <session id> <window id> <cache>
#     <activity> <created> <lead> <miss>
# with one C line per screen (a non-control client with a tty), sorted by
# tty; <cache> is 1 when <host> may be remembered in @theme_hosts, <lead> is
# the @theme_lead of the window the screen displays, '' unless it has the
# NAME:PID shape, and <miss> is the epoch second until which the screen's
# "no tailnet name" may be remembered in @theme_nohost, 0 for none. Every
# value that later reaches a tmux command is validated here.
resolve() {
  THEME_HOSTS=$HOSTS THEME_SCREENS=$SCREENS_DATA \
  LC_ALL=C awk -v pin="$PINMODE" -v state="$STATEMODE" -v ver="$CHROME_VERSION" '
    function hexv(s,   i, n) {
      n = 0
      for (i = 1; i <= length(s); i++) n = n * 16 + index("0123456789abcdef", substr(s, i, 1)) - 1
      return n / 255
    }
    # The mode a style string gives by its background, "" when it sets none.
    # Later tokens win in a tmux style, so the LAST bg= decides. The weights
    # are Rec.709 on 0..1 components, the same test zsh/term-theme.zsh uses.
    function style_mode(s,   n, a, i, bg) {
      s = tolower(s)
      gsub(/[ \t]+/, ",", s)
      n = split(s, a, ",")
      for (i = n; i >= 1; i--) {
        if (a[i] !~ /^bg=/) continue
        bg = substr(a[i], 4)
        if (bg !~ /^#[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]$/) return ""
        return (0.2126 * hexv(substr(bg, 2, 2)) + 0.7152 * hexv(substr(bg, 4, 2)) + 0.0722 * hexv(substr(bg, 6, 2)) > 0.5) ? "light" : "dark"
      }
      return ""
    }
    function tty_ok(s) { return s ~ /^\/dev\/[A-Za-z0-9\/._-]+$/ }
    BEGIN {
      # "tty:pid host cache miss", one per screen (hosts_for), and "host
      # mode", one per host (screens_read); both validated by their writers.
      n = split(ENVIRON["THEME_HOSTS"], HL, "\n")
      for (i = 1; i <= n; i++)
        if (split(HL[i], f, " ") == 4) { hhost[f[1]] = f[2]; hcache[f[1]] = f[3]; hmiss[f[1]] = (f[4] ~ /^[0-9]+$/) ? f[4] : 0 }
      n = split(ENVIRON["THEME_SCREENS"], SL, "\n")
      for (i = 1; i <= n; i++) if (split(SL[i], f, " ") == 2 && (f[2] == "light" || f[2] == "dark")) scr[f[1]] = f[2]
    }
    /^::G / {
      split(substr($0, 5), g, "|")
      oc = g[1]; od = g[2]; om = g[3]; os = g[4]; orm = g[5]; oh = g[6]; onh = g[7]; olg = g[8]
      next
    }
    $0 == "::WS"  { sect = "ws"; next }
    $0 == "::WAS" { sect = "was"; next }
    $0 == "::END" { sect = ""; next }
    /^::C / {
      sect = ""
      n = split(substr($0, 5), f, "|")
      # Control-mode clients never draw, and a client without a tty cannot
      # be named in @theme_screens.
      if (n < 10 || f[4] == "1" || !tty_ok(f[2])) next
      nc++
      tty[nc] = f[2]
      name[nc] = (f[1] ~ /^[A-Za-z0-9\/._-]+$/) ? f[1] : f[2]
      pid[nc] = (f[3] ~ /^[0-9]+$/) ? f[3] : ""
      theme[nc] = f[5]
      act[nc] = f[6] + 0
      cre[nc] = f[7] + 0
      sid[nc] = (f[8] ~ /^\$[0-9]+$/) ? f[8] : ""
      wid[nc] = (f[9] ~ /^@[0-9]+$/) ? f[9] : ""
      # Only compared, never written anywhere; a value with a "|" in it has
      # split into more fields, and is no NAME:PID.
      lead[nc] = (n == 10 && f[10] ~ /^[A-Za-z0-9\/._-]+:[0-9]+$/) ? f[10] : ""
      next
    }
    sect == "ws"  { ws = $0; sect = ""; next }
    sect == "was" { was = $0; sect = ""; next }
    END {
      # The default, as detect() computes it: the pin, else the last default,
      # else the most recently active screen that reported, else light.
      if (pin != "") def = pin
      else if (state != "") def = state
      else {
        def = "light"; ba = -1; bc = -1
        for (i = 1; i <= nc; i++)
          if ((theme[i] == "light" || theme[i] == "dark") && (act[i] > ba || (act[i] == ba && cre[i] > bc))) {
            def = theme[i]; ba = act[i]; bc = cre[i]
          }
      }
      bgm = style_mode(ws)
      if (bgm == "") bgm = style_mode(was)

      for (i = 1; i <= nc; i++) ord[i] = i
      for (i = 2; i <= nc; i++) {
        k = ord[i]
        for (j = i - 1; j >= 1 && tty[ord[j]] > tty[k]; j--) ord[j + 1] = ord[j]
        ord[j + 1] = k
      }
      sids = ""
      for (i = 1; i <= nc; i++) {
        c = ord[i]
        # A screen hosts_for could not place (no pid) is local, uncached.
        k = tty[c] ":" pid[c]
        if (k in hhost) { host = hhost[k]; cache = hcache[k]; miss = hmiss[k] } else { host = "local"; cache = 0; miss = 0 }
        if (pin != "")                                      { m = pin;       src = "pin" }
        else if (host in scr)                               { m = scr[host]; src = "host" }
        else if (bgm != "")                                 { m = bgm;       src = "bg" }
        else if (theme[c] == "light" || theme[c] == "dark") { m = theme[c];  src = "report" }
        else                                                { m = def;       src = "default" }
        out[i] = "C\t" tty[c] "\t" name[c] "\t" pid[c] "\t" host "\t" m "\t" src "\t" sid[c] "\t" wid[c] "\t" cache "\t" act[c] "\t" cre[c] "\t" lead[c] "\t" miss
        if (sid[c] != "" && !(sid[c] in seen)) { seen[sid[c]] = 1; sids = sids " " sid[c] }
      }
      printf "H %d %d%s\n", (oc != ver), (olg != "1"), sids
      printf "G\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", oc, od, om, os, orm, oh, onh
      printf "N\t%s\n", def
      for (i = 1; i <= nc; i++) print out[i]
    }'
}

# plan: call 2's output on stdin, resolve's in $1 ($RECS), and whether the
# chrome ($2) and the legacy cleanup ($3) are owed. Prints the batch lines
# for every value that differs from its live one, in four sections,
#
#   <the chrome token, the globals, the environment, and the panes call 2
#    can decide alone>
#   ::REFRESH
#   <a refresh-client line for each screen whose mode changed>
#   ::QUERY
#   <"%N TARGET" for each pane that only its own pane-level answer (call 3)
#    can decide; TARGET is light, dark, or - for none>
#   ::LEGACY
#   <"$N" for each session and "@N" for each window the legacy call must
#    read, only while the cleanup is owed>
#   ::OK
#
# — or only ::INCOMPLETE when call 2 did not end in ::END. Only the
# TERM_THEME and COLORFGBG lines of the environment are kept, here inside the
# awk: the rest of it (kcenv secrets among it) is never stored, logged or put
# in a variable.
#
# The panes (see "Pane contents" at the top). Each window some screen
# displays gets a TARGET: none while its viewers agree, else the lead's mode
# — the viewer whose NAME:PID the window's @theme_lead names, or, when none
# does, the most recently active viewer (then the latest attach, then the
# lowest tty; client_activity counts whole seconds, so it ties). Call 2 gives
# every pane's EFFECTIVE values (pane, else window, else global), which is
# enough to decide a pane that is INTACT — its marker one record, its values
# that record's — and nothing else:
#   INTACT      repaint to the target, unpaint when there is none, or nothing
#   marked, not INTACT (SUSPECT), or unmarked in a window with a target
#   (CANDIDATE), or a line a "|" in some value has split: call 3 decides
#   unmarked, no target: nothing
# A clean pass whose paint is intact therefore makes no third call — unless
# a pane with its own foreign style sits in a displayed mixed window: it is a
# candidate on every pass, so every pass asks about it (3 calls, not 2).
plan() {
  RECS=$1 THEME_PAL_light=$PAL_LIGHT THEME_PAL_dark=$PAL_DARK \
  LC_ALL=C awk -v need="$2" -v legacy="$3" -v ver="$CHROME_VERSION" -v qq="'" "$PAINT_AWK"'
    function ask(p, t) { if (!(p in QT)) QP[++NQ] = p; QT[p] = (t == "") ? "-" : t }
    BEGIN {
      paint_init()
      n = split(ENVIRON["RECS"], L, "\n")
      for (i = 1; i <= n; i++) {
        split(L[i], f, "\t")
        if (f[1] == "G") { od = f[3]; om = f[4]; os = f[5]; orm = f[6]; oh = f[7]; onh = f[8] }
        else if (f[1] == "N") def = f[2]
        else if (f[1] == "C") {
          nc++; tty[nc] = f[2]; name[nc] = f[3]; pid[nc] = f[4]; host[nc] = f[5]; mode[nc] = f[6]; sid[nc] = f[8]
          wid[nc] = f[9]; cache[nc] = f[10]; act[nc] = f[11] + 0; cre[nc] = f[12] + 0; lid[nc] = f[13]; miss[nc] = f[14] + 0
        }
      }
      # Each displayed window: the modes of its viewers, and the lead it
      # recorded (a window option, so every viewer reports the same one).
      for (i = 1; i <= nc; i++) {
        cw = wid[i]
        if (cw == "") continue
        if (!(cw in vm)) { vm[cw] = mode[i]; wl[++nw] = cw }
        else if (vm[cw] != mode[i]) vm[cw] = "mixed"
        if (lid[i] != "") wlead[cw] = lid[i]
      }
      for (k = 1; k <= nw; k++) {
        cw = wl[k]
        if (vm[cw] != "mixed") continue
        ld = 0
        for (i = 1; i <= nc; i++)
          if (wid[i] == cw && pid[i] != "" && (name[i] ":" pid[i]) == wlead[cw]) { ld = i; break }
        if (!ld)
          for (i = 1; i <= nc; i++) {
            if (wid[i] != cw) continue
            if (!ld || act[i] > act[ld] || (act[i] == act[ld] && (cre[i] > cre[ld] || (cre[i] == cre[ld] && tty[i] < tty[ld])))) ld = i
          }
        target[cw] = mode[ld]
      }
      cur = ""
    }
    /^::P / {
      n = split(substr($0, 5), f, "|")
      p = f[3]
      # A linked window lists its panes once per session: decide each once.
      if (p !~ /^%[0-9]+$/ || (p in pseen)) next
      pseen[p] = 1
      t = (f[2] in target) ? target[f[2]] : ""
      if (n != 8) { ask(p, t); next }
      if (f[4] == "") { if (t != "") ask(p, t); next }
      m = (f[4] == REC["light"]) ? "light" : (f[4] == REC["dark"]) ? "dark" : ""
      if (m == "" || f[5] != WS[m] || f[6] != WS[m] || f[7] != CC[m] || f[8] != PCS[m]) { ask(p, t); next }
      if (t == m) next
      LMK = REC[m]; LWS = WS[m]; LWAS = WS[m]; LCC = CC[m]; LPC = PCS[m]
      if (t == "") unpaint(p); else repaint(p, t)
      next
    }
    /^::E /       { cur = substr($0, 5); next }
    /^::W /       { split(substr($0, 5), w, " ")
                    if (w[1] ~ /^\$[0-9]+$/) sess[w[1]] = 1
                    if (w[2] ~ /^@[0-9]+$/) win[w[2]] = 1
                    next }
    $0 == "::END" { ended = 1; next }
    /^TERM_THEME=/ { env[cur, "TERM_THEME"] = substr($0, 12); next }
    /^COLORFGBG=/  { env[cur, "COLORFGBG"] = substr($0, 11); next }
    END {
      if (!ended) { print "::INCOMPLETE"; exit }

      # The chrome token; chrome_cmds itself is written by sync_once, ahead
      # of this batch.
      if (need == 1) print "set -g @theme_chrome " q(ver)

      # @theme_hosts and @theme_nohost hold the current screens only,
      # rewritten whole, so a detached client drops out on the next pass. An
      # uncached host (an IP fallback, an unreadable environment) is left out
      # of @theme_hosts and asked about again — unless it is a remembered
      # miss, which @theme_nohost carries until it runs out.
      scr = " "; rem = " "; hs = " "; nh = " "
      for (i = 1; i <= nc; i++) {
        scr = scr tty[i] "=" mode[i] " "
        if (host[i] != "local") rem = rem tty[i] " "
        if (pid[i] == "") continue
        if (cache[i] == 1) hs = hs tty[i] ":" pid[i] "=" host[i] " "
        else if (miss[i] > 0) nh = nh tty[i] ":" pid[i] "=" host[i] "@" miss[i] " "
      }
      if (scr != os)  print "set -g @theme_screens " q(scr)
      if (def != od)  print "set -g @theme_default " q(def)
      if (def != om)  print "set -g @theme_mode " q(def)
      if (rem != orm) print "set -g @theme_remote " q(rem)
      if (hs != oh)   print "set -g @theme_hosts " q(hs)
      if (nh != onh)  print "set -g @theme_nohost " q(nh)

      # The environment new panes inherit. The global TERM_THEME follows the
      # default. Each attached session gets the mode its panes are drawn for:
      # the target of its window while its screens disagree, else the mode
      # they agree on. A session with no screen (detached, or watched only by a
      # control client) keeps its last values. It is compared with the LIVE
      # value every pass, so a rewrite behind our back (Omarchy sets COLORFGBG
      # on every session) is undone by the next hook. Best effort all the
      # same: a pane opened in the very instant the lead changes inherits the
      # value before it; its shell asks the terminal itself at the first
      # prompt (zsh/term-theme.zsh), so only a program started before that
      # sees the stale one.
      if (env["", "TERM_THEME"] != def) print "set-environment -g TERM_THEME " q(def)
      for (i = 1; i <= nc; i++) {
        if (sid[i] == "") continue
        if (!(sid[i] in smode)) smode[sid[i]] = mode[i]
        else if (smode[sid[i]] != mode[i]) smode[sid[i]] = "mixed"
        if (target[wid[i]] != "") stgt[sid[i]] = target[wid[i]]
      }
      for (x in smode) {
        m = (x in stgt) ? stgt[x] : smode[x]
        if (m != "light" && m != "dark") continue
        fb = (m == "light") ? "0;15" : "15;0"
        if (env[x, "TERM_THEME"] != m) print "set-environment -t " q(x) " TERM_THEME " q(m)
        if (env[x, "COLORFGBG"] != fb) print "set-environment -t " q(x) " COLORFGBG " q(fb)
      }

      for (i = 1; i <= NOUT; i++) print OUT[i]

      # FR2.6: a refresh for each screen whose mode changed. Every option
      # write already flags every client for a redraw; this makes it explicit
      # and costs no extra fork, being part of the same batch.
      print "::REFRESH"
      no = split(os, O, " ")
      for (i = 1; i <= no; i++) { p = index(O[i], "="); if (p > 1) oldm[substr(O[i], 1, p - 1)] = substr(O[i], p + 1) }
      for (i = 1; i <= nc; i++) if (oldm[tty[i]] != mode[i]) print "refresh-client -S -t " q(name[i])

      # The panes call 3 must decide, in ascending id order.
      print "::QUERY"
      for (i = 1; i <= NQ; i++) num[i] = substr(QP[i], 2) + 0
      for (i = 2; i <= NQ; i++) {
        k = QP[i]; v = num[i]
        for (j = i - 1; j >= 1 && num[j] > v; j--) { QP[j + 1] = QP[j]; num[j + 1] = num[j] }
        QP[j + 1] = k; num[j + 1] = v
      }
      for (i = 1; i <= NQ; i++) print QP[i], QT[QP[i]]

      # What the legacy call must read: every session and every window
      # (see "legacy"), only while the cleanup is owed.
      print "::LEGACY"
      if (legacy == 1) {
        for (s in sess) print s
        for (x in win) print x
      }
      print "::OK"
    }'
}

# plan_panes: call 3's output on stdin, the asked panes in $1 ("%N TARGET"
# lines, ascending, as plan printed them). Prints the batch lines for every
# pane call 3 answered, then ::OK — or ::PARTIAL when some pane got no
# complete answer.
#
# Call 3 is `display -p '::Q %%N' ; show -p -t %N` per pane, then `display -p
# '::END'`. `%%`, because display-message runs strftime over its template
# first: a bare '::Q %5' comes back as '::Q 5'. `show -p` without -A lists
# the PANE-LEVEL options only, and prints a string double-quoted when it holds
# any of  #';${}%  (every record does), a colour bare, an array as one
# `name[i] value` line per index, and an array emptied index by index as its
# bare name (measured, 3.7c). A pane that died after call 2 makes its `show`
# fail, and a failed command ends the whole chain: its own header is the last
# line and no later pane has one. So a pane's answer counts only when its
# header is followed by the next header or by ::END. A pane without one is
# left alone — no paint, no unpaint, no stepping aside, because a missing
# answer looks exactly like "no foreign value" — and the pass is reported
# incomplete, so it runs again.
#
# Each answered pane, with T its target:
#   no marker     any pane-level style, cursor or pane-colours entry — an
#                 empty array included: FOREIGN, never painted (FR3.2's
#                 exception); else, with a T, PAINT
#   marker        a value is OURS when it equals that option's value in a
#                 record the marker holds (one record, or two while a mode
#                 change is in flight), read from the record itself, so a
#                 record of an older palette still owns its values; a
#                 pane-colours index >= 16 never is, nor an empty array
#     a value that is not ours: STEP ASIDE, unsetting only ours — pane-colours
#                 as one array unless a foreign index must stay — and then
#                 the marker; the pane is FOREIGN from then on
#     else no T:  UNPAINT
#     marker = today's record of T alone: REPAIR, writing back what is missing
#     else:       REPAINT to T, which also finishes a batch cut short and
#                 moves a pane from an older palette to today's
plan_panes() {
  THEME_QUERIES=$1 THEME_PAL_light=$PAL_LIGHT THEME_PAL_dark=$PAL_DARK \
  LC_ALL=C awk -v qq="'" "$PAINT_AWK$UQ_AWK"'
    # owns(o, v): v is the window-style ("ws"), window-active-style ("was")
    # or cursor ("cc") of a record the marker holds (RWS/RWAS/RCC[1..NRR]).
    function owns(o, v,   j) {
      for (j = 1; j <= NRR; j++)
        if ((o == "ws") ? (v == RWS[j]) : (o == "was") ? (v == RWAS[j]) : (v == RCC[j])) return 1
      return 0
    }
    function owns_pc(i, v,   j) {
      if (i >= 16) return 0
      for (j = 1; j <= NRR; j++) if (v == RPC[j, i]) return 1
      return 0
    }
    # records(m): the parts of marker m that are records (rec_ok), each
    # split into its fields, as RREC/RWS/RWAS/RCC/RPC[1..NRR].
    function records(m,   n, parts, j, f, c, k) {
      NRR = 0
      n = split(m, parts, "+")
      for (j = 1; j <= n; j++) {
        if (!rec_ok(parts[j])) continue
        NRR++
        split(parts[j], f, ";")
        RREC[NRR] = parts[j]; RWS[NRR] = f[2]; RWAS[NRR] = f[3]; RCC[NRR] = f[4]
        split(f[5], c, " ")
        for (k = 1; k <= 16; k++) RPC[NRR, k - 1] = c[k]
      }
    }
    function decide(p, t,   idx, npc, j, i, nf, fpc, nmine, mine, empty) {
      LMK = (p in MK) ? MK[p] : ABSENT
      LWS = (p in VWS) ? VWS[p] : ABSENT
      LWAS = (p in VWAS) ? VWAS[p] : ABSENT
      LCC = (p in VCC) ? VCC[p] : ABSENT
      npc = split(IX[p], idx, " ")
      if (npc) { LPC = VPC[p, idx[1] + 0]; for (j = 2; j <= npc; j++) LPC = LPC " " VPC[p, idx[j] + 0] }
      else LPC = (p in BARE) ? "" : ABSENT
      empty = (!npc && (p in BARE))

      if (LMK == ABSENT) {
        if (LWS != ABSENT || LWAS != ABSENT || LCC != ABSENT || npc || empty) return
        if (t != "") paint(p, t)
        return
      }
      records(LMK)
      nf = 0
      if (LWS != ABSENT && !owns("ws", LWS)) nf++
      if (LWAS != ABSENT && !owns("was", LWAS)) nf++
      if (LCC != ABSENT && !owns("cc", LCC)) nf++
      fpc = empty; nmine = 0
      for (j = 1; j <= npc; j++) {
        i = idx[j] + 0
        if (owns_pc(i, VPC[p, i])) { mine[j] = 1; nmine++ } else { mine[j] = 0; fpc++ }
      }
      if (nf || fpc) {
        if (LWS != ABSENT && owns("ws", LWS)) unset_p(p, "window-style")
        if (LWAS != ABSENT && owns("was", LWAS)) unset_p(p, "window-active-style")
        if (LCC != ABSENT && owns("cc", LCC)) unset_p(p, "cursor-colour")
        if (!fpc) { if (nmine) unset_p(p, "pane-colours") }
        else for (j = 1; j <= npc; j++) if (mine[j]) unset_p(p, q("pane-colours[" (idx[j] + 0) "]"))
        unset_p(p, "@theme_painted")
        return
      }
      if (t == "") { unpaint(p); return }
      if (NRR == 1 && RREC[1] == REC[t]) { repair(p, t); return }
      repaint(p, t)
    }
    BEGIN {
      paint_init()
      n = split(ENVIRON["THEME_QUERIES"], L, "\n")
      for (i = 1; i <= n; i++)
        if (split(L[i], f, " ") == 2 && f[1] ~ /^%[0-9]+$/) { QP[++NQ] = f[1]; QT[f[1]] = (f[2] == "-") ? "" : f[2] }
      cur = ""
    }
    /^::Q %[0-9]+$/ { cur = substr($0, 5); H[++NH] = cur; next }
    $0 == "::END"   { ended = 1; cur = ""; next }
    cur == ""       { next }
    {
      sp = index($0, " ")
      if (sp) { o = substr($0, 1, sp - 1); v = substr($0, sp + 1) } else { o = $0; v = "" }
      if (o == "@theme_painted") MK[cur] = uq(v)
      else if (o == "window-style") VWS[cur] = uq(v)
      else if (o == "window-active-style") VWAS[cur] = uq(v)
      else if (o == "cursor-colour") VCC[cur] = v
      else if (o == "pane-colours") BARE[cur] = 1
      else if (o ~ /^pane-colours\[[0-9]+\]$/) {
        i = substr(o, 14, length(o) - 14) + 0
        IX[cur] = IX[cur] " " i
        VPC[cur, i] = v
      }
    }
    END {
      for (k = 1; k <= NH; k++) known[H[k]] = (k < NH || ended)
      partial = 0
      for (k = 1; k <= NQ; k++) {
        if (!known[QP[k]]) { partial = 1; continue }
        decide(QP[k], QT[QP[k]])
      }
      for (i = 1; i <= NOUT; i++) print OUT[i]
      print partial ? "::PARTIAL" : "::OK"
    }'
}

# query_panes QUERIES: call 3 for plan's "%N TARGET" lines, and its plan.
# PANES = the batch lines for the panes it answered; 1 when some pane got no
# complete answer (it died mid-pass), so the pass is owed again.
query_panes() {
  PANES=
  qq_list=$1                  # kept: `set --` below reuses the positional parameters
  set --
  while read -r qp qt; do
    case $qp in %[0-9]*) ;; *) continue ;; esac
    case $qt in light|dark|-) ;; *) continue ;; esac
    set -- "$@" display-message -p "::Q %%${qp#%}" ';' show-options -p -t "$qp" ';'
  done <<EOF
$qq_list
EOF
  [ "$#" -gt 0 ] || return 0
  PANES=$(tmux "$@" display-message -p '::END' 2>/dev/null | plan_panes "$qq_list")
  case $PANES in
    *::OK) PANES=${PANES%::OK} ;;
    *::PARTIAL) PANES=${PANES%::PARTIAL}; return 1 ;;
    *) PANES=; return 1 ;;
  esac
}

# --- hosts: which machine each screen is on ---------------------------------
# (see "Which host a screen is on" at the top)

# host_ok HOST: HOST fits the stored grammar, [a-z0-9][a-z0-9._:-]{0,62}.
# The letters are spelled out because a bracket range follows the locale's
# collation in sh, where [a-z] can match capitals.
host_ok() {
  case $1 in
    ''|[!abcdefghijklmnopqrstuvwxyz0123456789]*) return 1 ;;
    *[!abcdefghijklmnopqrstuvwxyz0123456789._:-]*) return 1 ;;
  esac
  [ "${#1}" -le 63 ]
}

# ENV_CONN_AWK reads one process's environment — NUL-split /proc lines, or
# the single line `ps eww` prints, where the variables follow the command —
# and prints only what env_conn reports. The last SSH_CONNECTION= on a ps line
# wins: the variables come after the arguments. A ps line shows the
# environment only when the kernel lets it, and a hidden one looks like an
# empty one: argv alone. So a ps line counts as read only when it carries a
# PATH= or a HOME=, which every login environment has; without either it is
# `none`, never `local` (a remote screen would be cached as local for good).
# shellcheck disable=SC2016  # the $ in here are awk's, not the shell's
ENV_CONN_AWK='
  { seen = 1 }
  mode == "proc" && /^SSH_CONNECTION=/ { v = substr($0, 16); got = 1 }
  mode == "ps" {
    s = $0
    if (index(s, " PATH=") || index(s, " HOME=")) envok = 1
    while ((i = index(s, " SSH_CONNECTION=")) > 0) { s = substr(s, i + 16); v = s; got = 1 }
  }
  END {
    if (!seen) { print "none"; exit }
    sub(/ .*/, "", v)
    if (got && v ~ /^[0-9A-Fa-f.:]+$/) print "ip " v
    else if (mode == "ps" && !envok) print "none"
    else print "local"
  }'

# env_conn PID: what PID's environment says about its screen, as
#   ip ADDRESS   SSH_CONNECTION's first field: a remote screen
#   local        the environment was read, and has no valid SSH_CONNECTION
#   none         nothing could be read (the process is gone, or its
#                environment is hidden)
env_conn() {
  if [ -r "/proc/$1/environ" ]; then
    tr '\000' '\n' < "/proc/$1/environ" 2>/dev/null | LC_ALL=C awk -v mode=proc "$ENV_CONN_AWK"
  else
    ps eww -o command= -p "$1" 2>/dev/null | LC_ALL=C awk -v mode=ps "$ENV_CONN_AWK"
  fi
}

# tailnet_names IPS: TSNAMES = an "ip name" line for each of IPS (one per
# line) that `tailscale status` names. One run for all of them, in the
# background with a two-second watchdog, its output in a temp file whose name
# is fixed BEFORE it exists, beside the socket in tmux's 0700 directory. All
# three — the run, the watchdog, the file — are in globals, so on_exit cleans
# them up when the pass is interrupted: a trapped signal ends the `wait` at
# once. A missing, failing or slow tailscale leaves TSNAMES empty. TSANSWERED
# is 1 when an IP missing from TSNAMES has no name for now — tailscale
# answered without it, or there is no tailscale to ask — and 0 when the
# lookup failed or timed out, which says nothing about the IP (§16 item 2).
tailnet_names() {
  TSNAMES='' TSANSWERED=0
  tn_bin=$(command -v tailscale 2>/dev/null) || tn_bin=
  case $tn_bin in /*) ;; *) tn_bin= ;; esac
  if [ -z "$tn_bin" ] && [ -x "$TAILSCALE_APP" ]; then tn_bin=$TAILSCALE_APP; fi
  if [ -z "$tn_bin" ]; then TSANSWERED=1; return 0; fi
  TSOUT="$SOCK.theme.tailscale.$$"
  "$tn_bin" status > "$TSOUT" 2>/dev/null &
  TS_PID=$!
  # The watchdog sleeps two seconds, then kills the lookup. Cancelling it
  # (TERM) runs its trap, which kills its own sleep, so nothing of it outlives
  # the lookup; and its output goes nowhere, so no caller's $(…) waits on it.
  ( trap 'kill "$tn_sleep" 2>/dev/null; exit 0' TERM
    sleep 2 & tn_sleep=$!
    wait "$tn_sleep" && kill "$TS_PID" 2>/dev/null ) </dev/null >/dev/null 2>&1 &
  TS_WD=$!
  tn_rc=0
  # Braces, because the shell's "Terminated" notice for a killed lookup goes
  # to the shell's stderr when it reaps the job, not to wait's.
  { wait "$TS_PID" || tn_rc=$?; } 2>/dev/null
  TS_PID=
  # The lookup has ended, one way or the other: cancel the watchdog.
  { kill "$TS_WD"; wait "$TS_WD"; } 2>/dev/null || :
  TS_WD=
  if [ "$tn_rc" = 0 ]; then
    if TSNAMES=$(printf '%s\n' "$1" | LC_ALL=C awk '
        NR == FNR { if ($1 != "") want[$1] = 1; next }
        NF >= 2 && ($1 in want) && !($1 in got) { got[$1] = 1; print $1, $2 }' - "$TSOUT" 2>/dev/null); then
      TSANSWERED=1
    else
      TSNAMES=
    fi
  fi
  rm -f "$TSOUT" 2>/dev/null || :
  TSOUT=
}

# TAILNET_AWK: tailnet(ip), 1 when ip is in 100.64.0.0/10, the only
# addresses `tailscale status` lists (IPv4 only; nothing outside that range).
TAILNET_AWK='
  function tailnet(ip,   a) {
    if (ip !~ /^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$/) return 0
    split(ip, a, ".")
    return a[1] + 0 == 100 && a[2] + 0 >= 64 && a[2] + 0 <= 127
  }
'

# hosts_for CALL1: HOSTS = one "tty:pid host cache miss" line per screen of
# CALL1: cache 1 when the host may be remembered in @theme_hosts, and miss the
# epoch second until which its "no tailnet name" may be remembered in
# @theme_nohost (0: none). A cached host is taken as it is, and so is a miss
# that has not run out (and does not claim to last more than NOHOST_TTL from
# now). For the rest each client's environment is read, and every new IP the
# tailnet could name goes to ONE tailnet_names run. It runs in this shell,
# never in a $(…): the lookup's process and temp file must be in on_exit's
# view. "Now" is awk's srand(), which POSIX defines as the time of day: no
# extra fork for a clock.
hosts_for() {
  HOSTS=
  hf_known=$(printf '%s\n' "$1" | LC_ALL=C awk -v ttl="$NOHOST_TTL" '
    function tty_ok(s) { return s ~ /^\/dev\/[A-Za-z0-9\/._-]+$/ }
    function host_ok(h) { return h ~ /^[a-z0-9][a-z0-9._:-]*$/ && length(h) <= 63 }
    BEGIN { srand(); now = srand() }
    /^::G / {
      split(substr($0, 5), g, "|")
      n = split(g[6], h, " ")
      for (i = 1; i <= n; i++) {
        p = index(h[i], "=")
        if (p > 1 && host_ok(substr(h[i], p + 1))) cached[substr(h[i], 1, p - 1)] = substr(h[i], p + 1)
      }
      # tty:pid=host@until; host_ok admits no "@", so the first one splits.
      n = split(g[7], h, " ")
      for (i = 1; i <= n; i++) {
        p = index(h[i], "="); e = substr(h[i], p + 1); a = index(e, "@")
        if (p < 2 || a < 2) continue
        mh = substr(e, 1, a - 1); mu = substr(e, a + 1)
        if (host_ok(mh) && mu ~ /^[0-9]+$/ && mu + 0 > now && mu + 0 <= now + ttl) {
          k = substr(h[i], 1, p - 1); mhost[k] = mh; muntil[k] = mu
        }
      }
      next
    }
    /^::C / {
      split(substr($0, 5), f, "|")
      if (f[4] == "1" || !tty_ok(f[2]) || f[3] !~ /^[0-9]+$/) next
      k = f[2] ":" f[3]
      if (k in cached) print "K", k, cached[k], 1, 0
      else if (k in mhost) print "K", k, mhost[k], 0, muntil[k]
      else print "M", k, f[3]
    }')
  # One environment read per screen the cache does not know. A read that
  # returned nothing gives `local` for this pass only (cache 0).
  hf_new=$(printf '%s\n' "$hf_known" | while read -r hf_t hf_k hf_p; do
      [ "$hf_t" = M ] || continue
      hf_c=$(env_conn "$hf_p")
      # (pattern) with its opening paren: bash 3.2, macOS's sh, cannot parse
      # a bare `pattern)` inside $(…).
      case $hf_c in
        ('ip '*) printf 'I %s %s\n' "$hf_k" "${hf_c#ip }" ;;
        (local)  printf 'K %s local 1 0\n' "$hf_k" ;;
        (*)      printf 'K %s local 0 0\n' "$hf_k" ;;
      esac
    done)
  hf_ips=$(printf '%s\n' "$hf_new" | LC_ALL=C awk "$TAILNET_AWK"'$1 == "I" && tailnet($3) { print $3 }' | LC_ALL=C sort -u)
  TSNAMES='' TSANSWERED=0
  if [ -n "$hf_ips" ]; then tailnet_names "$hf_ips"; fi
  # A name is cached; an IP — no name, or no lookup — is not (§16 item 2).
  # An IP with no name for now (see TSANSWERED, and anything outside the
  # tailnet's range) is remembered as a miss until NOHOST_TTL from now; one
  # whose lookup failed is not, so the next pass asks again. Hosts are stored
  # lowercase, without anything outside [a-z0-9._:-], at most 63 long; an
  # IPv6 address that starts with a colon is given an ip- prefix to start
  # like a host.
  HOSTS=$(printf '%s\n%s\n' "$hf_known" "$hf_new" | THEME_NAMES=$TSNAMES \
    LC_ALL=C awk -v ttl="$NOHOST_TTL" -v answered="$TSANSWERED" "$TAILNET_AWK"'
    function host_ok(h) { return h ~ /^[a-z0-9][a-z0-9._:-]*$/ && length(h) <= 63 }
    function clean(s) { s = tolower(s); gsub(/[^a-z0-9._:-]/, "", s); return substr(s, 1, 63) }
    BEGIN {
      srand(); now = srand()
      n = split(ENVIRON["THEME_NAMES"], L, "\n")
      for (i = 1; i <= n; i++) if (split(L[i], f, " ") == 2) name[f[1]] = clean(f[2])
    }
    $1 == "K" { print $2, $3, $4, $5; next }
    $1 == "I" {
      if (($3 in name) && host_ok(name[$3])) { print $2, name[$3], 1, 0; next }
      h = clean($3)
      if (!host_ok(h)) h = substr("ip-" h, 1, 63)
      print $2, h, 0, ((!tailnet($3) || answered == 1) ? now + ttl : 0)
    }')
}

# resolve_now: call 1, the hosts and the resolver — one pass's reads, or one
# question's (status, toggle, menu, `screen this`). Sets OUT1 and RECS; 1 when
# there is no server, or it did not answer in full.
resolve_now() {
  if [ -z "$SOCK" ]; then sock_init || return 1; fi
  OUT1=$(call1 2>/dev/null) || return 1
  case $OUT1 in *::END) ;; *) return 1 ;; esac
  resolve_inputs
  hosts_for "$OUT1"
  RECS=$(printf '%s\n' "$OUT1" | resolve) || return 1
}

# sync_once: ONE pass. 0 when it published, or when nothing differed; 1 when
# it could not finish (the server did not answer in full, a pane got no
# complete answer, or a line of the batch failed because its target
# vanished), so the request is still owed.
#
#   call 1  the globals, the global styles and the clients (call1), then
#           the hosts of screens the cache does not know (hosts_for: no tmux
#           call; a failed lookup is not a failed pass, it falls back)
#   call 2  every pane's marker and effective paint values, the global
#           environment and each attached session's, plus every window while
#           the legacy cleanup is owed, ending in ::END (plan)
#   call 3  only when some pane needs it: those panes' own pane-level options
#           (query_panes, plan_panes)
#   legacy  once per server: each session's and window's own options
#           (legacy_cleanup, plan_legacy; see "legacy")
#   batch   one `source-file` of every line whose value differs, and nothing
#           at all when none does, because every option write walks every
#           pane and redraws every client (options.c:1180-1270). It ends with
#           the refresh-client lines, so the refresh costs no fork either.
#
# A pass that changes nothing makes exactly two tmux calls and no write —
# three while a pane with its own foreign style sits in a window whose
# screens disagree, because that pane is asked about on every pass. A pass
# that repaints a window makes three, and a first paint four. A server's
# first pass makes one more, the legacy call.
#
# tmux has no compare-and-set, so a foreign write that lands in the
# milliseconds between a pass's reads and its batch (Omarchy rewriting a
# session's COLORFGBG, say, or someone setting a pane's style) can be
# overwritten or unset. Accepted: every pass compares against the LIVE values
# again, so the next one sees whatever was lost.
#
# It runs as an `if` condition, where errexit is off, so every step checks its
# own result.
sync_once() {
  resolve_now || return 1
  hdr=${RECS%%"$NL"*}
  # shellcheck disable=SC2086  # word-split on purpose: H need legacy sid…, all validated
  set -- $hdr
  [ "$#" -ge 3 ] || return 1
  need=$2
  legacy=$3
  shift 3
  sids=$*

  set -- list-panes -a -F '::P #{session_id}|#{window_id}|#{pane_id}|#{@theme_painted}|#{window-style}|#{window-active-style}|#{cursor-colour}|#{pane-colours}' \
    ';' show-environment -g
  for sid in $sids; do
    set -- "$@" ';' display-message -p -t "$sid" "::E $sid" ';' show-environment -t "$sid"
  done
  if [ "$legacy" = 1 ]; then
    set -- "$@" ';' list-windows -a -F '::W #{session_id} #{window_id}'
  fi
  set -- "$@" ';' display-message -p '::END'
  paint_palettes
  planned=$(tmux "$@" 2>/dev/null | plan "$RECS" "$need" "$legacy")
  case $planned in *::OK) planned=${planned%::OK} ;; *) return 1 ;; esac
  body=${planned%%"::REFRESH$NL"*}
  planned=${planned#*"::REFRESH$NL"}
  refresh=${planned%%"::QUERY$NL"*}
  planned=${planned#*"::QUERY$NL"}
  queries=${planned%%"::LEGACY$NL"*}
  targets=${planned#*"::LEGACY$NL"}

  rc=0
  PANES='' LEGACY=''
  if [ -n "$queries" ]; then query_panes "$queries" || rc=1; fi
  if [ "$legacy" = 1 ]; then legacy_cleanup "$targets" || rc=1; fi
  [ -n "$body$PANES$LEGACY$refresh" ] || return "$rc"

  # The batch lives beside the socket, in tmux's own 0700 directory, under a
  # name fixed BEFORE it exists, so on_exit can always remove it.
  BATCH="$SOCK.theme.batch.$$"
  if ! { if [ "$need" = 1 ]; then chrome_cmds; fi; printf '%s' "$body$PANES$LEGACY$refresh"; } > "$BATCH"; then
    rm -f "$BATCH"; BATCH=
    return 1
  fi
  tmux source-file "$BATCH" >/dev/null 2>&1 || rc=1
  rm -f "$BATCH"; BATCH=
  return "$rc"
}

# --- sync: one runner per server -------------------------------------------
#
# Hooks fire in storms — an attach fires client-attached and a theme report
# within milliseconds, a window switch fires two more — and two passes at once
# would each write, and a stale one could undo a fresh one. So there is one
# runner per server, and no request is ever dropped:
#
#   - A request is a file, <socket>.theme.dirty, created BEFORE the lock is
#     tried. A request can therefore never fall between a runner's last look
#     and its release.
#   - The lock is a directory, <socket>.theme.lock (mkdir is atomic), holding
#     owner.<pid>, written the moment it is taken, which records the path of
#     the script that took it ($0).
#   - The runner takes the request (deletes the flag) before each pass, and
#     counts it served only when the pass published (sync_once returns 0). A
#     failed pass re-creates the flag and is retried — three attempts at most,
#     then the flag is left for the next request: bounded, never dropped. A
#     signal mid-pass re-creates it too (on_exit), which also releases the
#     lock and starts a fresh runner for it.
#   - After releasing, the runner looks at the flag once more, so a request
#     that arrived during its last pass is served by it.
#   - A lock is broken only when its holder is provably gone: its owner's pid
#     is dead; or alive, but the process now running under that pid is not
#     the script the owner file records (`ps` shows its command line); or the
#     lock has no owner file at all and is two minutes old (a runner killed
#     between mkdir and the owner write). A LIVE owner is never broken,
#     whatever the lock's age: a slow pass is not a dead one.
#
#     The identity check is what keeps a runner killed with SIGKILL (no trap
#     runs) from freezing theming for good: its lock outlives even the server,
#     and on this machine's process churn its pid is soon some other live
#     process, which `kill -0` alone would take for the runner. The command,
#     not the start time (`ps -o lstart`), because lstart is printed in local
#     time and, on Linux, re-derived from the wall clock, so a timezone change
#     or a clock step would make a live runner look like a stranger. An owner
#     file with nothing in it (written by an older theme.sh) falls back to
#     liveness alone, and so does a `ps` that answers nothing for a live pid.
#     What is left: a SIGKILLed runner's pid reused by another theme.sh of the
#     same path. Every such process is short-lived (a request returns at once,
#     the menu as soon as it is drawn), so the next request breaks the lock.
#
# Nobody ever waits on this lock: a request that finds it held returns at once,
# and the runner serves it. So the config-load `auto` cannot deadlock, and a
# hook costs one mkdir when a runner is already busy. Both files live beside
# the socket, in tmux's 0700 directory, and outlive the server; a stale flag
# costs the next server one extra pass.
#
# ONE trap per process: on_exit owns EXIT for the whole life of every theme.sh
# process, and HUP, INT and TERM turn into an exit that runs it. It is
# installed once, before any verb runs, and acts on what the process holds at
# that moment, so it removes every temp file of a pass itself — the host
# lookup's output, the batch — and nothing ever installs a trap of its own (a
# second EXIT trap would silently replace this one, and the next signal would
# lose the request). A trapped signal that arrives while the shell waits for a
# tmux call runs its trap when that call returns, so an abort waits for at most
# one call; the host lookup is awaited with `wait`, which a trapped signal ends
# at once.
#
# THEME_SYNC_TESTHOOK is for tests only (tests/theme-per-screen.test.sh): if
# it names an executable, the runner calls it with one argument at
# `acquired`, `pass`, `before_release` and `released`. No config sets it.

SOCK='' LOCK='' DIRTY='' OWNER='' BATCH='' INFLIGHT=0
# What on_exit may have to clean up: a host lookup (its output file, its pid,
# its watchdog's pid), the screens lock (its dir, owner file and temp file)
# and write_file's temp file.
TSOUT='' TS_PID='' TS_WD='' SL_DIR='' SL_OWNER='' SL_TMP='' WF_TMP=''
HOSTS='' TSNAMES='' TSANSWERED=0 SCREENS_DATA='' OUT1='' RECS='' PAL_LIGHT='' PAL_DARK='' PANES='' LEGACY=''

# sock_init: the socket this process talks to, and the two files beside it.
# 1 when there is no server to talk to.
sock_init() {
  SOCK=${TMUX%%,*}
  [ -n "$SOCK" ] || SOCK=$(tmux display-message -p '#{socket_path}' 2>/dev/null) || return 1
  [ -S "$SOCK" ] || return 1
  LOCK="$SOCK.theme.lock"
  DIRTY="$SOCK.theme.dirty"
}

testhook() {
  [ -n "${THEME_SYNC_TESTHOOK:-}" ] && [ -x "$THEME_SYNC_TESTHOOK" ] || return 0
  "$THEME_SYNC_TESTHOOK" "$1" </dev/null >/dev/null 2>&1 || :
}

# on_exit: the EXIT trap of every theme.sh process. It does what the process
# holds at that moment calls for, and nothing when it holds nothing (a normal
# exit; a runner clears OWNER as it releases):
#   - a host lookup in flight: kill it and its watchdog, delete its output;
#   - a pass's batch file, or write_file's temp file: delete it;
#   - the screens lock: delete the temp file, release the lock;
#   - the sync lock: an interrupted pass is still owed, so the request is
#     re-created, the lock released, and a fresh runner started for it.
#     THEME_SYNC_GEN caps that relaunch chain at three, so a runner that dies
#     the same way every time cannot respawn forever.
# Every command is guarded: under set -e a failing last command here turns a
# clean exit 0 into 1 (measured, in sh and dash).
on_exit() {
  if [ -n "$TS_WD" ]; then kill "$TS_WD" 2>/dev/null || :; fi
  if [ -n "$TS_PID" ]; then kill "$TS_PID" 2>/dev/null || :; fi
  if [ -n "$TSOUT" ]; then rm -f "$TSOUT" 2>/dev/null || :; fi
  if [ -n "$BATCH" ]; then rm -f "$BATCH" 2>/dev/null || :; fi
  if [ -n "$WF_TMP" ]; then rm -f "$WF_TMP" 2>/dev/null || :; fi
  if [ -n "$SL_TMP" ]; then rm -f "$SL_TMP" 2>/dev/null || :; fi
  if [ -n "$SL_OWNER" ]; then
    rm -f "$SL_OWNER" 2>/dev/null || :
    rmdir "$SL_DIR" 2>/dev/null || :
  fi
  [ -n "$OWNER" ] || return 0
  if [ "$INFLIGHT" = 1 ]; then { true > "$DIRTY"; } 2>/dev/null || :; fi
  rm -f "$OWNER" 2>/dev/null || :
  rmdir "$LOCK" 2>/dev/null || :
  gen=${THEME_SYNC_GEN:-0}
  case $gen in ''|*[!0-9]*) gen=3 ;; esac
  if [ -e "$DIRTY" ] && [ "$gen" -lt 3 ]; then
    case $0 in /*) self=$0 ;; *) self="$(pwd)/$0" ;; esac
    THEME_SYNC_GEN=$((gen + 1)) "$self" sync </dev/null >/dev/null 2>&1 &
  fi
  return 0
}

# lock_stale DIR: true only when DIR's holder is provably gone (see above).
# Sets STALE_OWNER to the gone owner's pid, or '' for an ownerless lock.
lock_stale() {
  STALE_OWNER=
  for f in "$1"/owner.*; do
    [ -e "$f" ] || continue
    STALE_OWNER=${f##*/owner.}
    break
  done
  if [ -n "$STALE_OWNER" ]; then
    case $STALE_OWNER in *[!0-9]*) return 0 ;; esac   # not a pid: nobody owns it
    kill -0 "$STALE_OWNER" 2>/dev/null || return 0    # dead
    # Alive. Still the runner that took the lock? `read` is a regular
    # built-in, so a file that vanished is an ordinary failure here.
    ls_rec=
    { IFS= read -r ls_rec < "$f"; } 2>/dev/null || :
    [ -n "$ls_rec" ] || return 1                       # nothing recorded: alive is enough
    ls_cmd=$(ps -ww -o command= -p "$STALE_OWNER" 2>/dev/null) || ls_cmd=
    case $ls_cmd in
      '') kill -0 "$STALE_OWNER" 2>/dev/null && return 1; return 0 ;;   # gone, or ps is no help
      *"$ls_rec"*) return 1 ;;                         # the runner: never stale, at any age
    esac
    return 0                                           # someone else has its pid now
  fi
  [ -n "$(find "$1" -maxdepth 0 -mmin +2 2>/dev/null)" ]
}

# lock_break DIR: of several breakers, only the one whose rm of the dead
# owner's file succeeds goes on to remove the directory.
lock_break() {
  if [ -n "$STALE_OWNER" ]; then
    rm "$1/owner.$STALE_OWNER" 2>/dev/null || return 0
  else
    [ -n "$(find "$1" -maxdepth 0 -mmin +2 2>/dev/null)" ] || return 0
  fi
  rmdir "$1" 2>/dev/null || :
}

# sync_request: record a request, then become the runner if nobody is.
#
# Files are created with `true > F`, never `: > F`: `:` is a special built-in,
# and dash ends the whole shell when a special built-in's redirection fails,
# `|| …` or not (measured) — a runner would die holding its lock.
sync_request() {
  sock_init || return 0
  { true > "$DIRTY"; } 2>/dev/null || return 0
  breaks=0
  while :; do
    if mkdir "$LOCK" 2>/dev/null; then
      OWNER="$LOCK/owner.$$"
      # printf is a regular built-in, so a failed redirection is an ordinary
      # failure, not the end of the shell (see `true >` above).
      if ! { printf '%s\n' "$0" > "$OWNER"; } 2>/dev/null; then
        OWNER=; rmdir "$LOCK" 2>/dev/null || :
        return 0
      fi
      # From here on_exit treats this process as the runner (OWNER is set).
      testhook acquired
      fails=0 gave_up=
      while [ -e "$DIRTY" ]; do
        INFLIGHT=1                      # before the take: an abort from here re-marks
        rm -f "$DIRTY"
        testhook pass
        if sync_once; then
          INFLIGHT=0 fails=0
        else
          { true > "$DIRTY"; } 2>/dev/null || :
          INFLIGHT=0
          fails=$((fails + 1))
          if [ "$fails" -ge 3 ]; then gave_up=1; break; fi
          sleep 0.2
        fi
      done
      testhook before_release
      # OWNER is cleared only after the lock is gone: a signal in between
      # still finds a runner to release.
      rm -f "$OWNER"
      rmdir "$LOCK" 2>/dev/null || :
      OWNER=
      testhook released
      [ -z "$gave_up" ] || return 0     # the flag stays: owed to the next request
      [ -e "$DIRTY" ] || return 0       # re-check AFTER the release
      continue
    fi
    if [ "$breaks" -lt 5 ] && lock_stale "$LOCK"; then
      breaks=$((breaks + 1))
      lock_break "$LOCK"
      continue
    fi
    return 0                            # a live runner holds it and will serve us
  done
}

# --- files -------------------------------------------------------------------

# write_file FILE VALUE: a temp file in the same directory, then a rename, so
# a reader sees the old value or the new one, whole. WF_TMP is on_exit's to
# delete until the rename has happened: under set -e a failed write or rename
# ends the shell right there.
write_file() {
  mkdir -p "$CFG"
  WF_TMP="$CFG/.${1##*/}.tmp.$$"
  printf '%s\n' "$2" > "$WF_TMP"
  mv -f "$WF_TMP" "$1"
  WF_TMP=
}

pin() { write_file "$PIN" "$1"; write_file "$STATE" "$1"; }

# valid_client ARG: CLIENT is ARG when it looks like a client's tty, else ''.
# A client name is baked into menu commands and targets, so nothing else is
# ever let through.
valid_client() {
  CLIENT=
  case $1 in
    /dev/*[!A-Za-z0-9/._-]*) ;;
    /dev/?*) CLIENT=$1 ;;
  esac
}

# --- the screens file ----------------------------------------------------------
#
# ~/.config/tmux/screens holds the host rule: one "HOST MODE" line per host,
# below a one-line header comment, sorted by host. HOST is compared
# lowercase; MODE is light or dark, and auto is the absence of a line.
# Comments, blank lines and malformed lines are skipped, and when a host is
# listed twice the last line wins. Readers (sync, status, the menu) take no
# lock: the writer renames a whole new file into place, so a reader sees the
# old file or the new one, never half of either.

SCREENS_HEADER='# light/dark per host for tmux screens: HOST MODE (written by theme.sh screen)'

# SCREENS_AWK: the parser, shared by the reader and the writer. Given
# -v set=HOST -v to=MODE it applies one change on the way out (to=auto
# removes HOST); the order of its output is not defined, so writers sort.
# shellcheck disable=SC2016  # the $ in here are awk's, not the shell's
SCREENS_AWK='
  function host_ok(h) { return h ~ /^[a-z0-9][a-z0-9._:-]*$/ && length(h) <= 63 }
  /^[ \t]*(#|$)/ { next }
  NF == 2 {
    h = tolower($1); m = tolower($2)
    if (host_ok(h) && (m == "light" || m == "dark")) mode[h] = m
  }
  END {
    if (set != "") { if (to == "auto") delete mode[set]; else mode[set] = to }
    for (h in mode) print h, mode[h]
  }'

# screens_read: the screens file as "host mode" lines. Nothing, and 0, when
# there is no file; non-zero when it exists but cannot be read.
screens_read() {
  [ -e "$SCREENS" ] || return 0
  LC_ALL=C awk "$SCREENS_AWK" "$SCREENS"
}

# screens_set HOST MODE: one change to the screens file, made under the file's
# own lock, screens.lock beside it. A rename makes the new file appear whole,
# but it does not serialise a read-modify-write: two writers at once would
# each rename in a file that lacks the other's host. So the lock spans the
# read, the write and the rename. It is a directory holding owner.PID, broken
# only when its owner is provably gone (lock_stale, as for sync), and a live
# owner is waited for up to five seconds — a human, or a remote machine's
# push, can afford that; no hook ever takes this lock.
#
# Every failure leaves the file exactly as it was and returns 1 with one line
# on stderr: the lock stays busy, the old file exists but cannot be read
# (rewriting it from nothing would drop every other host), the temp file
# cannot be written, or the rename fails. The temp file sits in the same
# directory, so the rename never crosses a filesystem.
screens_set() {
  if ! mkdir -p "$CFG" 2>/dev/null; then
    printf 'theme.sh: cannot create %s\n' "$CFG" >&2
    return 1
  fi
  ss_lock="$CFG/screens.lock"
  ss_n=0
  until mkdir "$ss_lock" 2>/dev/null; do
    ss_n=$((ss_n + 1))
    if [ "$ss_n" -gt 100 ]; then                 # 100 x 50 ms: five seconds
      printf 'theme.sh: %s is busy\n' "$SCREENS" >&2
      return 1
    fi
    if lock_stale "$ss_lock"; then lock_break "$ss_lock"; else sleep 0.05; fi
  done
  # From here on_exit releases the lock if the process dies.
  SL_DIR=$ss_lock
  SL_OWNER="$ss_lock/owner.$$"
  SL_TMP="$CFG/.screens.tmp.$$"
  ss_rc=0
  { printf '%s\n' "$0" > "$SL_OWNER"; } 2>/dev/null || ss_rc=1
  [ ! -d "$SCREENS" ] || ss_rc=1
  ss_src=$SCREENS
  [ -e "$ss_src" ] || ss_src=/dev/null
  if [ "$ss_rc" = 0 ]; then
    ss_body=$(LC_ALL=C awk -v set="$1" -v to="$2" "$SCREENS_AWK" "$ss_src" 2>/dev/null) || ss_rc=1
  fi
  # printf and sort are not special built-ins, so a redirection that fails
  # here is an ordinary failure, not the end of the shell.
  if [ "$ss_rc" = 0 ]; then
    printf '%s\n' "$SCREENS_HEADER" 2>/dev/null > "$SL_TMP" || ss_rc=1
  fi
  if [ "$ss_rc" = 0 ] && [ -n "$ss_body" ]; then
    printf '%s\n' "$ss_body" | LC_ALL=C sort 2>/dev/null >> "$SL_TMP" || ss_rc=1
  fi
  if [ "$ss_rc" = 0 ]; then
    mv -f "$SL_TMP" "$SCREENS" 2>/dev/null || ss_rc=1
  fi
  rm -f "$SL_TMP" 2>/dev/null || :
  SL_TMP=
  rm -f "$SL_OWNER" 2>/dev/null || :
  rmdir "$SL_DIR" 2>/dev/null || :
  SL_OWNER='' SL_DIR=''
  if [ "$ss_rc" != 0 ]; then
    printf 'theme.sh: cannot write %s\n' "$SCREENS" >&2
    return 1
  fi
  return 0
}

# --- verbs ---------------------------------------------------------------------

# status: what sync would publish now, one line per screen. Read-only: a host
# it has to look up is looked up, but not cached.
status_cmd() {
  resolve_now || return 0
  printf '%s\n' "$RECS" | awk -F '\t' '$1 == "C" { print $2, $5, $6, $7 }'
}

# this_host [CLIENT]: THIS_HOST = the host of CLIENT, or, with no CLIENT, of
# the client tmux names (its best guess when several screens are attached).
# 1 when that client is not attached.
this_host() {
  valid_client "${1:-}"
  if [ -z "$CLIENT" ]; then valid_client "$(tmux display-message -p '#{client_name}' 2>/dev/null || :)"; fi
  [ -n "$CLIENT" ] || return 1
  resolve_now || return 1
  THIS_HOST=$(printf '%s\n' "$RECS" |
    awk -F '\t' -v c="$CLIENT" '$1 == "C" && ($2 == c || $3 == c) { print $5; exit }')
  [ -n "$THIS_HOST" ]
}

# screen HOST|this MODE [CLIENT]: set HOST's mode in the screens file (auto
# removes its line), then sync. Both words are compared lowercase. `this` is
# CLIENT's host; the menu never uses it, it bakes the host in, so a row means
# the same host however long the menu stays open. A bad HOST or MODE, or a
# file that cannot be written, exits 1 and leaves the file as it was.
screen_cmd() {
  sc_host=$(printf '%s' "${1:-}" | LC_ALL=C tr '[:upper:]' '[:lower:]')
  sc_mode=$(printf '%s' "${2:-}" | LC_ALL=C tr '[:upper:]' '[:lower:]')
  case $sc_mode in light|dark|auto) ;; *) usage; exit 1 ;; esac
  if [ "$sc_host" = this ]; then
    if ! this_host "${3:-}"; then
      printf 'theme.sh: no attached screen to call "this"\n' >&2
      exit 1
    fi
    sc_host=$THIS_HOST
  fi
  host_ok "$sc_host" || { usage; exit 1; }
  screens_set "$sc_host" "$sc_mode" || exit 1
  sync_request
}

# toggle [CLIENT]: flip the screen that asked. From the menu, CLIENT is the
# screen that pressed it, and toggle pins the opposite of THAT screen's mode,
# so the bar you are looking at always flips. With no CLIENT (the bare CLI),
# or one that is not attached, it pins the opposite of the server default.
toggle_cmd() {
  valid_client "${1:-}"
  cur=
  if [ -n "$CLIENT" ] && resolve_now; then
    cur=$(printf '%s\n' "$RECS" |
      awk -F '\t' -v c="$CLIENT" '$1 == "C" && ($2 == c || $3 == c) { print $6; exit }')
  fi
  if [ -z "$cur" ]; then
    cur=$(tmux show-options -gqv @theme_default 2>/dev/null) || cur=
    case $cur in light|dark) ;; *) cur=$(detect) ;; esac
  fi
  if [ "$cur" = light ]; then pin dark; else pin light; fi
  sync_request
}

# auto: the config-load entry point. The default is re-derived and kept; the
# chrome token is dropped, because Catppuccin has just re-baked status-style,
# message-style and the rest, so the next sync reinstalls the chrome over them;
# and the theme hooks of every OLDER config are removed.
#
# moox registers the theme hooks in ONE fixed slot, `NAME[100]` (conf/theme.conf,
# the slot table in architecture §5.1): an indexed `set-hook -g` replaces itself
# on every load, so an edit deploys on `prefix r` and nothing piles up — the old
# `%if @theme_hooks_v2` guard and the `@theme_hooks_text` cksum fingerprint that
# detected a frozen hook block are gone with the problem. What is left to clean
# is what older configs appended with `-ga` at other indexes: every hook entry
# that runs theme.sh and is NOT in slot 100 is unset, by its own index (hook
# arrays are sparse, so a foreign hook beside it stays), together with the old
# guard options.
load_cleanup() {
  out=$(tmux show-hooks -g 2>/dev/null) || out=
  set -f
  set -- set-option -gu @theme_chrome ';' set-option -gu @theme_hooks_installed \
    ';' set-option -gu @theme_hooks_v2 ';' set-option -gu @theme_hooks_text
  # show-hooks prints `NAME[IDX] command`.
  for ref in $(printf '%s\n' "$out" | awk '$1 ~ /^[a-z-]+\[[0-9]+\]$/ && $1 !~ /\[100\]$/ && /theme\.sh (sync|follow|refresh)/ { print $1 }'); do
    set -- "$@" ';' set-hook -gu "$ref"
  done
  set +f
  tmux "$@" >/dev/null 2>&1 || :
}

# --- interactive picker ----------------------------------------------------
#
# A native display-menu rather than fzf or a popup shell: it needs no
# dependency, opens instantly, and each entry applies on the spot, so the menu
# doubles as a preview — pick Dark, look at the bar, pick Light back.
#
#   This screen: HOST · MODE (RULE)    what the screen that opened it shows,
#     Auto / Light / Dark              and why; set or drop HOST's line in the
#                                      screens file (every screen on HOST)
#   All screens
#     Follow each screen / Light / Dark   the pin, for every screen
#   Toggle                             flip THIS screen (D18), by pinning
#   Tab colour ▸                       the Claude tab colour picker
#
# The dots mark what is in force: in "This screen", HOST's line in the
# screens file (Auto when it has none); in "All screens", the pin (Follow when
# there is none). The header names the rule that decided the screen's mode —
# a pin outranks a host line, so "dark (pin)" under "● Light" is the pin
# talking, not a stale menu.
#
# CLIENT is the screen that pressed prefix+T (the binding passes
# #{client_name}): the menu opens there, not on tmux's best guess, its header
# describes that screen, and its Toggle flips that screen. HOST and CLIENT
# are baked into the commands, because tmux expands a menu's commands as
# formats when it builds the menu (menu.c:130-136); both are validated
# (host_ok, valid_client), so neither can smuggle in a `#{…}`. Entries run $0
# by absolute path because run-shell executes with the server's environment,
# not an interactive shell's.

menu() {
  valid_client "${1:-}"
  if [ -z "$CLIENT" ]; then valid_client "$(tmux display-message -p '#{client_name}' 2>/dev/null || :)"; fi
  self=$(cd "$(dirname "$0")" && pwd)/$(basename "$0")
  tabsh=$(dirname "$self")/claude-tab-colour.sh

  # The screen that asked: its host, mode and the rule that decided it, as
  # `status` would print them. A client tmux does not list gets no section.
  t_host='' t_mode='' t_src=''
  if [ -n "$CLIENT" ] && resolve_now; then
    t_line=$(printf '%s\n' "$RECS" |
      awk -F '\t' -v c="$CLIENT" '$1 == "C" && ($2 == c || $3 == c) { print $5, $6, $7; exit }')
    if [ -n "$t_line" ]; then
      read -r t_host t_mode t_src <<EOF
$t_line
EOF
    fi
  fi
  host_ok "$t_host" || t_host=

  # Assembled into the positional parameters, in order, so that a section can
  # be left out whole — no "This screen" for a client tmux does not list, no
  # tab row when claude-tab-colour.sh is missing (a half-stowed checkout shows
  # no entry rather than one that opens nothing). `set --` is the POSIX way
  # to grow an argument list without re-splitting labels that contain spaces.
  # A label starting with `-` is a disabled row (a header); "" alone is a
  # separator.
  #
  # The `>/dev/null 2>&1 || true` on EVERY entry matches every other run-shell
  # in this setup: a job that prints nothing and exits 0 cannot reach the
  # proc_send path that segfaulted the server and took all 16 sessions with it
  # on 2026-08-16.
  set --
  if [ -n "$t_host" ]; then
    h_mode=$(printf '%s\n' "$SCREENS_DATA" | awk -v h="$t_host" '$1 == h { m = $2 } END { print m }')
    s_a="  Auto"; s_l="  Light"; s_d="  Dark"
    case $h_mode in
      light) s_l="● Light" ;;
      dark)  s_d="● Dark" ;;
      *)     s_a="● Auto" ;;
    esac
    set -- "-This screen: $t_host · $t_mode ($t_src)" "" "" \
      "$s_a" a "run-shell '$self screen $t_host auto >/dev/null 2>&1 || true'" \
      "$s_l" l "run-shell '$self screen $t_host light >/dev/null 2>&1 || true'" \
      "$s_d" d "run-shell '$self screen $t_host dark >/dev/null 2>&1 || true'" \
      ""
  fi

  # "Follow each screen" is `forget`: it clears the pin, otherwise detect()
  # would just hand back the very choice being discarded.
  read_mode "$PIN"
  m_f="  Follow each screen"; m_l="  Light"; m_d="  Dark"
  case $RM in
    light) m_l="● Light" ;;
    dark)  m_d="● Dark" ;;
    *)     m_f="● Follow each screen" ;;
  esac
  set -- "$@" "-All screens" "" "" \
    "$m_f" f "run-shell '$self forget >/dev/null 2>&1 || true'" \
    "$m_l" L "run-shell '$self light >/dev/null 2>&1 || true'" \
    "$m_d" D "run-shell '$self dark >/dev/null 2>&1 || true'" \
    "" \
    "Toggle" t "run-shell '$self toggle $CLIENT >/dev/null 2>&1 || true'" \
    ""

  # The per-window Claude tab colour picker hangs off this menu instead of
  # taking a key of its own. prefix+T is already the appearance entry point, and
  # the obvious mnemonic was not free: capital C is tmux's built-in
  # `customize-mode -Z`, so a binding for it would have clobbered a built-in
  # without saying so.
  if [ -x "$tabsh" ]; then
    set -- "$@" "Tab colour ▸" c "run-shell '$tabsh menu >/dev/null 2>&1 || true'"
  fi

  # `--` ends the options. Without it a FIRST item that is a disabled header
  # ("-This screen: …", or "-All screens") is parsed as an option cluster:
  # tmux read "-This …" as -T "his screen: …", which replaced the title and
  # swallowed the header row (measured on 3.7c, tests T2 and T9).
  tmux display-menu ${CLIENT:+-c "$CLIENT"} -T "#[align=centre] tmux theme " -x C -y C -- \
    "$@" "Cancel" q "" 2>/dev/null || :
}

usage() {
  printf '%s\n' "usage: theme.sh [light|dark|toggle [CLIENT]|forget|screen HOST|this light|dark|auto [CLIENT]|sync|follow|refresh [SESSION]|status|detect|auto|menu [CLIENT]]" >&2
}

# One trap for the life of the process, before any verb runs (see on_exit).
trap on_exit EXIT
trap 'exit 1' HUP INT TERM

case "${1:-auto}" in
  # Explicit choices pin, for every screen. These are the only entry points a
  # human uses.
  light|dark) pin "$1"; sync_request ;;
  toggle)     toggle_cmd "${2:-}" ;;
  # Drops the pin as well, otherwise "re-detect" would just hand back the very
  # choice it is meant to discard. The default is re-derived, then every
  # screen is handed back to its own terminal.
  forget)     rm -f "$STATE" "$PIN"; write_file "$STATE" "$(detect)"; sync_request ;;
  # One host's screens: the menu's "This screen" rows, or a remote push.
  screen)     screen_cmd "${2:-}" "${3:-}" "${4:-}" ;;
  sync|follow|refresh) sync_request ;;
  auto)       write_file "$STATE" "$(detect)"; load_cleanup; sync_request ;;
  status)     status_cmd ;;
  detect)     detect ;;
  menu)       menu "${2:-}" ;;
  *)          usage; exit 1 ;;
esac
