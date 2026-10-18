---
description: 评审工作区里还没提交的改动，按严重程度列出问题，不修改文件
argument-hint: [关注点，可选]
---
评审工作区里还没提交的改动。

1. 在 bash 里运行 `git -C /workspace status --short` 和 `git -C /workspace diff`（没有 bash 时用 grep、read_file 看改动涉及的文件），弄清改了什么、为什么改。
2. 需要上下文时用 read_file 读改动所在的函数和直接调用方，不要只看 diff 片段下结论；查到能下结论就停，不另写程序复现。
3. 按“会出错的 › 可能出错的 › 可读性”排序，每条写清文件与行号、问题、触发条件和建议的改法；没有问题就直说没有。
4. 只评审，不修改任何文件。

额外关注点：$ARGUMENTS
