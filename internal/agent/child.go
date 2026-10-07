package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"learning-agent/internal/llm"
	"learning-agent/internal/telemetry"
)

// RunChild 保证一个任务ID对应的任务最终只完成一次，D10 队列和子 agent 共用：
//   - 已有 done 检查点：不启动进程，直接返回存档的答案，重复提交、重复派发都不会重复执行；
//   - 检查点可以续跑：用 -resume 接着跑，已完成的轮次不重做；
//   - 没有检查点：全新启动。
//
// 每个任务一个子进程：功能开关是包级变量、一个进程只跑一个任务；子进程崩溃也拖不垮调度方。
// 返回的错误按能否再试分类：llm.IsPermanent 为真时再跑也一样，否则可以稍后续跑。
func RunChild(ctx context.Context, id, question string, args []string, out io.Writer) (cp *Checkpoint, replayed bool, err error) {
	cp, err = LoadCheckpoint(id)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		args = append([]string{"-task-id", id, "-question", question}, args...)
	case err != nil:
		return nil, false, llm.Permanent(err)
	case cp.Question != question:
		// 同一个幂等键配了不同的请求，多半是调用方出错；不能把旧答案当成新问题的结果。
		return cp, false, llm.Permanent(fmt.Errorf("任务ID %s 已用于另一个问题，不能复用", id))
	case cp.Status == "done":
		return cp, true, nil
	case !Resumable(cp):
		return cp, false, llm.Permanent(fmt.Errorf("任务 %s 已停止（%s），续跑结果也一样", id, cp.Reason))
	default:
		args = []string{"-resume", id}
	}
	// 先试一下锁：别的进程正在跑这个任务就直接返回，不启动第二个。子进程启动后会自己再加锁。
	unlock, err := LockCheckpoint(id)
	if err != nil {
		return cp, false, llm.Permanent(err)
	}
	unlock()
	self, err := os.Executable()
	if err != nil {
		return cp, false, err
	}
	cmd := exec.CommandContext(ctx, self, args...)
	cmd.Stdout, cmd.Stderr = out, out
	// D13：TRACEPARENT 指向调用方当前的 span（spawn_agent 的 execute_tool，或队列的 queue_task），
	// 子进程的 invoke_agent 就挂在它下面。同名变量以后面的为准，覆盖从父进程继承来的那个。
	cmd.Env = append(os.Environ(), telemetry.Env(ctx)...)
	// 子进程放进自己的进程组：终端的 Ctrl+C 只发给调度方，再由调度方通过 ctx 把取消传下去，每个子进程只收到一次。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// 取消时先发 SIGINT，让子进程回填工具结果、写好检查点再退出；10 秒还没退出才强制结束。
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	runErr := cmd.Run()
	cp, err = LoadCheckpoint(id)
	if err != nil {
		// 连检查点都没写出来：多半是参数错误，再启动一次也一样。
		return nil, false, llm.Permanent(fmt.Errorf("任务 %s 没有留下检查点：%w", id, errors.Join(runErr, err)))
	}
	if cp.Status == "done" {
		return cp, false, nil
	}
	reason := cp.Reason
	if cp.Status == "running" {
		reason = "进程中途退出"
	}
	err = fmt.Errorf("任务 %s 未完成（%s）：%s", id, reason, firstLine(cp.Error))
	if !Resumable(cp) {
		err = llm.Permanent(err)
	}
	return cp, false, err
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
