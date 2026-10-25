# moox.zsh — `moox setup` adds one guarded line to ~/.zshrc that sources this:
#   [[ -r "<moox share root>/shell/moox.zsh" ]] && source "<moox share root>/shell/moox.zsh"
# It loads the vendored continuity hook from the same share root, so the rc
# line never names a plugin path. Safe to source more than once.
source "${${(%):-%x}:A:h}/../plugins/tmux-claude-continuity/claude-continuity.zsh"
