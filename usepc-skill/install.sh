#!/usr/bin/env bash
# Install usepc as a Claude skill plus a CLI on PATH.
#
#   bash install.sh            symlink (edits here take effect immediately)
#   bash install.sh --copy     copy instead, for machines without the checkout
#   bash install.sh --project  install into ./.claude/skills of the current repo
set -euo pipefail

here="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
mode="link"
scope="user"
for arg in "$@"; do
  case "$arg" in
    --copy) mode="copy" ;;
    --link) mode="link" ;;
    --project) scope="project" ;;
    --user) scope="user" ;;
    -h|--help) sed -n '2,8p' "$0"; exit 0 ;;
    *) echo "unknown option: $arg" >&2; exit 2 ;;
  esac
done

if [ "$scope" = "project" ]; then
  skills_dir="$PWD/.claude/skills"
else
  skills_dir="$HOME/.claude/skills"
fi
target="$skills_dir/usepc"
bin_dir="$HOME/.local/bin"

mkdir -p "$skills_dir" "$bin_dir"

if [ -e "$target" ] || [ -L "$target" ]; then
  echo "replacing existing $target"
  rm -rf "$target"
fi

if [ "$mode" = "link" ]; then
  ln -s "$here" "$target"
  echo "linked  $target -> $here"
else
  cp -r "$here" "$target"
  echo "copied  $here -> $target"
fi

ln -sf "$here/scripts/usepc" "$bin_dir/usepc"
chmod +x "$here/scripts/usepc" "$here/scripts/usepc_ctl.py"
echo "cli     $bin_dir/usepc"

case ":$PATH:" in
  *":$bin_dir:"*) ;;
  *) echo "note    $bin_dir is not on your PATH — add it to your shell rc" ;;
esac

echo
"$here/scripts/usepc" doctor || true
echo
echo "Restart Claude Code (or run /doctor) so the skill is picked up."
