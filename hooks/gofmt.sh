#!/bin/sh
# PostToolUse（edit_file、write_file）：改过的 .go 文件直接 gofmt -w。harness 把这次改写算进写入（快照、读取账本跟着更新），
# 并把改动片段告诉模型；gofmt 报错（语法错误）时把错误反馈给模型。
path=$(jq -r '.tool_input.path // empty')
case "$path" in *.go) ;; *) exit 0 ;; esac
file="$AGENT_PROJECT_DIR/$path"
[ -f "$file" ] || exit 0
if ! out=$(gofmt -l "$file" 2>&1); then
  jq -n --arg r "gofmt 报错：$out" '{decision: "block", reason: $r}'
elif [ -n "$out" ]; then
  gofmt -w "$file"
fi
