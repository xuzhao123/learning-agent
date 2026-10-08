package agent

import (
	"context"
	"encoding/json"
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
	"learning-agent/internal/protocol"
	"learning-agent/internal/sandbox"
	"learning-agent/internal/telemetry"
)

// RunChild 保证一个任务ID对应的任务最终只完成一次，D10 队列和子 agent 共用：
//   - 已有 done 检查点：不启动进程，直接返回存档的答案，重复提交、重复派发都不会重复执行；
//   - 检查点可以续跑：用 -resume 接着跑，已完成的轮次不重做；
//   - 没有检查点：全新启动。
//
// 每个任务一个子进程：功能开关是包级变量、一个进程只跑一个任务；子进程崩溃也拖不垮调度方。
// 返回的错误按能否再试分类：llm.IsPermanent 为真时再跑也一样，否则可以稍后续跑。
//
// B0：子进程以 -app-server 启动，父进程就是它的界面：子进程的通知包一层 subagent/message 转给上级界面（callID 为空时，
// 比如队列，不转发）；子进程的请求（联网审批）转给上级界面，没有上级界面时如实回答“没人能批准”。给人看的日志写到 out。
func RunChild(ctx context.Context, id, question string, args []string, out io.Writer, callID string) (cp *Checkpoint, replayed bool, err error) {
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
	cmd := exec.CommandContext(ctx, self, append([]string{"-app-server"}, args...)...)
	// 协议走一对管道：子进程的 stdout → 父进程读，父进程写 → 子进程的 stdin。用 *os.File 而不是 io.Writer，Wait 不必等复制协程。
	fromChild, childOut, err := os.Pipe()
	if err != nil {
		return cp, false, err
	}
	childIn, toChild, err := os.Pipe()
	if err != nil {
		return cp, false, err
	}
	defer fromChild.Close()
	defer toChild.Close()
	cmd.Stdout, cmd.Stdin, cmd.Stderr = childOut, childIn, out
	var conn *protocol.Conn
	conn = protocol.NewConn(fromChild, toChild, func(m protocol.Message) {
		if m.IsRequest() {
			go relayRequest(ctx, conn, id, m)
			return
		}
		if callID != "" {
			protocol.Notify("subagent/message", map[string]any{"call_id": callID, "task_id": id, "message": m})
		}
	})
	// D13：TRACEPARENT 指向调用方当前的 span（spawn_agent 的 execute_tool，或队列的 queue_task），
	// 子进程的 invoke_agent 就挂在它下面。同名变量以后面的为准，覆盖从父进程继承来的那个。
	cmd.Env = append(os.Environ(), telemetry.Env(ctx)...)
	// 子进程放进自己的进程组：终端的 Ctrl+C 只发给调度方，再由调度方通过 ctx 把取消传下去，每个子进程只收到一次。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// 取消时先发 SIGINT，让子进程回填工具结果、写好检查点再退出；10 秒还没退出才强制结束。
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second
	if err := cmd.Start(); err != nil {
		childOut.Close()
		childIn.Close()
		return cp, false, err
	}
	childOut.Close() // 父进程这一侧不再需要子进程那一端；子进程退出后读到 EOF
	childIn.Close()
	runErr := cmd.Wait()
	select { // 读完子进程退出前写出的最后几条通知（turn/completed 等）
	case <-conn.Closed():
	case <-time.After(2 * time.Second):
	}
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

// 子 agent 发来的请求转给上级界面。目前只有联网审批：用户批准后，父进程自己的白名单也加上这个域名。
func relayRequest(ctx context.Context, conn *protocol.Conn, taskID string, m protocol.Message) {
	if m.Method != "network/requestApproval" {
		conn.Reply(*m.ID, nil, &protocol.Error{Code: -32601, Message: "未知方法 " + m.Method})
		return
	}
	params := map[string]any{}
	json.Unmarshal(m.Params, &params)
	params["task_id"] = taskID
	var answer struct {
		Decision string `json:"decision"`
	}
	if err := protocol.Call(ctx, m.Method, params, &answer); err != nil {
		answer.Decision = "unavailable"
	}
	if domain, ok := params["domain"].(string); ok && answer.Decision == "allow" {
		sandbox.AllowDomain(domain)
	}
	conn.Reply(*m.ID, answer, nil)
}
