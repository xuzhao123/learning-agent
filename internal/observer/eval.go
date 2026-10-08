package observer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"learning-agent/internal/agent"
	"learning-agent/internal/eval"
	"learning-agent/internal/llm"
	"learning-agent/internal/telemetry"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// D16/D17：观测台是评测的界面。评测逻辑（选题、执行、打分、回放）在 internal/eval，与 go run . eval 共用同一份记录；
// 观测台给每个试次建一条对话：模型请求经本机代理、协议通知存成事件，所以每个试次都能像普通对话一样看轨迹、调用链。
type evalJob struct {
	runner *eval.Runner
	busy   string // 正在做的事：run、replay、regrade；空表示空闲
	cancel context.CancelFunc
}

type evals struct {
	mu   sync.Mutex
	jobs map[string]*evalJob
}

// job 取一条评测记录。空闲的记录每次从磁盘读回：命令行可能刚改过它（重新打分、备注），内存里的旧副本不能再写回去。
func (s *server) job(id string) (*evalJob, error) {
	s.evals.mu.Lock()
	defer s.evals.mu.Unlock()
	if j := s.evals.jobs[id]; j != nil && j.busy != "" {
		return j, nil
	}
	runner, err := eval.Open(id, "deepseek", s.evalAttach(id))
	if err != nil {
		// 没有评审模型的密钥时仍能查看和备注，只是不能回放、重新打分。
		if runner, err = eval.Open(id, "", s.evalAttach(id)); err != nil {
			return nil, err
		}
	}
	j := &evalJob{runner: runner}
	s.evals.jobs[id] = j
	return j, nil
}

// start 在后台做一件事；同一条评测同时只做一件，避免运行、回放和重新打分交错写记录。
func (s *server) start(j *evalJob, what string, work func(ctx context.Context)) error {
	s.evals.mu.Lock()
	if j.busy != "" {
		s.evals.mu.Unlock()
		return errors.New("这条评测正在" + map[string]string{"run": "运行", "replay": "回放", "regrade": "重新打分", "attribute": "归因"}[j.busy] + "，请稍后")
	}
	ctx, cancel := context.WithCancel(context.Background())
	j.busy, j.cancel = what, cancel
	s.evals.mu.Unlock()
	go func() {
		work(ctx)
		s.evals.mu.Lock()
		j.busy, j.cancel = "", nil
		s.evals.mu.Unlock()
		cancel()
	}()
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func (s *server) listEvals(w http.ResponseWriter, _ *http.Request) {
	list := []map[string]any{}
	for _, rec := range eval.List() {
		s.evals.mu.Lock()
		j := s.evals.jobs[rec.ID]
		busy := ""
		if j != nil {
			rec, busy = j.runner.Snapshot(), j.busy
		}
		s.evals.mu.Unlock()
		sum := eval.Summarize(rec, nil, nil)
		item := map[string]any{"id": rec.ID, "kind": rec.Kind, "status": rec.Status, "busy": busy, "started": rec.Started, "set": rec.Set, "k": rec.K,
			"commit": rec.Commit, "model": rec.Model, "baseline": rec.Baseline, "main": sum.Main, "progress": sum.Progress}
		if a := rec.Attribution; a != nil {
			item["run"], item["stage"], item["label"], item["result"] = a.Run, a.Stage, a.Label, a.Result
		}
		list = append(list, item)
	}
	writeJSON(w, list)
}

// 评测集概览：开始评测前看要跑哪些题、各多少次。
func (s *server) evalCases(w http.ResponseWriter, _ *http.Request) {
	cases, hash, err := eval.LoadCases("eval/cases.jsonl")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"file": "eval/cases.jsonl", "hash": hash, "cases": cases})
}

func (s *server) startEval(w http.ResponseWriter, req *http.Request) {
	var input struct {
		Set      string   `json:"set"`
		K        int      `json:"k"`
		Ablation bool     `json:"ablation"`
		Baseline string   `json:"baseline"`
		Cases    []string `json:"cases"`
	}
	if json.NewDecoder(req.Body).Decode(&input) != nil {
		http.Error(w, "请求必须是JSON", http.StatusBadRequest)
		return
	}
	var j *evalJob
	o := eval.Options{Set: input.Set, K: input.K, Workers: 3, Ablation: input.Ablation, Baseline: input.Baseline, Only: input.Cases, Judge: "deepseek"}
	// Attach 需要评测ID才能给对话打标记，而ID在 Prepare 里生成：先用一个间接引用，Prepare 之后再填上。
	var id string
	o.Attach = func(ctx context.Context, t *eval.Trial, c eval.Case) (context.Context, agent.ChildIO, string, func(bool)) {
		return s.evalAttach(id)(ctx, t, c)
	}
	runner, err := eval.Prepare(o)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id = runner.ID()
	j = &evalJob{runner: runner}
	s.evals.mu.Lock()
	s.evals.jobs[id] = j
	s.evals.mu.Unlock()
	s.start(j, "run", func(ctx context.Context) { runner.Run(ctx, o.Workers) })
	writeJSON(w, map[string]string{"id": id})
}

func (s *server) getEval(w http.ResponseWriter, req *http.Request) {
	j, err := s.job(req.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	rec := j.runner.Snapshot()
	var baseline *eval.Record
	if rec.Baseline != "" {
		baseline, _ = eval.Load(rec.Baseline)
	}
	s.evals.mu.Lock()
	busy := j.busy
	s.evals.mu.Unlock()
	cases := j.runner.Cases()
	info := map[string]any{}
	for _, c := range cases {
		graders := []map[string]any{}
		for _, g := range c.Graders {
			graders = append(graders, map[string]any{"name": g.Name(), "process": g.Process(), "with_only": g.WithOnly})
		}
		info[c.ID] = map[string]any{"question": c.Question, "reference": c.Reference, "group": c.Group, "suite": c.Suite, "holdout": c.Holdout, "args": c.Args, "ablation": c.Ablation, "graders": graders}
	}
	writeJSON(w, map[string]any{"record": rec, "busy": busy, "summary": eval.Summarize(rec, cases, baseline), "cards": eval.Cards(rec, cases), "cases": info})
}

func (s *server) evalAction(w http.ResponseWriter, req *http.Request) {
	j, err := s.job(req.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	var input struct {
		Trial  string `json:"trial"`
		Rounds int    `json:"rounds"`
		Note   string `json:"note"`
		N      int    `json:"n"`
		Text   string `json:"text"`
		Label  string `json:"label"`
	}
	if req.ContentLength > 0 && json.NewDecoder(req.Body).Decode(&input) != nil {
		http.Error(w, "请求必须是JSON", http.StatusBadRequest)
		return
	}
	r := j.runner
	switch req.PathValue("action") {
	case "stop":
		s.evals.mu.Lock()
		if j.cancel != nil {
			j.cancel() // 在跑的试次收到 SIGINT，写好检查点后退出；之后可以“继续”从断点续跑
		}
		s.evals.mu.Unlock()
	case "resume":
		err = s.start(j, "run", func(ctx context.Context) {
			if r.Reopen() > 0 {
				r.Run(ctx, 3)
			}
		})
	case "regrade":
		err = s.start(j, "regrade", func(ctx context.Context) { r.Regrade(ctx) })
	case "replay":
		var rp *eval.Replay
		if rp, err = r.StartReplay(input.Trial, input.Rounds, input.Note, max(1, input.N)); err == nil {
			err = s.start(j, "replay", func(ctx context.Context) { r.RunReplay(ctx, rp) })
		}
	case "note":
		err = r.Annotate(input.Trial, input.Text, input.Label)
	default:
		http.NotFound(w, req)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// evalAttach 为一个试次建一条对话：和普通对话一样经本机代理请求模型、记录协议通知，只是由评测发起，
// 列表里不显示，从评测页面打开。每个试次一个 trace（根 span interaction），调用链照常可看。
func (s *server) evalAttach(evalID string) eval.Attach {
	return func(ctx context.Context, t *eval.Trial, c eval.Case) (context.Context, agent.ChildIO, string, func(bool)) {
		id := uuid.NewString()
		file, err := os.OpenFile(filepath.Join(s.runsDir, id+".jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return ctx, agent.ChildIO{Out: discard{}}, "", func(bool) {}
		}
		r := &run{ID: id, Query: c.Question, Eval: evalID, changed: make(chan struct{}), file: file}
		s.mu.Lock()
		s.runs = append(s.runs, r)
		s.mu.Unlock()
		_, span := r.beginTurn(c.Question)
		span.SetAttributes(attribute.String("eval.id", evalID), attribute.String("eval.case", t.Case), attribute.String("eval.trial", t.TaskID))
		label := evalID + " · " + t.Case + " #" + strconv.Itoa(t.N)
		if t.Arm != "with" {
			label += " · " + map[string]string{"without": "对照组（去掉 " + c.Ablation + "）", "replay": "回放"}[t.Arm]
		}
		provider := ""
		if i := slices.Index(c.Args, "-provider"); i >= 0 && i+1 < len(c.Args) {
			provider = c.Args[i+1]
		}
		args := c.ArmArgs(t.Arm)
		r.add(event{Kind: "start", Text: c.Question, Eval: label, Subagents: slices.Contains(args, "-subagents"), Browser: slices.Contains(args, "-browser"),
			Bash: slices.Contains(args, "-bash"), Provider: provider, Embedding: "ark", Trace: span.SpanContext().TraceID().String(), Protocol: true})
		out := &lines{r: r}
		child := agent.ChildIO{Out: out, Notify: r.handle, Env: []string{"LLM_API_URL=http://" + s.addr + "/llm/" + id, "AGENT_CONVERSATION_ID=" + id}}
		done := func(ok bool) {
			out.flush()
			text := "exit 0"
			if !ok {
				text = "exit status 1"
			}
			span.SetAttributes(attribute.String("process.exit", text))
			var err error
			if !ok {
				err = errors.New(text)
			}
			telemetry.End(span, err, "agent_exit")
			r.add(event{Kind: "exit", Text: text})
		}
		return trace.ContextWithSpan(ctx, span), child, id, done
	}
}

// lines 把子进程的 stderr 按行存成 stdout 事件，和普通对话的“终端输出”一样。管道按块到达，攒够一行再记。
type lines struct {
	r   *run
	mu  sync.Mutex
	buf []byte
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := slices.Index(l.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		l.r.add(event{Kind: "stdout", Text: string(l.buf[:i])})
		l.buf = l.buf[i+1:]
	}
}

func (l *lines) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) > 0 {
		l.r.add(event{Kind: "stdout", Text: string(l.buf)})
		l.buf = nil
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// attributeRun 对一段对话的最后一轮自动归因（见 eval.Attribute）：诊断、评分、回放验证都在后台进行，
// 返回归因记录的ID，页面打开它的详情即可看到进度。回放的每次尝试同样是一条可打开的对话。
func (s *server) attributeRun(w http.ResponseWriter, req *http.Request) {
	r := s.find(req.PathValue("id"))
	if r == nil {
		http.Error(w, "找不到这条对话", http.StatusNotFound)
		return
	}
	r.mu.Lock()
	done, task := r.Done, r.lastCheckpoint()
	source := r.trajectory(task)
	r.mu.Unlock()
	if !done || task == "" {
		http.Error(w, "对话还在运行，或者没有检查点（Day 9 之前的存档、实验）", http.StatusConflict)
		return
	}
	var id string
	attach := func(ctx context.Context, t *eval.Trial, c eval.Case) (context.Context, agent.ChildIO, string, func(bool)) {
		return s.evalAttach(id)(ctx, t, c)
	}
	runner, err := eval.NewAttribution(r.ID, task, source, "deepseek", attach)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	id = runner.ID()
	j := &evalJob{runner: runner}
	s.evals.mu.Lock()
	s.evals.jobs[id] = j
	s.evals.mu.Unlock()
	s.start(j, "attribute", runner.Attribute)
	writeJSON(w, map[string]string{"id": id})
}

// trajectory 从观测台记下的事件重建这一轮的完整过程，交给诊断：检查点里只有压缩后的 View，
// 早先的决定只剩一段摘要，诊断就会把下游的错误当成第一个错误。这里用代理记录的每一次主任务响应
// （模型的推理、回复与工具调用）和协议里的工具结果，按实际请求顺序编号；压缩只记一行，不算一次决定。
// 调用方持有 r.mu。
func (r *run) trajectory(task string) eval.Source {
	from := 0
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i].Kind == "start" || r.events[i].Kind == "continue" {
			from = i
			break
		}
	}
	type message struct {
		Role             string `json:"role"`
		Content          string `json:"content"`
		ReasoningContent string `json:"reasoning_content"`
		ToolCalls        []struct {
			ID       string                           `json:"id"`
			Function struct{ Name, Arguments string } `json:"function"`
		} `json:"tool_calls"`
	}
	src := eval.Source{Snapshots: map[int][]llm.Message{}, CallsBefore: map[int]int{}}
	results := map[string]string{} // 调用ID → 工具结果
	var snapshot []llm.Message     // 最近一次主任务请求的完整输入：下一次主任务响应就是对它做的决定
	requests := 0                  // 本进程（不含子 agent）已经发出的模型请求，含压缩与记忆辅助调用
	for _, e := range r.events[from:] {
		if e.Kind != "notify" || e.Method != "item/completed" {
			continue
		}
		var p struct {
			TurnID string `json:"turn_id"`
			Item   struct {
				ID   string `json:"id"`
				Type string `json:"type"`
				Data struct {
					Observation json.RawMessage `json:"observation"`
				} `json:"data"`
			} `json:"item"`
		}
		if json.Unmarshal(e.Body, &p) == nil && p.TurnID == task && p.Item.Type == "toolCall" {
			results[p.Item.ID] = string(p.Item.Data.Observation)
		}
	}
	var b strings.Builder
	header := false
	for _, e := range r.events[from:] {
		main := !e.Sub && (e.Purpose == "" || e.Purpose == "main")
		if e.Kind == "request" && !e.Sub {
			requests++
			var body struct {
				Messages []llm.Message `json:"messages"`
			}
			if main && json.Unmarshal(e.Body, &body) == nil {
				snapshot = body.Messages
			}
		}
		switch {
		case e.Kind == "request" && main && !header:
			// 第一次主任务请求里有系统提示、用户问题和可用工具。
			var body struct {
				Messages []message `json:"messages"`
				Tools    []struct {
					Function struct{ Name, Description string } `json:"function"`
				} `json:"tools"`
			}
			if json.Unmarshal(e.Body, &body) != nil {
				continue
			}
			header = true
			for _, t := range body.Tools {
				src.Tools += "- " + t.Function.Name + "：" + t.Function.Description + "\n"
			}
			if src.Tools == "" {
				src.Tools = "（没有提供任何工具）\n"
			}
			for _, m := range body.Messages {
				fmt.Fprintf(&b, "\n[%s] %s\n", map[string]string{"system": "系统提示", "user": "用户", "assistant": "之前的回复", "tool": "之前的工具结果"}[m.Role], eval.Clip(m.Content, 800))
			}
		case e.Kind == "response" && !e.Sub && e.Purpose == "compact":
			b.WriteString("\n（这里发生了一次上下文压缩：之前的内容被换成摘要，这次请求不算模型决定）\n")
		case e.Kind == "response" && main:
			var body struct {
				Choices []struct {
					Message message `json:"message"`
				} `json:"choices"`
			}
			if e.Status != 200 || json.Unmarshal(e.Body, &body) != nil || len(body.Choices) == 0 {
				fmt.Fprintf(&b, "\n（一次模型请求失败：HTTP %d %s）\n", e.Status, eval.Clip(e.Text, 200))
				continue
			}
			m := body.Choices[0].Message
			src.Decisions++
			src.Snapshots[src.Decisions], src.CallsBefore[src.Decisions] = snapshot, requests-1
			fmt.Fprintf(&b, "\n第 %d 次模型决定：", src.Decisions)
			if m.ReasoningContent != "" {
				fmt.Fprintf(&b, "\n  推理：%s", eval.Clip(m.ReasoningContent, 600))
			}
			if m.Content != "" {
				fmt.Fprintf(&b, "\n  回复：%s", eval.Clip(m.Content, 1200))
			}
			for _, call := range m.ToolCalls {
				result, ok := results[call.ID]
				if !ok {
					result = "（没有执行：这批调用被拦下或进程中途退出）"
				}
				fmt.Fprintf(&b, "\n  调用 %s(%s)\n    结果：%s", call.Function.Name, eval.Clip(call.Function.Arguments, 300), eval.Clip(result, 500))
			}
			b.WriteString("\n")
		}
	}
	src.Trajectory = b.String()
	return src
}
