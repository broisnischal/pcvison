#!/usr/bin/env bash
# Build pc and install it as a Claude Code plugin (skill + MCP server + `pc` on
# the Bash PATH), plus a `pc` command for my own shell.
#
#   bash install.sh              build, install, retire the old usepc/pcvision
#   bash install.sh --uninstall  remove the plugin, the marketplace and ~/.local/bin/pc
set -euo pipefail

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
bin="$here/plugin/bin/pc"
link="$HOME/.local/bin/pc"

if [ "${1:-}" = "--uninstall" ]; then
  "$bin" daemon stop 2>/dev/null || true
  claude plugin uninstall pc@pc 2>/dev/null || true
  claude plugin marketplace remove pc 2>/dev/null || true
  [ -L "$link" ] && rm -f "$link"
  echo "removed pc"
  exit 0
fi

command -v go >/dev/null || { echo "go is needed to build pc" >&2; exit 1; }
(cd "$here" && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o "$bin" ./cmd/pc)
echo "built   $bin ($(du -h "$bin" | cut -f1))"

mkdir -p "$(dirname "$link")"
ln -sf "$bin" "$link"
echo "linked  $link"

# The old Python tools this replaces: unregister them if they point into this checkout.
if claude mcp get pcvision >/dev/null 2>&1; then
  claude mcp remove pcvision -s user >/dev/null 2>&1 && echo "removed the pcvision MCP server"
fi
for old in "$HOME/.claude/skills/usepc" "$HOME/.local/bin/usepc" "$HOME/.local/bin/pcvision"; do
  if [ -L "$old" ] && [[ "$(readlink "$old")" == "$here"* ]]; then
    rm -f "$old" && echo "removed $old"
  fi
done

if claude plugin marketplace list 2>/dev/null | grep -qE '❯ pc$'; then
  claude plugin marketplace update pc >/dev/null 2>&1 || true
else
  claude plugin marketplace add "$here"
fi
claude plugin install pc@pc 2>/dev/null || claude plugin update pc@pc 2>/dev/null || true
echo

# a running daemon would keep serving the old binary
"$bin" daemon restart >/dev/null 2>&1 || true
"$bin" doctor || true
echo
echo "In a running Claude Code session, run /reload-plugins to pick it up."
