#!/bin/sh
# PostToolUse（edit_file、write_file）：改过的 .go 文件没有 gofmt 时把差异反馈给模型。文件已经写进去了，这里只能提醒。
path=$(jq -r '.tool_input.path // empty')
case "$path" in *.go) ;; *) exit 0 ;; esac
file="$AGENT_PROJECT_DIR/$path"
[ -f "$file" ] || exit 0
if ! out=$(gofmt -d "$file" 2>&1); then
  jq -n --arg r "gofmt 报错：$out" '{decision: "block", reason: $r}'
elif [ -n "$out" ]; then
  jq -n --arg r "$path 没有按 gofmt 格式化，差异如下（用 edit_file 改成 gofmt 的样子）：
$(printf '%s' "$out" | head -n 40)" '{decision: "block", reason: $r}'
fi
