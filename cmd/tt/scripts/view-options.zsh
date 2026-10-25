#!/usr/bin/env zsh
# Design sheet for the WHOLE tt panel view: the summary header strip, five group-heading
# versions (H1-H5), and the footer (hidden-row badges + dim key chips). Rows use the chosen
# row look (status family A + layout L2 + L1 name colour). `task tt:views` runs it.
#
#   zsh view-options.zsh               follows $TERM_THEME (light or dark)
#
# Summary badges reuse the tmux status bar's glyphs and fills (statusline.go): bell = needs
# you, warning = error, question = unclear. Needs a Nerd Font for those three glyphs.
# zsh, not bash: macOS's /bin/bash (3.2) has no associative arrays.

THEME=${TERM_THEME:-dark}; [[ $THEME == light ]] || THEME=dark
B=$'\e[1m'; R=$'\e[0m'
fg() { printf '\e[38;2;%d;%d;%dm' "0x${1:0:2}" "0x${1:2:2}" "0x${1:4:2}"; }
bg() { printf '\e[48;2;%d;%d;%dm' "0x${1:0:2}" "0x${1:2:2}" "0x${1:4:2}"; }
chip() { printf '%s%s%s%s%s' "$(fg "$1")" "$(bg "$2")" "${4:+$B}" "$3" "$R"; }   # FG BG TEXT [bold]
txt()  { printf '%s%s%s' "$(fg "$1")" "$2" "$R"; }
rep()  { local s=''; repeat $2 s+="$1"; printf '%s' "$s"; }

BELL=$''; WARN=$''; QUES=$''

typeset -A P
if [[ $THEME == light ]]; then
  P=( cloud 1E2030 slate 4E5364 rule C9CED9
      blkS B91C1C askS 6D28D9 snow FFFFFF
      runF 116B36 runB D6EEDD idlF 1D4ED8 idlB DCE6FF valB D3D8E6
      ctxG 03664A ctxGb E1EFEB ctxW 6E300B ctxWb E9D9CF ctxR 8B1515 ctxRb F1D2D2
      nameGf 0E5B2E nameGb D9E7DF gutG 116B36
      headF 1E2030 headB E2E6F0 cntF 1E2030 cntB D3D8E6 band E9ECF3
      sWait d79921 sWaitI 11111b sErr cc241d sErrI ffffff sUnk bdae93 sUnkI 11111b
      keyF 737889 keyB ECEDF1 )
else
  P=( cloud CDD6F4 slate 7A7F9A rule 3C3F51
      blkS FF4D5E askS A78BFA snow 0B0B12
      runF 8FD9A8 runB 1E3326 idlF 89B4FA idlB 263241 valB 2E323D
      ctxG 8FD9A8 ctxGb 1E3326 ctxW FFB454 ctxWb 473420 ctxR FF5464 ctxRb 4D171C
      nameGf 8BD69A nameGb 1F3A28 gutG 5FD75F
      headF CDD6F4 headB 2E323D cntF CDD6F4 cntB 3C3F51 band 1E2130
      sWait fab387 sWaitI 11111b sErr ff5f5f sErrI 11111b sUnk 585b70 sUnkI ffffff
      keyF 6E7290 keyB 22252E )
fi
W=46

# ---- the chosen row look (A + L2), two sample rows reused under every heading ----
row_blk() { printf ' '; chip ${P[snow]} ${P[blkS]} " BLK " b; printf ' '; chip ${P[snow]} ${P[blkS]} " 2d16h " b
            printf '   tt-ui-rebuil         '; chip ${P[snow]} ${P[blkS]} " 582k " b; echo; }
row_run() { txt ${P[gutG]} '▌'; chip ${P[runF]} ${P[runB]} " RUN "; printf ' '; chip ${P[cloud]} ${P[valB]} "   11h "
            printf '  '; chip ${P[nameGf]} ${P[nameGb]} " madbecnh-skills "; printf '     '; chip ${P[ctxW]} ${P[ctxWb]} " 330k "; echo; }
row_idl() { printf ' '; chip ${P[idlF]} ${P[idlB]} " IDL "; printf ' '; chip ${P[cloud]} ${P[valB]} " 2d10h "
            printf '   dotfiles-a9          '; chip ${P[ctxG]} ${P[ctxGb]} " 117k "; echo; }
meta()    { printf '     '; txt ${P[slate]} "$1"; echo; }

# summary badges, as the tmux status bar draws them
s_wait() { chip ${P[sWaitI]} ${P[sWait]} " $BELL $1 " b; }
s_err()  { chip ${P[sErrI]}  ${P[sErr]}  " $WARN $1 " b; }
s_unk()  { chip ${P[sUnkI]}  ${P[sUnk]}  " $QUES $1 " b; }
label()  { chip ${P[headF]} ${P[headB]} " $1 " b; }
count()  { chip ${P[cntF]} ${P[cntB]} " $1 "; }
rule()   { txt ${P[rule]} "$(rep ─ $1)"; }

section() { printf '\n%s%s%s  %s\n' "$B" "$1" "$R" "$(txt ${P[slate]} "$2")"; }


# ================= CHOSEN: the whole panel, 46 columns =================
# header line 1 = grouping tabs, centred · line 2 = split count badges [ number | LABEL ]
# with the number brighter and the label dimmer, centred · headings = H3 with the
# summary badges in the count badge's neutral colour.
typeset -A S   # split-badge halves: <state>n = number half (fg bg), <state>l = label half (fg bg)
if [[ $THEME == light ]]; then
  S=( needNf FFFFFF needNb B91C1C needLf 8B1515 needLb F6D5D5
      runNf 0E5B2E runNb C2E3CD runLf 116B36 runLb E4F2E8
      idlNf 173EAD idlNb C9D6F7 idlLf 1D4ED8 idlLb E6ECFB
      unkNf 1E2030 unkNb D3D8E6 unkLf 4E5364 unkLb E6E9F0 )
else
  S=( needNf 0B0B12 needNb FF4D5E needLf FF8A93 needLb 4D171C
      runNf C8F0D4 runNb 2E5A3C runLf 8FD9A8 runLb 1C2E23
      idlNf C4D7FC idlNb 34496B idlLf 89B4FA idlLb 1E2838
      unkNf CDD6F4 unkNb 3C3F51 unkLf 9FA6C2 unkLb 2A2C38 )
fi
split2() { chip ${S[${1}Nf]} ${S[${1}Nb]} " $2 " b; chip ${S[${1}Lf]} ${S[${1}Lb]} " $3 "; }   # state number LABEL
tab_on()  { chip ${P[headF]} ${P[headB]} " $1 " b; }
tab_off() { txt ${P[slate]} " $1 "; }
nsum()    { chip ${P[cntF]} ${P[cntB]} " $1 $2 "; }            # neutral summary badge: glyph n
h3() { # label count left-rule right-rule [summary-pad summary...]
  rule $3; printf ' '; label $1; count $2; printf ' '; rule $4; echo; }

# ---- CHOSEN v2: tab bar · blank · joined count strip on a full-width band · plain centred
# section titles · rows with up to 3 dim lines of latest text · key strip on a band ----
if [[ $THEME == light ]]; then
  P+=( tabOnF FFFFFF tabOnB 0369A1 tabOffF 4E5364 tabOffB E2E6F0 latest 8A8FA3
       keyKf 4E5364 keyKb D8DCE6 keyLf 8A8FA3 keyLb E9ECF3 )
else
  P+=( tabOnF 0B0B12 tabOnB 5AC8FF tabOffF 9FA6C2 tabOffB 2A2C38 latest 5E6280
       keyKf 9FA6C2 keyKb 34374A keyLf 6E7290 keyLb 262837 )
fi
BAND=${P[band]}
onband() { printf '%s' "$(bg $BAND)"; }                     # re-enter the band after a chip reset
bpad()   { onband; printf '%s' "$(rep ' ' $1)"; }
bchip()  { chip "$@"; onband; }                            # a chip that returns to the band
bsplit() { bchip ${S[${1}Nf]} ${S[${1}Nb]} " $2 " b; bchip ${S[${1}Lf]} ${S[${1}Lb]} " $3 "; }
kchip()  { bchip ${P[keyKf]} ${P[keyKb]} " $1 " b; bchip ${P[keyLf]} ${P[keyLb]} " $2 "; }
tab()    { if [[ $2 == on ]]; then chip ${P[tabOnF]} ${P[tabOnB]} " $1 " b; else chip ${P[tabOffF]} ${P[tabOffB]} " $1 "; fi; }
title()  { rule $3; printf ' '; label "$1"; count $2; printf ' '; rule $4; echo; }   # plain centred title
latest() { printf '     '; txt ${P[latest]} "$1"; echo; }                          # dim, indented

PL=$''; PR=$''   # Nerd Font rounded caps (left, right)
strip() { bsplit need 2 NEED; bsplit run 3 RUN; bsplit idl 31 IDL; bsplit unk 5 '???'; printf '%s' "$R"; }  # 34 cols
latest_g() { txt $1 '▌'; printf '    '; txt ${P[latest]} "$2"; echo; }   # latest text under a coloured row: the bar continues

# ================= FINAL: the whole panel, 46 columns =================
# tabs = T1 (filled tabs on a baseline, thick coloured stretch under the active tab) with │
# dividers · blank · count strip = joined split badges with the S2 ░▒▓ fade either side ·
# blank · centred section titles · rows A + L2 with up to 3 dim lines of the latest text (the
# /color bar runs down through them) · keys = one joined strip of dim split badges.
tabbar() { # active tab first arg; widths: " Project " 9, " Status " 8, " Tmux " 6, " Colour " 8, dividers 3 → 34
  printf '  '
  local first=1 t k lbl U=$'\e[4m' NU=$'\e[24m'
  local -A HK=( Project 1 Status 2 Tmux 2 Colour 1 )
  for t in Project Status Tmux Colour; do
    (( first )) || chip ${P[rule]} ${P[tabOffB]} '│'
    first=0
    # the hotkey letter is underlined. First letter unless it is already taken (q g d r s are
    # the sidebar's other keys), then the next letter of the word: p, t (st̲atus), m (tm̲ux), c.
    k=${HK[$t]}
    lbl="${t[1,k-1]}[${t[k]}]${t[k+1,-1]}"
    if [[ $t == $1 ]]; then chip ${P[tabOnF]} ${P[tabOnB]} " $lbl " b
    else chip ${P[tabOffF]} ${P[tabOffB]} " $lbl "; fi
  done; echo
  # baseline: thin rule, thick coloured under the active tab (Project = cols 6..14)
  rule 2; txt ${P[tabOnB]} "$(rep ━ 11)"; rule 33; echo
}
keystrip() { # joined, no gaps: [g|group][d|ended][r|refresh] = 32 cols, centred, K4 ══ either side
  txt ${P[rule]} '══════ '
  chip ${P[keyKf]} ${P[keyKb]} " g " b; chip ${P[keyLf]} ${P[keyLb]} " group "
  chip ${P[keyKf]} ${P[keyKb]} " d " b; chip ${P[keyLf]} ${P[keyLb]} " ended "
  chip ${P[keyKf]} ${P[keyKb]} " r " b; chip ${P[keyLf]} ${P[keyLb]} " refresh "
  txt ${P[rule]} ' ══════'; echo
}

echo
tabbar Project
txt ${P[rule]} '░░▒▒▓'; printf ' '; strip; printf ' '; txt ${P[rule]} '▓▒▒░░'; echo
echo; echo
title "NEEDS YOU" 2 15 15
row_blk
latest "answer: Which \"herd\" do you want set up?"
latest "(Laravel Herd (Recommended) · Something"
latest "else) — waiting 2d"
printf ' '; chip ${P[snow]} ${P[askS]} " ASK " b; printf ' '; chip ${P[snow]} ${P[askS]} " 2d10h " b
printf '   attacemtns-slangs    '; chip ${P[snow]} ${P[blkS]} " 482k " b; echo
latest "permission-prompt: Bash(rm -rf dist)"
echo
title dotfiles 5 15 16
row_idl
latest "Committed the zsh prompt change; the deep"
latest "path segment now shortens to …/tt-ui"
row_run
latest_g ${P[gutG]} "Running go test ./... in tools/tt"
latest_g ${P[gutG]} "ok  tools/tt  16.1s · 21 packages"
echo
title magus-src 6 15 15
row_idl; row_run
echo; echo
keystrip

# ---- options, only with `view-options.zsh options` (task tt:views -- options) ----
[[ $1 == options ]] || exit 0
keyonly() { chip ${P[keyKf]} ${P[keyKb]} " g " b; chip ${P[keyLf]} ${P[keyLb]} " group "
            chip ${P[keyKf]} ${P[keyKb]} " d " b; chip ${P[keyLf]} ${P[keyLb]} " ended "
            chip ${P[keyKf]} ${P[keyKb]} " r " b; chip ${P[keyLf]} ${P[keyLb]} " refresh "; }   # 32 cols
kopt() { printf '%s\n' "$(txt ${P[slate]} "$1")"; txt ${P[rule]} "$2"; keyonly; txt ${P[rule]} "$3"; echo; }
printf '\n\n%s\n' "$(txt ${P[slate]} "$(rep ┄ 46)")"
section "ASK colour · ten options" "(A = first five, B = orange-ish and green-ish)"
typeset -A AF AI AN
if [[ $THEME == light ]]; then
  AF=( A1 D97706 A2 C2410C A3 6D28D9 A4 CA8A04 A5 BE185D
       B1 EA580C B2 F59E0B B3 B45309 B4 0F766E B5 65A30D )
  AI=( A1 1C1917 A2 FFFFFF A3 FFFFFF A4 1C1917 A5 FFFFFF
       B1 1C1917 B2 1C1917 B3 FFFFFF B4 FFFFFF B5 1C1917 )
else
  AF=( A1 FBBF24 A2 FB923C A3 A78BFA A4 FACC15 A5 F472B6
       B1 FDBA74 B2 FCD34D B3 F97316 B4 2DD4BF B5 A3E635 )
  AI=( A1 1C1917 A2 1C1917 A3 1C1917 A4 1C1917 A5 1C1917
       B1 1C1917 B2 1C1917 B3 1C1917 B4 1C1917 B5 1C1917 )
fi
AN=( A1 amber A2 orange A3 violet A4 gold A5 magenta
     B1 tangerine B2 apricot B3 burnt-orange B4 teal B5 lime )
for a in A1 A2 A3 A4 A5 B1 B2 B3 B4 B5; do
  printf ' '; chip ${AI[$a]} ${AF[$a]} " ASK " b; printf ' '; chip ${AI[$a]} ${AF[$a]} " 2d10h " b
  printf '   %-20s' "$a ${AN[$a]}"; chip ${P[snow]} ${P[blkS]} " 482k " b; echo
done
printf '%s\n' "$(txt ${P[slate]} '  for reference: BLK and RUN as they are')"
printf ' '; chip ${P[snow]} ${P[blkS]} " BLK " b; printf ' '; chip ${P[snow]} ${P[blkS]} " 2d16h " b; echo
txt ${P[gutG]} '▌'; chip ${P[runF]} ${P[runB]} " RUN "; printf ' '; chip ${P[cloud]} ${P[valB]} "   11h "; echo
printf '%s\n' "$(txt ${P[slate]} '  now: violet (chosen 2026-10-21)')"
printf ' '; chip ${P[snow]} ${P[askS]} " ASK " b; printf ' '; chip ${P[snow]} ${P[askS]} " 2d10h " b; echo
