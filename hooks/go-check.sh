#!/bin/sh
# Stop：Go 仓库里有未提交的 .go 改动时，编译和 vet 都通过才允许结束；没通过就把错误交给模型接着修。
cd "$AGENT_PROJECT_DIR" || exit 0
[ -f go.mod ] || exit 0
git status --porcelain -- '*.go' | grep -q . || exit 0
if ! out=$(go build ./... 2>&1 && go vet ./... 2>&1); then
  printf '工作区有未提交的 Go 改动，但 go build / go vet 没通过，先修好再结束：\n%s\n' "$(printf '%s' "$out" | head -n 30)" >&2
  exit 2
fi
