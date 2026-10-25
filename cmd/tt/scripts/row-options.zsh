#!/usr/bin/env zsh
# Colour preview of the status families (A-E) and line layouts (L1-L5) for the tt panel,
# in the panel's own palette (madbench-derived), dark or light. Truecolor escapes.
#
#   zsh row-options.zsh           dark (default)
#   TERM_THEME=light zsh row-options.zsh   (the dotfiles' shared theme variable, set in .zshenv)
#
# A design sheet, not the renderer: the rows are drawn here by hand so candidate
# looks can be compared side by side before one is built. `task tt:options` runs it.
# zsh, not bash, because macOS's /bin/bash (3.2) has no associative arrays.
#
# Only badges carry a background; everything else draws on the terminal's own
# background, exactly as the panel does. Pick the theme that matches the terminal.

THEME=${TERM_THEME:-dark}; [[ $THEME == light ]] || THEME=dark
B=$'\e[1m'
fg() { printf '\e[38;2;%d;%d;%dm' "0x${1:0:2}" "0x${1:2:2}" "0x${1:4:2}"; }
bg() { printf '\e[48;2;%d;%d;%dm' "0x${1:0:2}" "0x${1:2:2}" "0x${1:4:2}"; }

typeset -A HUE SBG LFG LBG VFG P
if [[ $THEME == light ]]; then
  INK=10121C; SNOW=FFFFFF; CLOUD=1E2030; SLATE=4E5364
  HUE=( busy 1D4ED8 work 1D4ED8 wait 6D28D9 blk B91C1C idle 4B4E8F sh 0E6C77 unk 4E5364 end 737889 )
  SBG=( busy DCE6FF work DCE6FF wait E5D8F8 blk F6D5D5 idle E4E5F2 sh D3ECEE unk E2E6F0 end ECEDF1 )
  # loud split badge: label half = dark hue text on a pale tint, value half = white on the hue
  LFG=( wait 441987 blk 721111 ); LBG=( wait E5D8F8 blk F6D5D5 ); LOUDINK=$SNOW
  P=( valbg D3D8E6 gutR C41E3A gutG 116B36 nameFg 145434 nameBg D6EEDD
      ctxW 92400E ctxO B4380A ctxR B91C1C ctxWbg F5E4CC ctxObg F7DCCB ctxRbg F6D5D5
      bandR F8E1E1 bandB E3EAF8 bandI EDEEF3 )
else
  INK=0B0B12; SNOW=FFFFFF; CLOUD=CDD6F4; SLATE=7A7F9A
  HUE=( busy 89B4FA work 89B4FA wait A78BFA blk FF4D5E idle A6A8D8 sh 5FD7C8 unk 9FA6C2 end 6E7290 )
  SBG=( busy 263241 work 263241 wait 32294B blk 4D171C idle 2A2B3D sh 1B3C38 unk 22252E end 1E1F28 )
  LFG=( wait FFFFFF blk FFFFFF ); LBG=( wait 4B3F70 blk 72222A ); LOUDINK=$INK
  P=( valbg 2E323D gutR FF5F5F gutG 5FD75F nameFg 8BD69A nameBg 1F3A28
      ctxW FFB454 ctxO FF8A3D ctxR FF5464 ctxWbg 473420 ctxObg 4A2A15 ctxRbg 4D171C
      bandR 3A1A1F bandB 1E2838 bandI 1B1D26 )
fi

R=$'\e[0m'
chip() { printf '%s%s%s%s%s' "$(fg "$1")" "$(bg "$2")" "${4:+$B}" "$3" "$R"; }   # FG BG TEXT [bold]
txt()  { printf '%s%s%s' "$(fg "$1")" "$2" "$R"; }

STATES=(busy work wait blk idle sh unk end)
typeset -A A=( busy RUN work WRK wait ASK blk BLK idle IDL sh "SH " unk "???" end END )
typeset -A Bw=( busy BUSY work WORK wait WAIT blk STOP idle IDLE sh SHEL unk UNKN end DONE )
typeset -A C=( busy ● work ◉ wait ◐ blk ✕ idle ○ sh › unk ◌ end ✓ )
typeset -A Dw=( busy run work work wait ask blk blocked idle idle sh shell unk unclear end done )
typeset -A E=( busy R work W wait A blk B idle I sh '$' unk '?' end ✓ )
attn() { [[ $1 == wait || $1 == blk ]]; }
badge() { # state text
  if attn "$1"; then chip "$LOUDINK" "${HUE[$1]}" " $2 " b; else chip "${HUE[$1]}" "${SBG[$1]}" " $2 "; fi; }
gut() { [[ -n $1 ]] && txt "$1" "▌" || printf ' '; }
ctx() { txt "$2" "$(printf '%5s' "$1")"; printf ' '; txt "$2" "$3"; }   # tokens colour bar
split() { # state runtime
  if attn "$1"; then chip "${LFG[$1]}" "${LBG[$1]}" " ${A[$1]} " b; chip "$LOUDINK" "${HUE[$1]}" "$(printf ' %5s ' "$2")" b
  else chip "${HUE[$1]}" "${SBG[$1]}" " ${A[$1]} "; chip "$CLOUD" "${P[valbg]}" "$(printf ' %5s ' "$2")"; fi; }
rows() { printf '\n%s%s%s\n' "$B" "$1" "$R"; }
band() { # bg state runtime name ctx ctxfg
  printf '%s%s%s %s %s' "$(bg "$1")" "$(fg "${HUE[$2]}")" "$B" "${A[$2]}" "$R$(bg "$1")"
  printf '%s %5s  %-24s' "$(fg "$CLOUD")" "$3" "$4"; printf '%s%6s %s\n' "$(fg "$6")" "$5" "$R"; }

sheet() {
  printf '%s\n%sStatus families%s  (solid = needs you, soft tint = everything else) · %s\n\n' "$R" "$B" "$R" "$THEME"
  printf '  %-8s %-7s %-8s %-4s %-12s %s\n' state A B C D E
  for s in "${STATES[@]}"; do
    printf '  %-8s ' "$s"
    badge "$s" "${A[$s]}"; printf '  '
    badge "$s" "${Bw[$s]}"; printf '  '
    txt "${HUE[$s]}" "${C[$s]}"; printf '    '
    txt "${HUE[$s]}" "$(printf '%-11s' "${C[$s]} ${Dw[$s]}")"; printf ' '
    badge "$s" "${E[$s]}"; printf '\n'
  done

  rows "L1 · split badge (status + runtime), ctx count + bar"
  gut ${P[gutR]}; split blk 2d04h; printf '  magmux-claude-sessions  '; ctx 371k ${P[ctxW]} ▰▰▱▱▱; echo
  printf '       '; txt ${P[ctxW]} "waiting 4h"; txt "$SLATE" " · confirm or override recommend…"; echo
  gut ${P[gutG]}; split busy 3h12m; printf ' '; chip ${P[nameFg]} ${P[nameBg]} " tt-ui-rebuil "; printf '           '; ctx 417k ${P[ctxO]} ▰▰▱▱▱; echo
  gut "";          split idle 6d02h; printf '  ssh-agent-91            '; ctx 598k ${P[ctxR]} ▰▰▰▱▱; echo

  rows "L2 · three separate badges"
  gut ${P[gutR]}; badge blk BLK; printf ' '; chip "$LOUDINK" "${HUE[blk]}" " 2d04h " b; printf '  magmux-claude-sessions '; chip ${P[ctxW]} ${P[ctxWbg]} " 371k "; echo
  gut ${P[gutG]}; badge busy RUN; printf ' '; chip "$CLOUD" ${P[valbg]} " 3h12m "; printf '  tt-ui-rebuil           '; chip ${P[ctxO]} ${P[ctxObg]} " 417k "; echo
  gut "";          badge idle IDL; printf ' '; chip "$CLOUD" ${P[valbg]} " 6d02h "; printf '  ssh-agent-91           '; chip ${P[ctxR]} ${P[ctxRbg]} " 598k "; echo

  rows "L3 · name first, badges right"
  gut ${P[gutR]}; printf 'magmux-claude-sessions   '; badge blk BLK; chip "$LOUDINK" "${HUE[blk]}" " 2d04h " b; printf ' '; txt ${P[ctxW]} 371k; echo
  gut ${P[gutG]}; txt ${P[nameFg]} 'tt-ui-rebuil             '; badge busy RUN; chip "$CLOUD" ${P[valbg]} " 3h12m "; printf ' '; txt ${P[ctxO]} 417k; echo
  gut "";          printf 'ssh-agent-91             '; badge idle IDL; chip "$CLOUD" ${P[valbg]} " 6d02h "; printf ' '; txt ${P[ctxR]} 598k; echo

  rows "L4 · glyph row + detail line (research pick)"
  gut ${P[gutR]}; txt "${HUE[blk]}" "✕ "; printf 'magmux-claude-sessions            '; txt ${P[ctxW]} "4h"; echo
  printf '    '; txt "$SLATE" "opus-5 · 2d04h · 4 sub   "; txt ${P[ctxW]} "▰▰▱▱▱ 37%"; echo
  gut ${P[gutG]}; txt "${HUE[busy]}" "● "; txt ${P[nameFg]} 'tt-ui-rebuil                      '; txt "$SLATE" "3m"; echo
  printf '    '; txt "$SLATE" "opus-5.5 · 3h12m · 2 sub "; txt ${P[ctxO]} "▰▰▱▱▱ 42%"; echo

  rows "L5 · tinted row band"
  band ${P[bandR]} blk 2d04h magmux-claude-sessions 371k ${P[ctxW]}
  band ${P[bandB]} busy 3h12m tt-ui-rebuil 417k ${P[ctxO]}
  band ${P[bandI]} idle 6d02h ssh-agent-91 598k ${P[ctxR]}
  echo
}

sheet
