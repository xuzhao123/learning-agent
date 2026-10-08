package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"learning-agent/internal/agent"
	"learning-agent/internal/llm"
	"learning-agent/internal/protocol"
	"learning-agent/internal/queue"
	"learning-agent/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

const Dir = ".data/eval"

// Record 是一次评测的全部记录，存在 .data/eval/<ID>.json。可复现所需的信息（代码版本、题目版本、参数、模型）都在这里。
type Record struct {
	ID        string          `json:"id"`
	Kind      string          `json:"kind,omitempty"` // 空：评测集运行；attribution：对一段对话的自动归因（见 attribute.go）
	Status    string          `json:"status"`         // running、done、interrupted
	Started   time.Time       `json:"started"`
	Finished  time.Time       `json:"finished,omitzero"`
	Commit    string          `json:"commit"`
	CasesFile string          `json:"cases_file"`
	CasesHash string          `json:"cases_hash"`
	Set       string          `json:"set"`
	Only      []string        `json:"only,omitempty"`
	K         int             `json:"k"`
	Ablation  bool            `json:"ablation"`
	Args      []string        `json:"args"` // 所有试次共用的 agent 参数
	Model     string          `json:"model"`
	Judge     string          `json:"judge"`
	JudgeUse  JudgeUse        `json:"judge_usage"`
	Baseline  string          `json:"baseline,omitempty"`
	Regraded  []string        `json:"regraded,omitempty"` // 用改过的题目文件重新打过分：何时、从哪一版到哪一版
	Trials    []*Trial        `json:"trials"`
	Replays   []*Replay       `json:"replays,omitempty"`
	Notes     map[string]Note `json:"notes,omitempty"` // D17 人工开放编码：试次任务ID → 备注与归类
	// 对话归因没有题目文件：题目与评分标准由诊断现写，存在记录里。
	Inline      []Case       `json:"inline_cases,omitempty"`
	Attribution *Attribution `json:"attribution,omitempty"`
}

type JudgeUse struct {
	Calls  int `json:"calls"`
	Input  int `json:"input_tokens"`
	Output int `json:"output_tokens"`
}

// Replay 是 D17 的回放验证：保留一个失败试次的前 Rounds 轮，插入 Note，再跑几次，看结局会不会翻转。
type Replay struct {
	ID      string    `json:"id"`
	From    string    `json:"from"` // 原试次的任务ID
	Case    string    `json:"case"`
	Rounds  int       `json:"rounds"`
	Note    string    `json:"note"`
	Created time.Time `json:"created"`
	Status  string    `json:"status"`
	Trials  []*Trial  `json:"trials"`
}

// Note 是人工开放编码：先自由描述看到的第一个上游错误，再归到一个失败类别（轴心编码）。
type Note struct {
	Text    string    `json:"text"`
	Label   string    `json:"label"`
	Updated time.Time `json:"updated"`
}

// Options 是一次评测的设置。Attach 让界面接管每个试次（观测台为它建一条对话、接上代理和协议）；命令行运行时为 nil。
type Options struct {
	CasesFile string
	Set       string // dev（默认，不含留出题）、holdout、all
	Only      []string
	K         int
	Workers   int
	Ablation  bool
	Args      []string
	Judge     string
	Baseline  string
	Attach    Attach
}

// Attach 让界面接管一个试次：返回这个试次用的 ctx（挂上界面的 trace）、子进程的界面、界面里的对话ID，以及结束时的回调。
type Attach func(ctx context.Context, t *Trial, c Case) (context.Context, agent.ChildIO, string, func(ok bool))

// Runner 持有一条评测记录：运行、回放、人工备注都经它写盘，互不覆盖。
type Runner struct {
	mu     sync.Mutex
	rec    *Record
	cases  []Case
	judge  *Judge
	attach Attach
	hash   string // 当前用例文件的哈希：重新打分后写进记录
}

func path(id string) string { return filepath.Join(Dir, id+".json") }

// NewID 用 UTC 时间：可读、按时间排序，和在哪个时区运行无关。
func NewID() string { return "e" + time.Now().UTC().Format("20060102-150405") }

// Prepare 建立一次评测的记录（还没开始运行）：选题、记下代码与题目版本、检查评审模型和基线是否可用。
func Prepare(o Options) (*Runner, error) {
	if o.CasesFile == "" {
		o.CasesFile = "eval/cases.jsonl"
	}
	if o.Set == "" {
		o.Set = "dev"
	}
	if o.K < 1 || o.K > 5 || o.Workers < 1 || o.Workers > 8 {
		return nil, errors.New("k 取 1 到 5，workers 取 1 到 8")
	}
	cases, hash, err := LoadCases(o.CasesFile)
	if err != nil {
		return nil, err
	}
	var selected []Case
	for _, c := range cases {
		inSet := o.Set == "all" || (o.Set == "holdout") == c.Holdout
		if inSet && (len(o.Only) == 0 || slices.Contains(o.Only, c.ID)) {
			selected = append(selected, c)
		}
	}
	if o.Set != "dev" && o.Set != "holdout" && o.Set != "all" {
		return nil, errors.New("set 取 dev、holdout 或 all")
	}
	if len(selected) == 0 {
		return nil, errors.New("没有选中任何用例")
	}
	if o.Baseline != "" {
		if _, err := Load(o.Baseline); err != nil {
			return nil, fmt.Errorf("基线 %s：%w", o.Baseline, err)
		}
	}
	judge, err := NewJudge(o.Judge)
	if err != nil {
		return nil, err
	}
	rec := &Record{ID: NewID(), Status: "running", Started: time.Now().UTC(), Commit: commit(), CasesFile: o.CasesFile, CasesHash: hash,
		Set: o.Set, Only: o.Only, K: o.K, Ablation: o.Ablation, Args: o.Args, Judge: judge.Model(), Baseline: o.Baseline}
	for _, c := range selected {
		arms := []string{"with"}
		if o.Ablation && c.Ablation != "" {
			arms = append(arms, "without")
		}
		for _, arm := range arms {
			for n := 1; n <= o.K; n++ {
				rec.Trials = append(rec.Trials, &Trial{Case: c.ID, Arm: arm, N: n, Status: "pending",
					TaskID: fmt.Sprintf("eval-%s-%s-%s-%d", rec.ID, c.ID, map[string]string{"with": "w", "without": "wo"}[arm], n)})
			}
		}
	}
	if _, err := os.Stat(path(rec.ID)); err == nil {
		return nil, fmt.Errorf("评测 %s 已存在，请稍后再试", rec.ID)
	}
	r := &Runner{rec: rec, cases: cases, judge: judge, attach: o.Attach}
	return r, r.save()
}

// Open 读回一条已有的评测记录，用于重新打分、回放和人工备注。用例按记录里的文件重新读取（改过的打分器在这里生效）。
func Open(id string, judgeProvider string, attach Attach) (*Runner, error) {
	rec, err := Load(id)
	if err != nil {
		return nil, err
	}
	cases, hash := rec.Inline, ""
	if rec.CasesFile != "" {
		if cases, hash, err = LoadCases(rec.CasesFile); err != nil {
			return nil, err
		}
	}
	r := &Runner{rec: rec, cases: cases, attach: attach, hash: hash}
	if judgeProvider != "" {
		if r.judge, err = NewJudge(judgeProvider); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func Load(id string) (*Record, error) {
	if !agent.ValidID(id) {
		return nil, errors.New("评测ID无效")
	}
	data, err := os.ReadFile(path(id))
	if err != nil {
		return nil, err
	}
	rec := &Record{}
	return rec, json.Unmarshal(data, rec)
}

// List 按时间倒序列出全部评测记录。
func List() []*Record {
	paths, _ := filepath.Glob(filepath.Join(Dir, "*.json"))
	var list []*Record
	for _, p := range paths {
		if rec, err := Load(strings.TrimSuffix(filepath.Base(p), ".json")); err == nil {
			list = append(list, rec)
		}
	}
	slices.SortFunc(list, func(a, b *Record) int { return b.Started.Compare(a.Started) })
	return list
}

func (r *Runner) ID() string { return r.rec.ID }

func (r *Runner) Cases() []Case { return r.cases }

// Snapshot 返回记录的一份拷贝，界面轮询进度时读它。
func (r *Runner) Snapshot() *Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	data, _ := json.Marshal(r.rec)
	copy := &Record{}
	json.Unmarshal(data, copy)
	return copy
}

// 先写临时文件再改名，和检查点一样：读到的总是完整的一版。调用方持有 r.mu。
func (r *Runner) save() error {
	if r.judge != nil {
		r.judge.mu.Lock()
		r.rec.JudgeUse.Calls, r.rec.JudgeUse.Input, r.rec.JudgeUse.Output = r.judge.Calls+r.rec.JudgeUse.Calls, r.judge.Input+r.rec.JudgeUse.Input, r.judge.Output+r.rec.JudgeUse.Output
		r.judge.Calls, r.judge.Input, r.judge.Output = 0, 0, 0
		r.judge.mu.Unlock()
	}
	data, err := json.MarshalIndent(r.rec, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(Dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path(r.rec.ID)+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path(r.rec.ID)+".tmp", path(r.rec.ID))
}

func (r *Runner) find(id string) (Case, bool) {
	i := slices.IndexFunc(r.cases, func(c Case) bool { return c.ID == id })
	if i < 0 {
		return Case{}, false
	}
	return r.cases[i], true
}

// Run 执行全部待跑的试次：固定数量的 worker，每个试次一个 agent 子进程（复用队列的 RunTask：暂时性失败续跑）。
// 每个试次结束就打分、写盘；中途停止的评测保留已完成的试次。
func (r *Runner) Run(ctx context.Context, workers int) error {
	ctx, span := telemetry.Begin(ctx, "invoke_workflow eval", trace.SpanKindInternal, semconv.GenAIOperationNameInvokeWorkflow,
		attribute.String("gen_ai.workflow.name", "eval"), attribute.String("eval.id", r.rec.ID), attribute.Int("eval.trials", len(r.rec.Trials)))
	defer span.End()
	jobs := make(chan *Trial)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			for t := range jobs {
				r.execute(ctx, t, r.rec.Args)
			}
		})
	}
dispatch:
	for _, t := range r.rec.Trials {
		if t.Status != "pending" {
			continue
		}
		select {
		case jobs <- t:
		case <-ctx.Done():
			break dispatch
		}
	}
	close(jobs)
	wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rec.Status, r.rec.Finished = "done", time.Now().UTC()
	if ctx.Err() != nil {
		r.rec.Status = "interrupted"
	}
	return r.save()
}

// execute 跑一个试次：子进程的协议通知收下来还原工具过程，结局与用量读检查点，然后打分。
func (r *Runner) execute(ctx context.Context, t *Trial, common []string) {
	c, ok := r.find(t.Case)
	r.mu.Lock()
	t.Status, t.Started = "running", time.Now().UTC()
	r.save()
	r.mu.Unlock()
	if !ok {
		r.finish(t, "failed", "用例已从评测集中删除")
		return
	}
	child, done := agent.ChildIO{Out: io.Discard}, func(bool) {}
	if r.attach != nil {
		var run string
		ctx, child, run, done = r.attach(ctx, t, c)
		r.mu.Lock()
		t.Run = run
		r.mu.Unlock()
	} else if log, err := os.OpenFile(filepath.Join(Dir, r.rec.ID+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
		defer log.Close()
		child.Out = &prefixed{w: log, prefix: t.TaskID + " | "}
	}
	var capture capture
	if cp, err := agent.LoadCheckpoint(t.TaskID); err == nil {
		capture.round = cp.Step // 续跑或回放：轮次接着检查点往下数
	}
	forward := child.Notify
	child.Notify = func(m protocol.Message) {
		capture.add(t.TaskID, m)
		if forward != nil {
			forward(m)
		}
	}
	args := c.ArmArgs(t.Arm)
	if t.Arm == "replay" {
		args = nil // 回放的检查点已存好启动参数，RunChild 用 -resume 接着跑
	}
	result := queue.RunTask(ctx, queue.Task{ID: t.TaskID, Question: c.Question}, append(slices.Clone(args), common...), 2, child)
	cp, err := agent.LoadCheckpoint(t.TaskID)
	r.mu.Lock()
	t.Tools, t.Model = append(t.Tools, capture.tools...), capture.model
	if r.rec.Model == "" {
		r.rec.Model = capture.model
	}
	if err == nil {
		t.Outcome, t.Answer, t.Calls, t.Tokens, t.Traces = cp.Reason, cp.Answer, cp.Calls, cp.Input+cp.Output, cp.Traces
		if cp.Status == "running" {
			t.Outcome = "vanished"
		}
		if !cp.StartedAt.IsZero() {
			t.Started, t.Seconds = cp.StartedAt, cp.UpdatedAt.Sub(cp.StartedAt).Seconds()
		}
	}
	r.mu.Unlock()
	status := "done"
	switch result.Status {
	case "done", "replayed":
	case "interrupted":
		status = "interrupted"
	default:
		status = "failed"
	}
	done(status == "done") // 先让界面里的对话结束，再打分（评审模型要几秒）
	r.finish(t, status, result.Detail)
}

func (r *Runner) finish(t *Trial, status, detail string) {
	c, _ := r.find(t.Case)
	r.mu.Lock()
	t.Status, t.Detail = status, detail
	r.mu.Unlock()
	if status != "interrupted" {
		// 打分可能要请求评审模型，不持锁；只改这个试次自己的字段。
		snapshot := *t
		snapshot.Checks = nil
		Grade(context.Background(), c, &snapshot, r.judge)
		r.mu.Lock()
		t.Checks, t.Score, t.Pass = snapshot.Checks, snapshot.Score, snapshot.Pass
		r.mu.Unlock()
	}
	r.mu.Lock()
	r.save()
	r.mu.Unlock()
}

// Reopen 把中途停下的试次重新排队：已有检查点的从断点续跑，已完成的不重跑。
func (r *Runner) Reopen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, t := range r.rec.Trials {
		if t.Status == "interrupted" || t.Status == "running" {
			t.Status, n = "pending", n+1
		}
	}
	if n > 0 {
		r.rec.Status = "running"
		r.save()
	}
	return n
}

// Regrade 用当前的用例文件和评审模型重新打分，不重跑 agent。改了打分器、或要校准评审模型时用。
func (r *Runner) Regrade(ctx context.Context) error {
	if r.judge == nil {
		return errors.New("重新打分需要评审模型")
	}
	all := slices.Clone(r.rec.Trials)
	for _, rp := range r.rec.Replays {
		all = append(all, rp.Trials...)
	}
	var wg sync.WaitGroup
	slots := make(chan struct{}, 4)
	for _, t := range all {
		c, ok := r.find(t.Case)
		if !ok || t.Status == "pending" || t.Status == "running" || t.Status == "interrupted" {
			continue
		}
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			snapshot := *t
			snapshot.Checks = nil
			Grade(ctx, c, &snapshot, r.judge)
			r.mu.Lock()
			t.Checks, t.Score, t.Pass = snapshot.Checks, snapshot.Score, snapshot.Pass
			r.mu.Unlock()
		})
	}
	wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hash != "" && r.hash != r.rec.CasesHash {
		r.rec.Regraded = append(r.rec.Regraded, fmt.Sprintf("%s：题目 %s → %s", time.Now().UTC().Format(time.RFC3339), r.rec.CasesHash, r.hash))
		r.rec.CasesHash = r.hash
	}
	return r.save()
}

// StartReplay 登记一次回放验证：从失败试次的检查点分叉 n 份（保留前 rounds 轮、插入 note），之后由 RunReplay 执行。
func (r *Runner) StartReplay(from string, rounds int, note string, n int) (*Replay, error) {
	return r.startReplay(from, rounds, note, n, nil)
}

// snapshot 不为空时从它分叉（见 agent.ForkSnapshot），否则按轮截断检查点里的 View。
type snapshot struct {
	messages []llm.Message
	calls    int
}

func (r *Runner) startReplay(from string, rounds int, note string, n int, snap *snapshot) (*Replay, error) {
	if n < 1 || n > 5 || rounds < 0 {
		return nil, errors.New("回放次数取 1 到 5，保留轮数不能为负")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	i := slices.IndexFunc(r.rec.Trials, func(t *Trial) bool { return t.TaskID == from })
	if i < 0 {
		return nil, fmt.Errorf("评测 %s 里没有试次 %s", r.rec.ID, from)
	}
	src, err := agent.LoadCheckpoint(from)
	if err != nil {
		return nil, err
	}
	rp := &Replay{ID: fmt.Sprintf("rp%d", len(r.rec.Replays)+1), From: from, Case: r.rec.Trials[i].Case, Rounds: rounds, Note: strings.TrimSpace(note), Created: time.Now().UTC(), Status: "running"}
	for k := 1; k <= n; k++ {
		id := fmt.Sprintf("%s-%s-%d", from, rp.ID, k)
		// 保留的轮次里的工具调用也算这次回放的过程：打分器看的是整条轨迹。
		kept := slices.DeleteFunc(slices.Clone(r.rec.Trials[i].Tools), func(u ToolUse) bool { return u.Round > rounds })
		if snap != nil {
			kept = toolsFrom(snap.messages)
			_, err = agent.ForkSnapshot(src, id, snap.messages, rounds, snap.calls, rp.Note)
		} else {
			_, err = agent.ForkCheckpoint(src, id, rounds, rp.Note)
		}
		if err != nil {
			return nil, err
		}
		rp.Trials = append(rp.Trials, &Trial{Case: rp.Case, Arm: "replay", N: k, TaskID: id, Status: "pending", Tools: kept})
	}
	r.rec.Replays = append(r.rec.Replays, rp)
	return rp, r.save()
}

// RunReplay 依次跑完一次回放的各份分叉（每份都从分叉点接着跑，所以不需要题目参数）。
func (r *Runner) RunReplay(ctx context.Context, rp *Replay) error {
	var wg sync.WaitGroup
	for _, t := range rp.Trials {
		wg.Go(func() { r.execute(ctx, t, nil) })
	}
	wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	rp.Status = "done"
	if ctx.Err() != nil {
		rp.Status = "interrupted"
	}
	return r.save()
}

// Annotate 保存人工开放编码。文字为空表示删除这条备注。
func (r *Runner) Annotate(taskID, text, label string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.ContainsFunc(r.rec.Trials, func(t *Trial) bool { return t.TaskID == taskID }) {
		return fmt.Errorf("没有试次 %s", taskID)
	}
	if r.rec.Notes == nil {
		r.rec.Notes = map[string]Note{}
	}
	text, label = strings.TrimSpace(text), strings.TrimSpace(label)
	if text == "" && label == "" {
		delete(r.rec.Notes, taskID)
	} else {
		r.rec.Notes[taskID] = Note{Text: text, Label: label, Updated: time.Now().UTC()}
	}
	return r.save()
}

// capture 从子进程的协议通知里还原工具过程。只看本任务的 Item（子 agent 的通知包在 subagent/message 里，不算父任务的调用）。
type capture struct {
	mu    sync.Mutex
	round int
	tools []ToolUse
	model string
}

func (c *capture) add(task string, m protocol.Message) {
	var p struct {
		TurnID string `json:"turn_id"`
		Model  string `json:"model"`
		Item   struct {
			Type string `json:"type"`
			Data struct {
				Purpose     string `json:"purpose"`
				Tool        string `json:"tool"`
				Arguments   string `json:"arguments"`
				Observation *struct {
					Status   string          `json:"status"`
					Result   json.RawMessage `json:"result"`
					Error    string          `json:"error"`
					Attempts int             `json:"attempts"`
				} `json:"observation"`
			} `json:"data"`
		} `json:"item"`
	}
	if json.Unmarshal(m.Params, &p) != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case m.Method == "turn/started" && p.TurnID == task:
		c.model = p.Model
	case m.Method == "item/completed" && p.TurnID == task && p.Item.Type == "modelCall" && p.Item.Data.Purpose == "main":
		c.round++
	case m.Method == "item/completed" && p.TurnID == task && p.Item.Type == "toolCall" && p.Item.Data.Observation != nil:
		o := p.Item.Data.Observation
		result := string(o.Result)
		if o.Status != "ok" {
			result = o.Error
		}
		c.tools = append(c.tools, ToolUse{Round: c.round, Name: p.Item.Data.Tool, Args: p.Item.Data.Arguments, Status: o.Status, Attempts: o.Attempts, Result: clip(result, 2000)})
	}
}

type prefixed struct {
	w      io.Writer
	prefix string
	mu     sync.Mutex
}

func (p *prefixed) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, line := range strings.SplitAfter(string(b), "\n") {
		if line != "" {
			io.WriteString(p.w, p.prefix+line)
		}
	}
	return len(b), nil
}

// 代码版本：commit 加上工作区是否有未提交的改动。
func commit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	id := strings.TrimSpace(string(out))
	if status, _ := exec.Command("git", "status", "--porcelain", "--untracked-files=no").Output(); len(strings.TrimSpace(string(status))) > 0 {
		id += "-dirty"
	}
	return id
}
