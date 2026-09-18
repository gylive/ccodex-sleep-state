#!/bin/sh
# Locate the executable relative to this file so the package works from any cwd.
set -u
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd) || exit 1
app="$script_dir/ccodex-sleep-state"
if [ ! -x "$app" ]; then app="$script_dir/../ccodex-sleep-state"; fi
if [ ! -x "$app" ]; then
  printf '%s\n' '未找到可执行的 ccodex-sleep-state。请完整解压 Linux 发布包。' >&2
  exit 1
fi
printf '%s\n' '正在启动。请保持这个终端打开；退出时按 Ctrl+C，以便恢复 Codex 配置。'
exec "$app" setup "$@"
