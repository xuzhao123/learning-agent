package queue

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"learning-agent/internal/agent"
	"learning-agent/internal/llm"
	"learning-agent/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// D10 任务队列：任务文件 → channel → 固定数量的 worker → 每个任务一个 agent 子进程（agent.RunChild）。
// 幂等靠任务ID：任务ID就是检查点ID，任务状态全在检查点文件里，队列自己不另存一份状态。
// 所以重复提交、重跑整个队列、队列进程中途被杀，都不会让已完成的任务再执行一次。
type Task struct {
	ID       string   `json:"id"`       // 幂等键；不给时用问题和参数的哈希
	Question string   `json:"question"` // 交给 agent 的问题
	Args     []string `json:"args"`     // 这个任务额外的 agent 参数，如 ["-lab-tools"]
}

type Outcome struct {
	Status   string // done、replayed（之前已完成，本次没执行）、failed、interrupted
	Calls    int
	Elapsed  time.Duration
	Attempts int
	Detail   string
}

func Run(arguments []string) error {
	flags := flag.NewFlagSet("queue", flag.ContinueOnError)
	workers := flags.Int("workers", 3, "同时运行的任务数，1到8")
	attempts := flags.Int("attempts", 3, "每个任务最多启动几次子进程（含续跑），1到5")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	rest := flags.Args()
	if len(rest) == 0 || *workers < 1 || *workers > 8 || *attempts < 1 || *attempts > 5 {
		return errors.New("用法：go run . queue [-workers 3] [-attempts 3] 任务文件.jsonl [-- 公共agent参数]")
	}
	file, extra := rest[0], rest[1:]
	if len(extra) > 0 && extra[0] == "--" {
		extra = extra[1:]
	}
	tasks, err := load(file)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// D13：跑一次队列是一个 trace：根 span invoke_workflow，每个任务一个 queue_task，任务的 agent 子进程挂在 queue_task 下。
	ctx, span := telemetry.Begin(ctx, "invoke_workflow queue", trace.SpanKindInternal, semconv.GenAIOperationNameInvokeWorkflow,
		attribute.String("gen_ai.workflow.name", "queue"), attribute.Int("queue.tasks", len(tasks)), attribute.Int("queue.workers", *workers))
	defer span.End()
	fmt.Printf("Queue: tasks=%d workers=%d attempts=%d common_args=%q logs=%s\n", len(tasks), *workers, *attempts, extra, filepath.Join(".data", "checkpoints", "<id>.log"))
	started := time.Now()
	results := make([]Outcome, len(tasks))
	jobs := make(chan int)
	var wg sync.WaitGroup
	// 有界并发：worker 数就是同时在跑的子进程数（也就是同时在请求模型的任务数）的上限。
	for range *workers {
		wg.Go(func() {
			for i := range jobs {
				results[i] = runTask(ctx, tasks[i], extra, *attempts)
			}
		})
	}
dispatch:
	for i := range tasks {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break dispatch // 收到 Ctrl+C 后不再派发新任务；在跑的任务由 RunChild 转发 SIGINT
		}
	}
	close(jobs)
	wg.Wait()
	err = report(tasks, results, time.Since(started))
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}

// 每行一个 JSON 任务；空行和 # 开头的行跳过。同一个ID在文件里出现两次，只入队第一次。
func load(path string) ([]Task, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var tasks []Task
	seen := map[string]int{}
	scanner := bufio.NewScanner(f)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var t Task
		if err := json.Unmarshal([]byte(text), &t); err != nil || strings.TrimSpace(t.Question) == "" {
			return nil, fmt.Errorf("%s 第%d行需要 {\"id\":…, \"question\":…}", path, line)
		}
		t.Question = strings.TrimSpace(t.Question)
		if t.ID == "" {
			// 内容寻址：同样的问题和参数得到同样的ID，不写ID也能去重。
			sum := sha256.Sum256([]byte(t.Question + "\x00" + strings.Join(t.Args, "\x00")))
			t.ID = "q-" + hex.EncodeToString(sum[:6])
		}
		if !agent.ValidID(t.ID) {
			return nil, fmt.Errorf("%s 第%d行：任务ID只能包含字母、数字和 ._-", path, line)
		}
		if first, ok := seen[t.ID]; ok {
			fmt.Printf("Queue duplicate: id=%s line=%d 与第%d行重复，只入队一次\n", t.ID, line, first)
			continue
		}
		seen[t.ID] = line
		tasks = append(tasks, t)
	}
	return tasks, scanner.Err()
}

func runTask(ctx context.Context, t Task, extra []string, attempts int) Outcome {
	// 10 个任务的过程输出混在终端里没法读：每个任务写自己的日志，终端只打印状态变化。
	_ = os.MkdirAll(filepath.Join(".data", "checkpoints"), 0o700)
	log, err := os.OpenFile(filepath.Join(".data", "checkpoints", t.ID+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return Outcome{Status: "failed", Detail: err.Error()}
	}
	defer log.Close()
	return RunTask(ctx, t, extra, attempts, agent.ChildIO{Out: log})
}

// RunTask 执行一个任务：暂时性失败退避后续跑，最多启动 attempts 次子进程。队列与 D16 评测共用。
func RunTask(ctx context.Context, t Task, extra []string, attempts int, child agent.ChildIO) (result Outcome) {
	ctx, span := telemetry.Begin(ctx, "queue_task "+t.ID, trace.SpanKindInternal, attribute.String("queue.task.id", t.ID))
	defer func() {
		span.SetAttributes(attribute.String("queue.task.status", result.Status), attribute.Int("queue.task.attempts", result.Attempts), attribute.Int("agent.model_calls", result.Calls))
		var err error
		if result.Status != "done" && result.Status != "replayed" {
			err = errors.New(result.Detail)
		}
		telemetry.End(span, err, result.Status)
	}()
	start := time.Now()
	args := append(append([]string(nil), t.Args...), extra...)
	for attempt := 1; attempt <= attempts; attempt++ {
		result.Attempts = attempt
		fmt.Printf("Queue start: id=%s attempt=%d/%d\n", t.ID, attempt, attempts)
		fmt.Fprintf(child.Out, "\n===== %s attempt %d =====\n", time.Now().Format(time.RFC3339), attempt)
		cp, replayed, err := agent.RunChild(ctx, t.ID, t.Question, args, child)
		result.Elapsed = time.Since(start)
		if cp != nil {
			result.Calls = cp.Calls
		}
		switch {
		case replayed:
			result.Status, result.Detail = "replayed", clip(cp.Answer)
			fmt.Printf("Queue skip: id=%s 已完成，直接取存档答案（不重复执行）\n", t.ID)
			return result
		case err == nil:
			result.Status, result.Detail = "done", clip(cp.Answer)
			fmt.Printf("Queue done: id=%s model_calls=%d elapsed=%s\n", t.ID, cp.Calls, result.Elapsed.Round(100*time.Millisecond))
			return result
		case ctx.Err() != nil:
			result.Status, result.Detail = "interrupted", "队列被中断；重新运行队列会从检查点续跑"
			fmt.Printf("Queue interrupted: id=%s\n", t.ID)
			return result
		case llm.IsPermanent(err) || attempt == attempts:
			result.Status, result.Detail = "failed", err.Error()
			fmt.Printf("Queue failed: id=%s error=%s\n", t.ID, err)
			return result
		}
		// 暂时性失败（模型请求失败、子进程中途退出）：退避后用 -resume 续跑，不从头再来。
		delay := time.Second << (attempt - 1)
		fmt.Printf("Queue retry: id=%s error=%s wait=%s\n", t.ID, err, delay)
		span.AddEvent("retry", trace.WithAttributes(attribute.Int("attempt", attempt), attribute.String("error", telemetry.Clip(err.Error(), 300)), attribute.Int64("wait_ms", delay.Milliseconds())))
		select {
		case <-time.After(delay):
		case <-ctx.Done():
		}
	}
	return result
}

func report(tasks []Task, results []Outcome, elapsed time.Duration) error {
	counts := map[string]int{}
	calls := 0
	fmt.Printf("\nQueue summary: elapsed=%s\n", elapsed.Round(100*time.Millisecond))
	for i, t := range tasks {
		r := results[i]
		if r.Status == "" {
			r.Status, r.Detail = "not_started", "队列被中断前没有派发；重新运行队列即可"
		}
		counts[r.Status]++
		calls += r.Calls
		fmt.Printf("- %-10s %-11s attempts=%d model_calls=%-2d %s\n", t.ID, r.Status, r.Attempts, r.Calls, r.Detail)
	}
	fmt.Printf("Queue totals: done=%d replayed=%d failed=%d interrupted=%d not_started=%d model_calls=%d\n",
		counts["done"], counts["replayed"], counts["failed"], counts["interrupted"], counts["not_started"], calls)
	if counts["done"]+counts["replayed"] != len(tasks) {
		return fmt.Errorf("%d个任务未完成", len(tasks)-counts["done"]-counts["replayed"])
	}
	return nil
}

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > 50 {
		return string([]rune(s)[:50]) + "…"
	}
	return s
}
