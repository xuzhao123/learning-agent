package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"learning-agent/internal/agent"
	"learning-agent/internal/llm"
)

// D17 扩展：对观测台里的一段对话自动归因，不需要人先写评分标准。
// 做法参照 REFLECT：模型先诊断（判断成败、写出评分标准、指出决定性的一步和原因、给一条纠正），
// 再用回放验证这条诊断——从那一步之前分叉，插入纠正跑 3 次，同时插入“继续。”作安慰剂对照跑 3 次。
// 纠正能让结局翻转、安慰剂不能，诊断才算“已证实”。自动定位出错步骤本身不可靠（Who&When 约 14%），
// 所以结论标明“模型初判”，证据是回放结果；人可以在卡片上改写开放编码。
type Attribution struct {
	Run    string `json:"run"`     // 观测台对话ID
	TaskID string `json:"task_id"` // 被归因的那一轮的检查点
	Tools  string `json:"tools"`   // 当时可用的工具：名字与描述
	// 诊断读到的轨迹。界面给出完整轨迹（观测台从代理与协议事件重建，压缩前的决定也在）；
	// 没有时退回检查点里的 View，压缩过的部分只剩摘要。
	Trajectory  string `json:"trajectory"`
	Full        bool   `json:"full_trajectory"`
	Stage       string `json:"stage"` // diagnose → grade → replay → done；failed
	Error       string `json:"error,omitempty"`
	Verdict     string `json:"verdict"` // 诊断：failure、success、unclear
	Expected    string `json:"expected"`
	Rubric      string `json:"rubric"`
	Round       int    `json:"round"` // 决定性错误是第几次模型决定
	What        string `json:"what"`
	Why         string `json:"why"`
	Label       string `json:"label"`
	Fix         string `json:"fix"`  // 建议落到哪里修：工具描述、系统规则、执行器……
	Note        string `json:"note"` // 回放时插入的纠正
	Blocked     string `json:"blocked,omitempty"`
	Result      string `json:"result"` // 结论：已证实 / 未证实 / 只做了诊断……
	decisions   int
	snapshots   map[int][]llm.Message
	callsBefore map[int]int
}

// NewAttribution 为一段对话建一条归因记录。被归因的是这段对话的最后一轮（它的检查点）。
// Source 是界面提供给诊断的材料：当时可用的工具，以及按实际请求编号的完整轨迹（Decisions 是模型决定的次数）。
// Snapshots[k] 是第 k 次决定当时的完整输入（代理记录的请求 messages），CallsBefore[k] 是此前已用掉的模型请求：
// 回放从这份快照分叉，压缩过、续聊的一轮也能验证。
type Source struct {
	Tools, Trajectory string
	Decisions         int
	Snapshots         map[int][]llm.Message
	CallsBefore       map[int]int
}

func NewAttribution(run, taskID string, src Source, judgeProvider string, attach Attach) (*Runner, error) {
	cp, err := agent.LoadCheckpoint(taskID)
	if err != nil {
		return nil, fmt.Errorf("读不到这一轮的检查点：%w", err)
	}
	if cp.Status == "running" {
		return nil, errors.New("这一轮还在运行，结束后再归因")
	}
	judge, err := NewJudge(judgeProvider)
	if err != nil {
		return nil, err
	}
	rec := &Record{ID: "a" + time.Now().UTC().Format("20060102-150405"), Kind: "attribution", Status: "running", Started: time.Now().UTC(),
		Commit: commit(), K: 1, Judge: judge.Model(), Attribution: &Attribution{Run: run, TaskID: taskID, Tools: src.Tools, Stage: "diagnose"}}
	if src.Decisions > 0 {
		head := fmt.Sprintf("用户问题：%s\n结局：%s（%s）\n", cp.Question, cp.Status, cp.Reason)
		if cp.Status != "done" {
			head += "运行停止：" + clip(cp.Error, 600) + "\n"
		}
		rec.Attribution.Trajectory, rec.Attribution.Full, rec.Attribution.decisions = head+src.Trajectory, true, src.Decisions
		rec.Attribution.snapshots, rec.Attribution.callsBefore = src.Snapshots, src.CallsBefore
	}
	r := &Runner{rec: rec, judge: judge, attach: attach}
	return r, r.save()
}

// Attribute 依次做诊断、给原对话打分、回放验证，每一步都写盘，页面轮询即可看到进度。
func (r *Runner) Attribute(ctx context.Context) {
	a := r.rec.Attribution
	fail := func(err error) {
		r.mu.Lock()
		a.Stage, a.Error, r.rec.Status, r.rec.Finished = "failed", err.Error(), "done", time.Now().UTC()
		r.save()
		r.mu.Unlock()
	}
	cp, err := agent.LoadCheckpoint(a.TaskID)
	if err != nil {
		fail(err)
		return
	}
	trajectory, decisions := a.Trajectory, a.decisions
	if !a.Full {
		trajectory, decisions = describe(cp)
		a.Trajectory = trajectory
	}
	if a.Tools != "" {
		trajectory = "agent 可用的工具：\n" + a.Tools + "\n\n" + trajectory
	}
	d, err := r.diagnose(ctx, cp, trajectory)
	if err != nil {
		fail(err)
		return
	}
	// 题目由诊断现写：参考答案与评分标准都来自审阅模型，原对话与回放用同一份标准评判。
	c := Case{ID: "conversation", Suite: "capability", Group: "对话归因", Question: cp.Question, Args: cp.Args, Reference: d.Expected,
		Graders: []Grader{{Type: "judge", Rubric: d.Rubric}}}
	t := &Trial{Case: c.ID, Arm: "with", N: 1, TaskID: cp.ID, Run: a.Run, Status: "done", Outcome: cp.Reason, Answer: cp.Answer, Tools: toolsFrom(cp.Messages),
		Started: cp.StartedAt, Seconds: cp.UpdatedAt.Sub(cp.StartedAt).Seconds(), Calls: cp.Calls, Tokens: cp.Input + cp.Output, Traces: cp.Traces}
	if cp.Status != "done" {
		t.Status = "failed"
	}
	r.mu.Lock()
	a.Verdict, a.Expected, a.Rubric, a.Round, a.What, a.Why, a.Label, a.Fix, a.Note = d.Verdict, d.Expected, d.Rubric, d.Round, d.What, d.Why, d.Label, d.Fix, d.Note
	a.Stage = "grade"
	r.rec.Inline, r.cases, r.rec.Trials = []Case{c}, []Case{c}, []*Trial{t}
	r.save()
	r.mu.Unlock()
	r.finish(t, t.Status, "")

	r.mu.Lock()
	if d.Verdict == "failure" || !t.Pass {
		// 诊断写进开放编码，标明是模型初判；人读轨迹后可以在卡片上改写。
		r.rec.Notes = map[string]Note{t.TaskID: {Text: fmt.Sprintf("第%d次模型决定：%s。原因：%s（模型初判，待复核）", d.Round, strings.TrimRight(d.What, "。"), strings.TrimRight(d.Why, "。")), Label: d.Label, Updated: time.Now().UTC()}}
	}
	switch {
	case t.Pass && d.Verdict != "failure":
		a.Result = "评审认为这段对话达标，不做回放。"
	case d.Round < 1 || d.Round > decisions:
		a.Blocked = fmt.Sprintf("诊断给出的轮次 %d 不在 1–%d 之间", d.Round, decisions)
	case strings.TrimSpace(d.Note) == "":
		a.Blocked = "诊断没有给出可插入的纠正"
	// 有请求快照时从快照分叉；没有时只能按轮截断检查点里的 View：压缩过就没有逐轮原文了，续聊的 View 里还混着之前各轮。
	case a.snapshots[d.Round] != nil:
	case compressed(cp):
		a.Blocked = "这一轮的上下文压缩过，检查点里没有逐轮原文，不能分叉"
	case cp.Continues != "":
		a.Blocked = "这是续聊的一轮，上下文里含有之前各轮，不能按轮分叉"
	}
	if a.Blocked != "" {
		a.Result = "只做了诊断，没有回放验证：" + a.Blocked + "。"
	}
	done := a.Result != ""
	if done {
		a.Stage, r.rec.Status, r.rec.Finished = "done", "done", time.Now().UTC()
	} else {
		a.Stage = "replay"
	}
	r.save()
	r.mu.Unlock()
	if done {
		return
	}
	// 保留决定性那一步之前的完整轮次，从这一步开始改变。
	keep := d.Round - 1
	var snap *snapshot
	if m := a.snapshots[d.Round]; m != nil {
		snap = &snapshot{messages: m, calls: a.callsBefore[d.Round]}
	}
	fixed, fixErr := r.replay(ctx, t.TaskID, keep, d.Note, snap)
	placebo, placeboErr := r.replay(ctx, t.TaskID, keep, "继续。", snap)
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case fixErr != nil || placeboErr != nil:
		a.Result = "回放没能完成：" + errors.Join(fixErr, placeboErr).Error()
	case fixed >= 2 && placebo <= 1:
		a.Result = fmt.Sprintf("已证实：插入纠正后 3 次中 %d 次达标，安慰剂“继续。”只有 %d 次。决定性错误就在第 %d 次模型决定。", fixed, placebo, d.Round)
	case fixed >= 2:
		a.Result = fmt.Sprintf("未证实：纠正 %d/3 达标，但安慰剂也有 %d/3，结局变化可能只是重跑的随机性或打断本身。", fixed, placebo)
	default:
		a.Result = fmt.Sprintf("未证实：插入纠正后只有 %d/3 达标，根因可能在更早的一步、不止一处，或纠正不够具体。", fixed)
	}
	a.Stage, r.rec.Status, r.rec.Finished = "done", "done", time.Now().UTC()
	if ctx.Err() != nil {
		a.Stage, r.rec.Status = "failed", "interrupted"
		a.Error = "已停止"
	}
	r.save()
}

func compressed(cp *agent.Checkpoint) bool {
	for _, m := range cp.Messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, llm.SummaryPrefix) {
			return true
		}
	}
	return false
}

func (r *Runner) replay(ctx context.Context, from string, rounds int, note string, snap *snapshot) (int, error) {
	rp, err := r.startReplay(from, rounds, note, 3, snap)
	if err != nil {
		return 0, err
	}
	if err := r.RunReplay(ctx, rp); err != nil {
		return 0, err
	}
	passed := 0
	for _, t := range rp.Trials {
		if t.Pass {
			passed++
		}
	}
	return passed, nil
}

type diagnosis struct {
	Verdict  string `json:"verdict"`
	Expected string `json:"expected"`
	Rubric   string `json:"rubric"`
	Round    int    `json:"round"`
	What     string `json:"what"`
	Why      string `json:"why"`
	Label    string `json:"label"`
	Fix      string `json:"fix"`
	Note     string `json:"note"`
}

const auditorSystem = `你是 agent 失败分析员。给你一段 agent 对话的完整轨迹：用户问题、每一次模型决定（第 k 次）、工具调用与结果、结局。
请按以下方法分析，只输出一个 JSON 对象：
1. 先判断这段对话是否达到了用户的目的（verdict：success / failure / unclear）。没有给出最终回答、答案错误、编造事实、做了不该做的有副作用的操作、明显的无效循环，都算 failure。
2. 写出正确结果应当是什么（expected），以及一条可用于判定任何一次重跑是否达标的评分标准（rubric），写成“PASS：……FAIL：……”的具体条件，不依赖措辞和格式。
   你不知道轨迹之外的事实：正确值无法从轨迹或公认常识确定时，不要自己编数值，把标准写成行为要求（例如“给出工具结果支持的答案；资料里没有时如实说明，不编造”）。
   agent 只能使用下面列出的工具：不要因为它没做到工具做不到的事而判失败，这种情况应归为工具或资料的缺口。
3. 如果失败，只找第一个上游错误：哪一次模型决定（round，从 1 开始，对应轨迹里的“第 k 次模型决定”）是决定性的——改掉它结局就会变好。
   取最早的那一次：从那时起，agent 已经掌握足够信息，本该停下、换一条可行的路或如实报告，却没有。之后的盲目尝试、重复调用、预算耗尽都是它的连锁结果，不要选它们。
   what：这一步做了什么；why：为什么会这样（模型当时看到了什么、缺了什么信息）；label：一个简短的失败类别；fix：应该在工具描述、系统规则、执行器反馈还是别处修。
4. note：一条插在这一步之前、以用户口吻说的话，用来验证你的判断。优先补充模型缺失的事实，必要时再说明应怎么做；不要直接给出最终答案。
   note 只能要求用上面列出的工具做得到的事，注意每个工具描述里的限制（例如只能打开 http/https 网址、沙箱默认看不到项目文件）；现有工具都做不到时，就让 agent 停止尝试、如实说明拿不到什么。
成功时 round 填 0，what、why、note 留空。
格式：{"verdict":"","expected":"","rubric":"","round":0,"what":"","why":"","label":"","fix":"","note":""}`

func (r *Runner) diagnose(ctx context.Context, cp *agent.Checkpoint, trajectory string) (diagnosis, error) {
	var d diagnosis
	messages := []llm.Message{{Role: "system", Content: auditorSystem}, {Role: "user", Content: trajectory}}
	// 诊断要读完整条轨迹再推理，用高推理强度；评审每一票只判一条标准，用低强度。
	auditor := &llm.Client{Config: r.judge.client.Config, Limit: 1}
	auditor.Config.Effort = "high"
	reply, err := auditor.Call(ctx, messages, false, "eval_attribute")
	r.judge.mu.Lock()
	r.judge.Calls++
	if reply.Usage != nil {
		r.judge.Input += reply.Usage.Prompt
		r.judge.Output += reply.Usage.Completion
	}
	r.judge.mu.Unlock()
	if err != nil {
		return d, fmt.Errorf("诊断请求失败：%w", err)
	}
	if err := json.Unmarshal([]byte(jsonObject.FindString(reply.Message.Content)), &d); err != nil {
		return d, fmt.Errorf("诊断输出不是 JSON：%s", clip(reply.Message.Content, 200))
	}
	if strings.TrimSpace(d.Rubric) == "" {
		return d, errors.New("诊断没有给出评分标准")
	}
	return d, nil
}

// describe 把检查点里的 View 写成审阅模型读的轨迹：按模型决定编号，和回放保留的轮数对得上。
func describe(cp *agent.Checkpoint) (string, int) {
	var b strings.Builder
	fmt.Fprintf(&b, "用户问题：%s\n结局：%s（%s）\n", cp.Question, cp.Status, cp.Reason)
	if cp.Continues != "" {
		b.WriteString("（这是续聊中的一轮，下面包含之前各轮的上下文）\n")
	}
	decisions := 0
	for _, m := range cp.Messages {
		switch m.Role {
		case "system":
			fmt.Fprintf(&b, "\n[系统提示] %s\n", clip(m.Content, 800))
		case "user":
			if strings.HasPrefix(m.Content, llm.SummaryPrefix) {
				fmt.Fprintf(&b, "\n[上下文摘要：更早的内容已被压缩] %s\n", clip(m.Content, 800))
			} else {
				fmt.Fprintf(&b, "\n[用户] %s\n", clip(m.Content, 800))
			}
		case "assistant":
			decisions++
			fmt.Fprintf(&b, "\n第 %d 次模型决定：", decisions)
			if m.ReasoningContent != "" {
				fmt.Fprintf(&b, "\n  推理：%s", clip(m.ReasoningContent, 600))
			}
			if m.Content != "" {
				fmt.Fprintf(&b, "\n  回复：%s", clip(m.Content, 1200))
			}
			for _, call := range m.ToolCalls {
				fmt.Fprintf(&b, "\n  调用 %s(%s)", call.Function.Name, clip(call.Function.Arguments, 300))
			}
			b.WriteString("\n")
		case "tool":
			fmt.Fprintf(&b, "  结果：%s\n", clip(m.Content, 500))
		}
	}
	if cp.Status != "done" {
		fmt.Fprintf(&b, "\n运行停止：%s\n", clip(cp.Error, 600))
	}
	return b.String(), decisions
}

// toolsFrom 从检查点的 View 还原工具序列（给打分器与卡片用）。对话没有经评测运行，没有协议事件可读；
// View 压缩过的部分已不在这里，这是和评测试次的区别。
func toolsFrom(messages []llm.Message) []ToolUse {
	var uses []ToolUse
	round := 0
	index := map[string]int{}
	for _, m := range messages {
		if m.Role == "assistant" {
			round++
			for _, call := range m.ToolCalls {
				index[call.ID] = len(uses)
				uses = append(uses, ToolUse{Round: round, Name: call.Function.Name, Args: call.Function.Arguments, Status: "not_run"})
			}
		}
		if i, ok := index[m.ToolCallID]; ok && m.Role == "tool" {
			var o llm.Observation
			if json.Unmarshal([]byte(m.Content), &o) == nil {
				uses[i].Status, uses[i].Attempts = o.Status, o.Attempts
				result, _ := json.Marshal(o.Result)
				uses[i].Result = string(result)
				if o.Status != "ok" {
					uses[i].Result = o.Error
				}
			}
		}
	}
	return uses
}
