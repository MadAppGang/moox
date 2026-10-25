#!/bin/sh
# Two-palette light/dark theming for the tmux status bar, per session.
#
#   theme.sh dark | light   pin a palette for EVERY session (the menu uses this)
#   theme.sh toggle         flip the pinned palette
#   theme.sh forget         drop the pin; every session follows its window again
#   theme.sh follow         give each session the palette of the window showing it
#   theme.sh refresh SESS   re-apply SESS's palette (a new window starts global)
#   theme.sh detect         print the server-wide default, "light" or "dark"
#   theme.sh auto           apply that default, then follow every client
#   theme.sh menu           the prefix+T picker
#
# Each session carries its own palette, and it follows the window showing it:
# a session attached from a dark iTerm window has a dark bar while one attached
# from a light window beside it stays light, and switching a window's profile
# re-skins its bar live. The hooks in .tmux.conf drive `follow`; see
# "Detection" below for where the answer comes from.
#
# Every apply also publishes TERM_THEME=light|dark into the environment new
# panes inherit (the session's own for a per-session apply, the global one for
# the default) — the shared variable tt, madbench and magmux honour ahead of
# their own probe.
#
# ---------------------------------------------------------------------------
# Why this works by setting @thm_* and nothing else
#
# Catppuccin resolves every colour it draws through `#{E:@thm_*}`, and the
# formats it stores keep those references UNEXPANDED, so they are resolved at
# draw time. Overwriting the ~26 @thm_* options therefore re-skins the entire
# bar live — verified: changing @thm_green alone flipped the session chip in
# `#{E:status-left}` with no plugin reload.
#
# That is the only supported way to switch. Setting @catppuccin_flavor and
# re-sourcing the plugin does NOT work: the palette files declare colours with
# `set -ogq`, and -o refuses to overwrite an option that already exists, so on
# a live server every line no-ops and the old palette stays.
#
# status-style is the one exception — the plugin computes it once at load with
# a literal value, so it has to be re-set explicitly here.
#
# ---------------------------------------------------------------------------
# Detection
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
# the old worry about racing it for keystrokes applies. Blink on a tablet
# reports nothing; its sessions keep whatever palette they had, else the global
# default.
#
# Per session, not per client: options live on sessions, so a session shown in
# two windows of different themes takes the most recent report.

set -e

STATE="${HOME}/.config/tmux/theme"
# Set only by a deliberate human choice (menu, toggle, explicit light/dark).
# detect() treats it as final, so auto-detection can never overturn it.
PIN="${HOME}/.config/tmux/theme.pinned"

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
# window-status-format note in .tmux.conf).
#
# No code change is needed to add entries here — apply() turns any "name
# value" line into `set -g @thm_<name>`, and the format chains in .tmux.conf
# resolve @thm_cc_* at DRAW time, which is why prefix+T re-skins every
# coloured tab for free and no per-window work is ever needed.

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

# --- apply -----------------------------------------------------------------

# apply MODE [SESSION]
#
# Without SESSION, sets the server-wide default (global options, global
# environment, the state file). With it, sets SESSION's own copy of the same
# options, which outranks the global one for that session only — verified at
# 3.7c: `set -t b @thm_x blue` over `set -g @thm_x red` draws blue in b's bar
# and red in every other.
apply() {
  mode="$1"
  sess="${2:-}"
  case "$mode" in
    light) pal=$(palette_light); fgbg="0;15" ;;
    dark)  pal=$(palette_dark);  fgbg="15;0" ;;
    *) echo "theme.sh: unknown mode '$mode'" >&2; exit 1 ;;
  esac
  if [ -n "$sess" ]; then
    # `=` makes the target an exact name: plain `-t a` also matches "aniflow".
    tmux has-session -t "=$sess" 2>/dev/null || return 0
    so="set -t \"=$sess:\""
    wins=$(tmux list-windows -t "=$sess" -F '#{window_id}')
  else
    so="set -g"
    wins=""
  fi

  # Literals come from the palette text, not read back from tmux: a session
  # apply must not pick up the global palette it is overriding.
  pv() { printf '%s\n' "$pal" | awk -v n="$1" '$1 == n { print $2; exit }'; }
  c_bg=$(pv bg);           c_fg=$(pv fg)
  c_crust=$(pv crust);     c_mantle=$(pv mantle)
  c_over0=$(pv overlay_0)
  c_yellow=$(pv yellow);   c_peach=$(pv peach)
  c_blue=$(pv blue);       c_lavender=$(pv lavender)

  # One batched command file; a single tmux invocation avoids a visible
  # repaint per colour across every attached client.
  cmds=$(mktemp "${TMPDIR:-/tmp}/tmux-theme.XXXXXX")
  {
    # shellcheck disable=SC2162
    printf '%s\n' "$pal" | while read name value; do
      [ -n "$name" ] || continue
      printf '%s @thm_%s "%s"\n' "$so" "$name" "$value"
    done

    # --- style options the plugin BAKES at load time -----------------------
    #
    # Everything above follows the palette for free, because the formats
    # Catppuccin stores keep `#{E:@thm_*}` unexpanded and resolve them at draw
    # time. These options do not: the plugin writes them with `set -gF`, or
    # with `set -gF` wrapped around an inner `E:`, both of which resolve
    # immediately and store a literal colour. They therefore freeze to
    # whichever palette was active during the last full config reload and
    # silently ignore every switch afterwards — a `theme.sh dark` was leaving a
    # cream #fbf1c7 popup and #bdae93 pane borders on a dark terminal.
    #
    # Three are SESSION options and the rest WINDOW options (measured at 3.7c
    # by setting each on a window target and listing where it landed), so a
    # session apply sets the first three on the session and the rest on each
    # of its windows. Windows created later start from the global values;
    # the after-new-window hook runs `refresh` for them.
    printf '%s status-style "bg=%s,fg=%s"\n' "$so" "$c_mantle" "$c_fg"

    # Message line — the bar you type into for prefix+, prefix+: rename-session
    # and every confirm-before. Deliberately the loudest thing this theme draws:
    # it is the one bar that is WAITING for an answer, so it should not be
    # mistakable for the status bar it replaces.
    #
    # Yellow because it is the only accent that is saturated in BOTH palettes
    # (#d79921 light, #f9e2af dark). Every other candidate — sapphire, mauve,
    # green — is a pale tint on the light side (see the palette note above) and
    # washes out against the cream background, which is exactly what the
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
    printf '%s message-style "fg=%s,bg=%s,fill=%s,bold"\n' "$so" "$c_crust" "$c_yellow" "$c_yellow"
    printf '%s message-command-style "fg=%s,bg=%s,fill=%s,bold"\n' "$so" "$c_crust" "$c_yellow" "$c_yellow"
    printf '%s @theme_mode "%s"\n' "$so" "$mode"

    if [ -n "$sess" ]; then
      for w in $wins; do window_styles "set -t $w"; done
      printf 'set-environment -t "=%s" TERM_THEME "%s"\n' "$sess" "$mode"
      printf 'set-environment -t "=%s" COLORFGBG "%s"\n' "$sess" "$fgbg"
    else
      window_styles "set -g"
      # Publish the decision as TERM_THEME so every NEW pane's programs (tt,
      # madbench, magmux, ...) can read it without asking the terminal. Global
      # env for the default; a per-session apply sets the session's own copy,
      # which new panes in that session inherit in preference to this one. It
      # must NOT be added to update-environment, or an attaching client without
      # it would erase it. Shells that already exist switch through their own
      # 2031 subscription (zsh/term-theme.zsh), not through this variable.
      printf 'set-environment -g TERM_THEME "%s"\n' "$mode"
    fi
  } > "$cmds"

  tmux source-file "$cmds"
  rm -f "$cmds"

  if [ -z "$sess" ]; then
    mkdir -p "$(dirname "$STATE")"
    printf '%s\n' "$mode" > "$STATE"
  fi
  tmux refresh-client -S 2>/dev/null || true
}

# The window-scoped styles, as command lines prefixed by $1 ("set -g" for the
# global default, "set -t @N" for one window). Reads the c_* set by apply().
window_styles() {
  # Copy-mode selection. Dark text on a strong yellow in both palettes, which
  # is the pairing that stays readable when it overpaints syntax-coloured text.
  printf '%s mode-style "bg=%s,fg=%s"\n' "$1" "$c_yellow" "$c_crust"
  printf '%s popup-style "bg=%s,fg=%s"\n' "$1" "$c_bg" "$c_fg"
  printf '%s popup-border-style "fg=%s"\n' "$1" "$c_over0"
  printf '%s menu-selected-style "fg=%s,bold,bg=%s"\n' "$1" "$c_fg" "$c_over0"
  # Borders are strokes, not fills, so they use the saturated stroke colours.
  # Active is blue rather than Catppuccin's lavender: lavender is a pale tint
  # in the light palette and vanishes as a one-pixel line on cream.
  printf '%s pane-border-style "fg=%s"\n' "$1" "$c_over0"
  printf '%s pane-active-border-style "#{?pane_in_mode,fg=%s,#{?pane_synchronized,fg=%s,fg=%s}}"\n' \
    "$1" "$c_yellow" "$c_peach" "$c_blue"
  printf '%s window-status-activity-style "bg=%s,fg=%s"\n' "$1" "$c_lavender" "$c_crust"
  printf '%s window-status-bell-style "bg=%s,fg=%s"\n' "$1" "$c_yellow" "$c_crust"
  printf '%s clock-mode-colour "%s"\n' "$1" "$c_blue"
}

# --- detect ----------------------------------------------------------------

detect() {
  # 1. A PINNED choice is final. Anything you select by hand — the menu, the
  #    toggle, `theme.sh light` — pins, and nothing auto-detected may overrule
  #    it. This is the opposite of the original design, which treated the
  #    terminal as the source of truth and let the next new shell overturn a
  #    manual pick. On this machine that was simply wrong, and it presented as
  #    "I chose light and it went dark by itself".
  if [ -f "$PIN" ]; then
    m=$(cat "$PIN")
    case "$m" in light|dark) echo "$m"; return ;; esac
  fi

  # 2. Last applied mode, so a reload is never a change.
  if [ -f "$STATE" ]; then
    m=$(cat "$STATE")
    case "$m" in light|dark) echo "$m"; return ;; esac
  fi

  # 3. The window most recently in use, as tmux measured it (see Detection).
  m=$(tmux list-clients -F '#{client_activity} #{client_theme}' 2>/dev/null |
    sort -rn | awk '$2 == "light" || $2 == "dark" { print $2; exit }')
  case "$m" in light|dark) echo "$m"; return ;; esac

  # 4. Default. macOS appearance is deliberately NOT consulted: it reports Dark
  #    on this machine while iTerm runs a light profile, so using it as the
  #    fallback made "Re-detect" reliably pick the wrong answer. The terminal
  #    owns this setting and the OS knows nothing about it.
  echo light
}

# --- interactive picker ----------------------------------------------------
#
# A native display-menu rather than fzf or a popup shell: it needs no
# dependency, opens instantly, and each entry applies on the spot, so the menu
# doubles as a preview — pick Dark, look at the bar, pick Light back.
#
# The dot marks what is in force: a pinned palette, or "Follow each window"
# when nothing is pinned. Entries run $0 by absolute path because
# run-shell executes with the server's environment, not an interactive shell's.

menu() {
  cur=$(tmux show -gv @theme_mode 2>/dev/null)
  self=$(cd "$(dirname "$0")" && pwd)/$(basename "$0")
  tabsh=$(dirname "$self")/claude-tab-colour.sh

  m_l="  Light"; m_d="  Dark"; m_f="  Follow each window"
  if [ ! -f "$PIN" ]; then m_f="● Follow each window"
  elif [ "$cur" = "light" ]; then m_l="● Light"
  else m_d="● Dark"
  fi

  # The per-window Claude tab colour picker hangs off this menu instead of
  # taking a key of its own. prefix+T is already the appearance entry point, and
  # the obvious mnemonic was not free: capital C is tmux's built-in
  # `customize-mode -Z`, so a binding for it would have clobbered a built-in
  # without saying so.
  #
  # Assembled into the positional parameters so that a missing
  # claude-tab-colour.sh drops the row entirely — a half-stowed checkout should
  # show no entry at all rather than one that opens nothing. `set --` is the
  # POSIX way to grow an argument list without re-splitting labels that contain
  # spaces, and "$@" expands to nothing when the row was never added.
  #
  # The `>/dev/null 2>&1 || true` matches every other run-shell in this setup:
  # a job that prints nothing and exits 0 cannot reach the proc_send path that
  # segfaulted the server and took all 16 sessions with it on 2026-08-16.
  set --
  if [ -x "$tabsh" ]; then
    set -- "Tab colour ▸" c "run-shell '$tabsh menu >/dev/null 2>&1 || true'" ""
  fi

  # "Follow each window" is `forget`: it clears the pin, otherwise detect()
  # would just hand back the very choice being discarded.
  tmux display-menu -T "#[align=centre] tmux theme " -x C -y C \
    "$m_f"                    f "run-shell '$self forget'" \
    "$m_l"                    l "run-shell '$self light'" \
    "$m_d"                    d "run-shell '$self dark'" \
    ""                                                    \
    "Toggle"                  t "run-shell '$self toggle'" \
    ""                                                    \
    "$@"                                                  \
    "Cancel"                  q ""
}

pin() { mkdir -p "$(dirname "$PIN")"; printf '%s\n' "$1" > "$PIN"; }

# Every session, one palette: what a pin means.
apply_everywhere() {
  apply "$1"
  tmux list-sessions -F '#{session_name}' 2>/dev/null | while IFS= read -r s; do
    apply "$1" "$s"
  done
}

# Give every attached session the palette of the window showing it. Run by
# the client-light-theme / client-dark-theme / client-attached /
# client-session-changed hooks. A pin wins; a client that has reported no
# theme (Blink) changes nothing; a session already in that palette is left
# alone, so a hook that changes nothing costs two tmux queries.
#
# All sessions rather than the one client the hook fired for, because the
# hook cannot say which client that was: measured at 3.7c, #{client_name} in a
# client-dark-theme hook expanded to a DIFFERENT client, so a dark window's
# session was never switched. A session shown in several windows takes the
# most recently active one.
#
# The background colour outranks #{client_theme}, as in zsh/term-theme.zsh.
# client_theme is the terminal's own light/dark LABEL (its 997 report), and
# foot under Omarchy is labelled dark in light themes too; Omarchy's retint
# changes the colour and never the label, so no client-*-theme hook fires.
# The colour is #{pane_bg} of the client's active pane: omarchy-theme-set-tmux
# writes the theme's colours into window-style on every switch, then the
# Omarchy theme-set hook (omarchy/.config/omarchy/hooks/theme-set.d/term-theme)
# runs this. With no window-style bg (macOS) pane_bg reads "default" and the
# label is all there is.
follow() {
  [ -f "$PIN" ] && return 0
  tmux list-clients -F '#{client_activity}|#{client_theme}|#{session_name}|#{pane_bg}' 2>/dev/null |
    sort -t'|' -k1,1rn |
    awk -F'|' '
      function hex(s,   i, n) {
        n = 0
        for (i = 1; i <= length(s); i++) n = n * 16 + index("0123456789abcdef", substr(s, i, 1)) - 1
        return n / 255
      }
      {
        bg = tolower($4)
        if (bg ~ /^#[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]$/)
          $2 = (0.2126 * hex(substr(bg, 2, 2)) + 0.7152 * hex(substr(bg, 4, 2)) + 0.0722 * hex(substr(bg, 6, 2)) > 0.5) ? "light" : "dark"
      }
      !seen[$3]++ && ($2 == "light" || $2 == "dark") { print $2 "|" $3 }' |
    while IFS='|' read -r theme sess; do
      [ "$(tmux show -qv -t "=$sess:" @theme_mode 2>/dev/null)" = "$theme" ] && continue
      apply "$theme" "$sess"
    done
}

# Re-apply SESS's own palette, if it has one. A new window starts with the
# global window styles, so the after-new-window hook calls this for its session.
refresh() {
  m=$(tmux show -qv -t "=$1:" @theme_mode 2>/dev/null)
  case "$m" in light|dark) apply "$m" "$1" ;; esac
}

case "${1:-auto}" in
  # Explicit choices pin, for every session. These are the only entry points a
  # human uses.
  light|dark) pin "$1"; apply_everywhere "$1" ;;
  toggle)
    cur=$(tmux show -gv @theme_mode 2>/dev/null)
    if [ "$cur" = "light" ]; then pin dark; apply_everywhere dark
    else pin light; apply_everywhere light; fi
    ;;
  follow)  follow ;;
  refresh) [ -n "${2:-}" ] && refresh "$2" ;;
  detect)  detect ;;
  auto)    apply "$(detect)"; follow ;;
  # Drops the pin as well, otherwise "re-detect" would just hand back the very
  # choice it is meant to discard. The global default is re-derived, then each
  # session is handed back to its window.
  forget)  rm -f "$STATE" "$PIN"; apply_everywhere "$(detect)"; follow ;;
  menu)    menu ;;
  *) echo "usage: theme.sh [light|dark|toggle|forget|follow|refresh SESSION|detect|auto|menu]" >&2; exit 1 ;;
esac
