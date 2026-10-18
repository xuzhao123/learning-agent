#!/bin/sh
# PreToolUse（edit_file、write_file）：Go 约定带 “Code generated … DO NOT EDIT.” 的文件由工具生成，手改会被下次生成覆盖。
path=$(jq -r '.tool_input.path // empty')
file="$AGENT_PROJECT_DIR/$path"
[ -n "$path" ] && [ -f "$file" ] || exit 0
if head -n 5 "$file" | grep -Eq '^// Code generated .* DO NOT EDIT\.$'; then
  jq -n --arg p "$path" '{hookSpecificOutput: {hookEventName: "PreToolUse", permissionDecision: "deny",
    permissionDecisionReason: ($p + " 是生成的文件（开头有 Code generated … DO NOT EDIT.），请改生成它的源文件或生成器，再重新生成")}}'
fi
