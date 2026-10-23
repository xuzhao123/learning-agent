package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"learning-agent/internal/llm"
)

// D32：后台命令。bash 带 background=true 时不等它结束：在同样的沙箱里启动（同样的 cgroup 限制、断网与代理），
// 立刻返回 job_id；之后用 bash_output 读新输出、看结束没有，用 bash_kill 停掉。适合监视文件、跑很久的构建。
// 每条命令都在自己的沙箱里（自己的进程与网络），后台起的服务，之后的命令看不到也连不上；同 Claude Code 在 Linux 上的沙箱，
// 启动结果照它的说法告诉模型：要测服务，在同一条命令里起服务再访问；查看和停止只经 bash_output、bash_kill。
// 后台命令只活到这一轮结束：进程退出前 Close 把还在跑的全部按 cgroup 杀掉，和前台命令被取消时一样，沙箱里不留进程。
const (
	maxJobs      = 4       // 同时在跑的后台命令最多几个
	jobOutputMax = 1 << 20 // 每个后台命令最多留 1MB 输出，超出时丢掉最早的
	maxJobWait   = 20 * time.Second
)

type job struct {
	id      string
	cmd     *exec.Cmd
	unit    string
	started time.Time
	done    chan struct{} // 进程结束后关闭

	mu      sync.Mutex
	buf     []byte // 还留着的输出
	dropped int    // 超过上限被丢掉的字节数（从最早的开始）
	read    int    // 已经交给模型的位置：按全部输出算的偏移
	exit    int
	stopped string // 不是自己结束的：被 bash_kill 停掉，或本轮结束时被终止
}

func (j *job) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.buf = append(j.buf, p...)
	if extra := len(j.buf) - jobOutputMax; extra > 0 {
		j.buf, j.dropped = j.buf[extra:], j.dropped+extra
	}
	return len(p), nil
}

var (
	jobsMu sync.Mutex
	jobs   = map[string]*job{}
	jobSeq int
)

func startJob(ctx context.Context, callID, command string) (any, error) {
	jobsMu.Lock()
	running := 0
	for _, j := range jobs {
		select {
		case <-j.done:
		default:
			running++
		}
	}
	if running >= maxJobs {
		jobsMu.Unlock()
		return nil, llm.Permanent(fmt.Errorf("最多同时 %d 个后台命令；先用 bash_kill 停掉不用的", maxJobs))
	}
	jobSeq++
	j := &job{id: fmt.Sprintf("job-%d", jobSeq), started: time.Now(), done: make(chan struct{})}
	jobsMu.Unlock()
	// 和前台命令一样在锁里启动（共用工作目录与代理的初始化），启动完就放锁，不占着它等命令结束。
	mu.Lock()
	cmd, unit, err := launch(ctx, callID, command, j, ProjectWrite)
	mu.Unlock()
	if err != nil {
		return nil, err
	}
	j.cmd, j.unit = cmd, unit
	jobsMu.Lock()
	jobs[j.id] = j
	jobsMu.Unlock()
	go func() {
		cmd.Wait()
		j.mu.Lock()
		j.exit = cmd.ProcessState.ExitCode()
		j.mu.Unlock()
		close(j.done)
		fmt.Printf("Bash job [%s]: exited exit=%d after=%s\n", j.id, cmd.ProcessState.ExitCode(), time.Since(j.started).Round(time.Millisecond))
	}()
	fmt.Printf("Bash job [%s]: started call=%s\n", j.id, callID)
	return map[string]any{"job_id": j.id, "status": "running",
		"note": "命令在后台它自己的沙箱里运行：之后的命令看不到、也连不上它和它起的服务，所以不要用 ps、pgrep、pkill 或网络请求查它还在不在。" +
			"用 bash_output 读输出、看结束没有（可以带 wait 等几秒），用 bash_kill 停掉；这一轮结束时还在跑的会被终止。要测一个服务，在同一条命令里起服务再访问。"}, nil
}

func findJob(id string) (*job, error) {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	if j := jobs[strings.TrimSpace(id)]; j != nil {
		return j, nil
	}
	return nil, llm.Permanent(errors.New("没有这个后台命令：job_id 来自 bash 带 background=true 的返回；后台命令只在本轮内有效"))
}

// jobOutput 交回上次读到之后的新输出；wait 秒内命令结束了就提前返回。等待不超过 maxJobWait 和单条命令时限的一半。
func jobOutput(id string, wait int) (any, error) {
	j, err := findJob(id)
	if err != nil {
		return nil, err
	}
	if wait > 0 {
		limit := min(time.Duration(wait)*time.Second, maxJobWait, Timeout/2)
		select {
		case <-j.done:
		case <-time.After(limit):
		}
	}
	return j.snapshot(), nil
}

// jobKill 停掉一个后台命令（按 cgroup 整体结束），交回剩下的输出。已经结束的直接交回。
func jobKill(id string) (any, error) {
	j, err := findJob(id)
	if err != nil {
		return nil, err
	}
	j.stop("被 bash_kill 停掉")
	return j.snapshot(), nil
}

func (j *job) stop(why string) {
	select {
	case <-j.done:
		return
	default:
	}
	j.mu.Lock()
	j.stopped = why
	j.mu.Unlock()
	kill(j.cmd, j.unit)
	<-j.done
}

func (j *job) snapshot() map[string]any {
	j.mu.Lock()
	defer j.mu.Unlock()
	from, note := j.read-j.dropped, ""
	if from < 0 {
		note = fmt.Sprintf("有 %d 字节输出在读到之前就超过了 1MB 的上限，被丢掉了", -from)
		from = 0
	}
	out := j.buf[from:]
	j.read = j.dropped + len(j.buf)
	r := map[string]any{"job_id": j.id, "output": strings.ToValidUTF8(string(out), "�"), "output_bytes": len(out), "elapsed_ms": time.Since(j.started).Milliseconds()}
	select {
	case <-j.done:
		r["status"], r["exit_code"] = "exited", j.exit
		if j.stopped != "" {
			r["stopped"] = j.stopped
		}
	default:
		r["status"] = "running"
	}
	if note != "" {
		r["note"] = note
	}
	return r
}

// stopJobs 在进程退出前终止所有还在跑的后台命令（Close 调用）。
func stopJobs() {
	jobsMu.Lock()
	list := make([]*job, 0, len(jobs))
	for _, j := range jobs {
		list = append(list, j)
	}
	jobsMu.Unlock()
	for _, j := range list {
		j.stop("本轮结束时被终止")
	}
}
