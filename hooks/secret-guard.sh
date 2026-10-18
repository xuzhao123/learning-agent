#!/bin/sh
# UserPromptSubmit：问题里像是粘贴了 API 密钥时拦下，不发给模型（密钥只放 .env）。
prompt=$(jq -r '.prompt')
if printf '%s' "$prompt" | grep -Eq '(sk-[A-Za-z0-9_-]{20,}|AKIA[0-9A-Z]{16}|-----BEGIN [A-Z ]*PRIVATE KEY-----)'; then
  echo "问题里像是包含密钥或私钥。模型请求会发到外部服务，请删掉密钥再提交；需要用时放进 .env。" >&2
  exit 2
fi
