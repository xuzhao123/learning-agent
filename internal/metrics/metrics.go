package metrics

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"learning-agent/internal/agent"
	"learning-agent/internal/telemetry"
)

// D15：5 个核心指标。结局、耗时、成本读检查点（kill -9 也不会丢）；工具指标读 trace，崩溃时可能少记（见 Q19）。
//
//	1 任务完成率        完成的根任务 / 根任务总数（不含运行中的任务）；完成 ≠ 答对，答对率要 D16 的评测集打分
//	2 异常结局与可恢复  非正常结局按原因分布；进程消失（running 且锁已释放）单列；其中能续跑的比例
//	3 端到端延迟        已结束任务的 P50 / P95（开始到最后一次写检查点，含中断的那段时间）
//	4 成本              每个任务的 token 与模型请求；单次完成成本 = 全部任务的用量 / 完成数（失败的花费也算进去）
//	5 工具可靠性        每个工具的调用数、失败率、结果未知率、重试次数、P95 耗时
type Report struct {
	Window     string `json:"window"`
	Tasks      int    `json:"tasks"`    // 根任务（不含子 agent），不含运行中的
	Running    int    `json:"running"`  // 正在运行，不计入
	Subtasks   int    `json:"subtasks"` // 子 agent 任务，单独统计完成率
	Completion struct {
		Done        int     `json:"done"`
		Rate        float64 `json:"rate"`
		SubDone     int     `json:"sub_done"`
		SubRate     float64 `json:"sub_rate"`
		Unconfirmed string  `json:"note"`
	} `json:"completion"`
	Reliability struct {
		Outcomes    map[string]int `json:"outcomes"` // 结局 → 根任务数
		Abnormal    float64        `json:"abnormal_rate"`
		Vanished    int            `json:"vanished"`  // 进程消失：检查点停在 running，锁已释放
		Resumable   int            `json:"resumable"` // 非正常结局里还能续跑的
		ResumeShare float64        `json:"resumable_share"`
	} `json:"reliability"`
	Latency struct {
		Samples int     `json:"samples"`
		P50     float64 `json:"p50_seconds"`
		P95     float64 `json:"p95_seconds"`
		Missing int     `json:"missing"` // D15 之前的检查点没有开始时间
	} `json:"latency"`
	Cost struct {
		Input         int     `json:"input_tokens"`
		Output        int     `json:"output_tokens"`
		Cached        int     `json:"cached_tokens"`
		Calls         int     `json:"model_calls"`
		PerTaskTokens float64 `json:"per_task_tokens"`
		PerDoneTokens float64 `json:"per_done_tokens"`
		PerTaskCalls  float64 `json:"per_task_calls"`
		CacheHit      float64 `json:"cache_hit_rate"`
		Missing       int     `json:"missing"` // D15 之前的检查点没有记用量
	} `json:"cost"`
	Tools []Tool `json:"tools"`
	Spans struct {
		Traces  int `json:"traces"`
		Missing int `json:"tasks_without_trace"`
	} `json:"spans"`
}

type Tool struct {
	Name      string  `json:"name"`
	Calls     int     `json:"calls"`
	Error     int     `json:"error"`
	Unknown   int     `json:"unknown"`
	NotRun    int     `json:"not_run"`
	Retries   int     `json:"retries"`
	P95       float64 `json:"p95_ms"`
	durations []float64
}

func Run(args []string) error {
	flags := flag.NewFlagSet("metrics", flag.ContinueOnError)
	since := flags.Duration("since", 7*24*time.Hour, "统计最近这段时间内结束（最后写检查点）的任务")
	asJSON := flags.Bool("json", false, "输出 JSON")
	prefix := flags.String("prefix", "", "只统计任务ID以它开头的任务，如 D16 一次评测的 eval-<评测ID>-")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *since <= 0 {
		return errors.New("用法：go run . metrics [-since 168h] [-prefix ID前缀] [-json]")
	}
	r, err := Compute(*since, *prefix)
	if err != nil {
		return err
	}
	if *asJSON {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		return encoder.Encode(r)
	}
	print(r)
	return nil
}

func Compute(since time.Duration, prefix string) (*Report, error) {
	if prefix != "" && !agent.ValidID(prefix) {
		return nil, errors.New("prefix 只能包含字母、数字和 ._-")
	}
	paths, err := filepath.Glob(filepath.Join(".data", "checkpoints", prefix+"*.json"))
	if err != nil {
		return nil, err
	}
	r := &Report{Window: since.String()}
	r.Reliability.Outcomes = map[string]int{}
	r.Completion.Unconfirmed = "完成表示模型给出了最终回答，不代表答对；答对率需要 D16 的评测集与打分"
	cutoff := time.Now().Add(-since)
	var latencies []float64
	traces := map[string]bool{}
	subs, subDone := 0, 0
	measured, measuredDone := 0, 0 // 有用量记录的根任务：成本的平均值只用它们做分母，旧检查点不摊薄结果
	for _, path := range paths {
		cp, err := agent.LoadCheckpoint(strings.TrimSuffix(filepath.Base(path), ".json"))
		if err != nil || cp.UpdatedAt.Before(cutoff) {
			continue
		}
		outcome := cp.Reason
		if cp.Status == "running" {
			// 还在运行的不计入；锁已释放说明进程已经不在了（kill -9、断电），这是 trace 里看不到的结局。
			unlock, err := agent.LockCheckpoint(cp.ID)
			if err != nil {
				r.Running++
				continue
			}
			unlock()
			outcome = "vanished"
		}
		for _, t := range cp.Traces {
			traces[t] = true
		}
		if strings.Contains(cp.ID, "-sub-") {
			subs++
			if cp.Status == "done" {
				subDone++
			}
			continue
		}
		r.Tasks++
		r.Reliability.Outcomes[outcome]++
		switch {
		case cp.Status == "done":
			r.Completion.Done++
		case outcome == "vanished":
			r.Reliability.Vanished++
			r.Reliability.Resumable++
		case agent.Resumable(cp):
			r.Reliability.Resumable++
		}
		if cp.StartedAt.IsZero() {
			r.Latency.Missing++
		} else if outcome != "vanished" {
			latencies = append(latencies, cp.UpdatedAt.Sub(cp.StartedAt).Seconds())
		}
		if cp.StartedAt.IsZero() && cp.Input == 0 {
			r.Cost.Missing++
		} else {
			measured++
			if cp.Status == "done" {
				measuredDone++
			}
		}
		r.Cost.Input += cp.Input
		r.Cost.Output += cp.Output
		r.Cost.Cached += cp.Cached
		r.Cost.Calls += cp.Calls
		if len(cp.Traces) == 0 {
			r.Spans.Missing++
		}
	}
	r.Subtasks, r.Completion.SubDone = subs, subDone
	r.Completion.Rate = ratio(r.Completion.Done, r.Tasks)
	r.Completion.SubRate = ratio(subDone, subs)
	abnormal := r.Tasks - r.Completion.Done
	r.Reliability.Abnormal = ratio(abnormal, r.Tasks)
	r.Reliability.ResumeShare = ratio(r.Reliability.Resumable, abnormal)
	slices.Sort(latencies)
	r.Latency.Samples, r.Latency.P50, r.Latency.P95 = len(latencies), math.Round(rank(latencies, 0.5)*10)/10, math.Round(rank(latencies, 0.95)*10)/10
	tokens := r.Cost.Input + r.Cost.Output
	r.Cost.PerTaskTokens = ratioF(tokens, measured)
	r.Cost.PerDoneTokens = ratioF(tokens, measuredDone)
	r.Cost.PerTaskCalls = ratioF(r.Cost.Calls, r.Tasks)
	r.Cost.CacheHit = ratio(r.Cost.Cached, r.Cost.Input)
	r.Spans.Traces = len(traces)
	r.Tools = tools(traces)
	return r, nil
}

// 工具指标从这些任务的 trace 里读 execute_tool span：Observation 的四种结局记在 agent.tool.status，重试是 span 事件。
func tools(traces map[string]bool) []Tool {
	byName := map[string]*Tool{}
	for id := range traces {
		data, err := os.ReadFile(telemetry.Path(id))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			var span telemetry.Record
			if json.Unmarshal([]byte(line), &span) != nil || span.Attributes["gen_ai.operation.name"] != "execute_tool" {
				continue
			}
			name, _ := span.Attributes["gen_ai.tool.name"].(string)
			t := byName[name]
			if t == nil {
				t = &Tool{Name: name}
				byName[name] = t
			}
			t.Calls++
			switch span.Attributes["agent.tool.status"] {
			case "error":
				t.Error++
			case "unknown":
				t.Unknown++
			case "not_run":
				t.NotRun++
			}
			for _, e := range span.Events {
				if e.Name == "retry" {
					t.Retries++
				}
			}
			t.durations = append(t.durations, float64(span.End.Sub(span.Start).Microseconds())/1000)
		}
	}
	list := []Tool{}
	for _, t := range byName {
		slices.Sort(t.durations)
		t.P95 = rank(t.durations, 0.95)
		list = append(list, *t)
	}
	slices.SortFunc(list, func(a, b Tool) int { return b.Calls - a.Calls })
	return list
}

// 最近秩法：第 ceil(p×n) 个值。样本少时 P95 就是最大值附近，读数要结合样本数。
func rank(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[max(0, int(math.Ceil(p*float64(len(sorted))))-1)]
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return math.Round(float64(a)/float64(b)*1000) / 1000
}

func ratioF(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return math.Round(float64(a)/float64(b)*10) / 10
}

func print(r *Report) {
	pct := func(v float64) string { return fmt.Sprintf("%.1f%%", v*100) }
	fmt.Printf("D15 核心指标（最近 %s 结束的任务；根任务 %d 个，运行中 %d 个不计入，子 agent 任务 %d 个）\n\n", r.Window, r.Tasks, r.Running, r.Subtasks)
	fmt.Printf("1 任务完成率   %s（%d/%d）；子 agent %s（%d/%d）\n  注：%s\n", pct(r.Completion.Rate), r.Completion.Done, r.Tasks, pct(r.Completion.SubRate), r.Completion.SubDone, r.Subtasks, r.Completion.Unconfirmed)
	outcomes := []string{}
	for k, v := range r.Reliability.Outcomes {
		outcomes = append(outcomes, fmt.Sprintf("%s=%d", k, v))
	}
	slices.Sort(outcomes)
	fmt.Printf("2 异常结局     %s；进程消失 %d 个；异常里能续跑的占 %s\n  结局分布：%s\n", pct(r.Reliability.Abnormal), r.Reliability.Vanished, pct(r.Reliability.ResumeShare), strings.Join(outcomes, " "))
	fmt.Printf("3 端到端延迟   P50 %.1fs  P95 %.1fs（样本 %d；%d 个旧检查点没有开始时间）\n", r.Latency.P50, r.Latency.P95, r.Latency.Samples, r.Latency.Missing)
	fmt.Printf("4 成本         每任务 %.0f tokens、%.1f 次模型请求；每完成一个任务 %.0f tokens；缓存命中 %s（%d 个旧检查点没有用量）\n", r.Cost.PerTaskTokens, r.Cost.PerTaskCalls, r.Cost.PerDoneTokens, pct(r.Cost.CacheHit), r.Cost.Missing)
	fmt.Printf("5 工具可靠性   来自 %d 个 trace（%d 个任务没有 trace；崩溃时正在执行的工具不会出现在这里）\n", r.Spans.Traces, r.Spans.Missing)
	fmt.Printf("  %-24s %6s %8s %8s %8s %6s %10s\n", "工具", "调用", "失败", "结果未知", "未执行", "重试", "P95")
	for _, t := range r.Tools {
		fmt.Printf("  %-24s %6d %8s %8s %8d %6d %9.0fms\n", t.Name, t.Calls, pct(ratio(t.Error, t.Calls)), pct(ratio(t.Unknown, t.Calls)), t.NotRun, t.Retries, t.P95)
	}
}
