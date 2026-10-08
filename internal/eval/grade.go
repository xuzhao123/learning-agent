package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"learning-agent/internal/llm"
)

// Trial 是一道题的一次尝试。打分只用这里存下的东西，所以改了打分器可以重新打分，不必重跑。
type Trial struct {
	Case    string    `json:"case"`
	Arm     string    `json:"arm"` // with：按题目参数运行；without：消融对照组
	N       int       `json:"n"`
	TaskID  string    `json:"task_id"`
	Run     string    `json:"run,omitempty"` // 观测台里的对话ID：从观测台发起时，每个试次都能打开看轨迹
	Status  string    `json:"status"`        // pending、running、done、failed、interrupted
	Outcome string    `json:"outcome"`       // 检查点的结局：no_tool_calls、repeated_action、max_steps…
	Detail  string    `json:"detail,omitempty"`
	Answer  string    `json:"answer"`
	Tools   []ToolUse `json:"tools"`
	Model   string    `json:"model,omitempty"`
	Started time.Time `json:"started"`
	Seconds float64   `json:"seconds"`
	Calls   int       `json:"model_calls"`
	Tokens  int       `json:"tokens"`
	Traces  []string  `json:"traces,omitempty"`
	Checks  []Check   `json:"checks"`
	Score   float64   `json:"score"` // 通过的检查占比
	Pass    bool      `json:"pass"`  // 全部检查通过
}

// ToolUse 来自 B0 事件流里的 toolCall Item：和检查点不同，它不受上下文压缩影响，是完整的过程记录。
type ToolUse struct {
	Round    int    `json:"round"` // 第几次主模型请求之后发起的
	Name     string `json:"name"`
	Args     string `json:"args"`
	Status   string `json:"status"` // ok、error、unknown、not_run
	Attempts int    `json:"attempts"`
	Result   string `json:"result,omitempty"`
}

type Check struct {
	Grader   string   `json:"grader"`
	Process  bool     `json:"process"`
	WithOnly bool     `json:"with_only,omitempty"`
	Pass     bool     `json:"pass"`
	Unknown  bool     `json:"unknown,omitempty"` // 评审模型认为信息不足；按未通过算
	Detail   string   `json:"detail"`
	Votes    []string `json:"votes,omitempty"`
}

// Grade 按用例的打分器给一个试次打分。判定只有通过 / 未通过；评审模型投出“不确定”也算未通过，但单独标出。
func Grade(ctx context.Context, c Case, t *Trial, judge *Judge) {
	t.Checks = t.Checks[:0]
	for _, g := range c.Graders {
		check := Check{Grader: g.Name(), Process: g.Process(), WithOnly: g.WithOnly}
		switch g.Type {
		case "judge":
			if judge == nil {
				check.Unknown, check.Detail = true, "没有配置评审模型"
				break
			}
			check = judge.Grade(ctx, c, g, t, check)
		default:
			check.Pass, check.Detail = gradeRule(g, t)
		}
		t.Checks = append(t.Checks, check)
	}
	passed := 0
	for _, check := range t.Checks {
		if check.Pass {
			passed++
		}
	}
	t.Score = float64(passed) / float64(max(1, len(t.Checks)))
	t.Pass = passed == len(t.Checks) && t.Status == "done"
}

var numberPattern = regexp.MustCompile(`-?\d[\d,]*(?:\.\d+)?`)

func numbers(text string) []float64 {
	var values []float64
	for _, s := range numberPattern.FindAllString(text, -1) {
		if v, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64); err == nil {
			values = append(values, v)
		}
	}
	return values
}

func near(values []float64, want, tol float64) bool {
	for _, v := range values {
		if math.Abs(v-want) <= tol+1e-9 {
			return true
		}
	}
	return false
}

var shanghai = time.FixedZone("Asia/Shanghai", 8*60*60)

// 时间题的期望值不写死：取 agent 实际拿到的时间（get_current_datetime 的结果），没有调用时取试次开始时间。
func observedTime(t *Trial) time.Time {
	for _, u := range t.Tools {
		var r struct {
			Datetime string `json:"datetime"`
		}
		if u.Name == "get_current_datetime" && u.Status == "ok" && json.Unmarshal([]byte(u.Result), &r) == nil {
			if at, err := time.Parse(time.RFC3339, r.Datetime); err == nil {
				return at.In(shanghai)
			}
		}
	}
	return t.Started.In(shanghai)
}

func gradeRule(g Grader, t *Trial) (bool, string) {
	answer := t.Answer
	switch g.Type {
	case "number":
		got := numbers(answer)
		return near(got, g.Value, g.Tol), fmt.Sprintf("答案里的数值 %v", clipNumbers(got))
	case "contains":
		lower := strings.ToLower(answer)
		missing := []string{}
		for _, s := range g.All {
			if !strings.Contains(lower, strings.ToLower(s)) {
				missing = append(missing, s)
			}
		}
		anyOK := len(g.Any) == 0
		for _, s := range g.Any {
			anyOK = anyOK || strings.Contains(lower, strings.ToLower(s))
		}
		if len(missing) > 0 {
			return false, "缺少 " + strings.Join(missing, "、")
		}
		if !anyOK {
			return false, "一个候选词都没有出现"
		}
		return true, "都出现了"
	case "regex":
		matched := regexp.MustCompile(g.Pattern).FindString(answer)
		if g.Not {
			return matched == "", "匹配到 " + strconv.Quote(matched)
		}
		return matched != "", "匹配到 " + strconv.Quote(matched)
	case "weekday":
		at := observedTime(t)
		names := [][]string{{"星期日", "星期天", "周日"}, {"星期一", "周一"}, {"星期二", "周二"}, {"星期三", "周三"}, {"星期四", "周四"}, {"星期五", "周五"}, {"星期六", "周六"}}
		for _, name := range names[at.Weekday()] {
			if strings.Contains(answer, name) {
				return true, "期望 " + names[at.Weekday()][0] + "（" + at.Format("2006-01-02 15:04") + "）"
			}
		}
		return false, "期望 " + names[at.Weekday()][0] + "（" + at.Format("2006-01-02 15:04") + "）"
	case "minutes_left":
		at := observedTime(t)
		midnight := time.Date(at.Year(), at.Month(), at.Day()+1, 0, 0, 0, 0, shanghai)
		want := math.Floor(midnight.Sub(at).Minutes())
		tol := max(g.Tol, 1)
		return near(numbers(answer), want, tol), fmt.Sprintf("期望约 %.0f 分钟（%s 起算）", want, at.Format("15:04:05"))
	case "tool_used":
		count := 0
		for _, u := range t.Tools {
			if g.Tool == "*" || u.Name == g.Tool {
				count++
			}
		}
		lo, hi := g.bounds()
		return count >= lo && (hi < 0 || count <= hi), fmt.Sprintf("调用了 %d 次", count)
	case "outcome":
		outcome := t.Outcome
		if outcome == "" {
			outcome = t.Status
		}
		for _, want := range g.outcomes() {
			if outcome == want {
				return true, "结局 " + outcome
			}
		}
		return false, "结局 " + outcome
	}
	return false, "未知打分器"
}

func clipNumbers(v []float64) []float64 {
	if len(v) > 8 {
		return v[:8]
	}
	return v
}

// Judge 是评审模型：默认用 DeepSeek 评方舟 agent 的回答，避免同一个模型给自己打分。
// 每项评审投三票、两票一致才算数；每票只看一个维度（这一条 rubric），允许回答“不确定”。
type Judge struct {
	client *llm.Client
	mu     sync.Mutex
	Calls  int `json:"calls"`
	Input  int `json:"input_tokens"`
	Output int `json:"output_tokens"`
}

func NewJudge(provider string) (*Judge, error) {
	config, err := llm.LoadConfig(provider)
	if err != nil {
		return nil, fmt.Errorf("评审模型：%w", err)
	}
	config.Effort = "low"
	return &Judge{client: &llm.Client{Config: config, Limit: math.MaxInt32}}, nil
}

func (j *Judge) Model() string { return j.client.Config.Provider + "/" + j.client.Config.Model }

const judgeSystem = `你是 agent 评测的打分员。只依据给出的评分标准判断 agent 的最终回答是否达标，不考虑标准以外的因素，也不因格式、措辞或详略扣分。
参考答案只用于核对事实，不要求逐字一致。工具记录是 agent 实际执行的情况，可以用来核对回答是否如实。
只输出一个 JSON 对象，不要输出其他内容：{"verdict":"PASS 或 FAIL 或 UNKNOWN","reason":"一句话理由"}。信息不足以判断时用 UNKNOWN。`

func (j *Judge) Grade(ctx context.Context, c Case, g Grader, t *Trial, check Check) Check {
	tools := []string{}
	for _, u := range t.Tools {
		// 尝试次数要给评审：执行器自动重试，回答里说“尝试了3次”是事实，漏掉它评审会误判为编造。
		line := fmt.Sprintf("第%d轮 %s(%s) → %s，执行器共尝试 %d 次", u.Round, u.Name, clip(u.Args, 200), u.Status, u.Attempts)
		if u.Result != "" {
			line += "：" + clip(u.Result, 300)
		}
		tools = append(tools, line)
	}
	if len(tools) == 0 {
		tools = append(tools, "（没有调用工具）")
	}
	answer := t.Answer
	if t.Status != "done" {
		answer = fmt.Sprintf("（agent 没有给出最终回答，结局：%s）", t.Outcome)
	}
	prompt := fmt.Sprintf("用户问题：\n%s\n\n参考答案：\n%s\n\n评分标准（全部满足为 PASS，任一不满足为 FAIL）：\n%s\n\nagent 的工具记录：\n%s\n\nagent 的最终回答：\n%s",
		c.Question, c.Reference, g.Rubric, strings.Join(tools, "\n"), answer)
	messages := []llm.Message{{Role: "system", Content: judgeSystem}, {Role: "user", Content: prompt}}
	votes := make([]string, 3)
	reasons := make([]string, 3)
	var wg sync.WaitGroup
	for i := range votes {
		wg.Go(func() {
			votes[i], reasons[i] = j.vote(ctx, messages)
		})
	}
	wg.Wait()
	count := map[string]int{}
	for i, v := range votes {
		count[v]++
		check.Votes = append(check.Votes, v+"："+reasons[i])
	}
	switch {
	case count["PASS"] >= 2:
		check.Pass = true
	case count["FAIL"] >= 2:
	default:
		check.Unknown = true
	}
	verdict := "未通过"
	if check.Pass {
		verdict = "通过"
	} else if check.Unknown {
		verdict = "不确定"
	}
	check.Detail = fmt.Sprintf("%s（PASS %d / FAIL %d / UNKNOWN %d）", verdict, count["PASS"], count["FAIL"], count["UNKNOWN"]+count["ERROR"])
	return check
}

var jsonObject = regexp.MustCompile(`(?s)\{.*\}`)

func (j *Judge) vote(ctx context.Context, messages []llm.Message) (string, string) {
	reply, err := j.client.Call(ctx, messages, false, "eval_judge")
	j.mu.Lock()
	j.Calls++
	if reply.Usage != nil {
		j.Input += reply.Usage.Prompt
		j.Output += reply.Usage.Completion
	}
	j.mu.Unlock()
	if err != nil {
		return "ERROR", err.Error()
	}
	var v struct{ Verdict, Reason string }
	if err := json.Unmarshal([]byte(jsonObject.FindString(reply.Message.Content)), &v); err != nil {
		return "ERROR", "评审输出不是 JSON：" + clip(reply.Message.Content, 120)
	}
	v.Verdict = strings.ToUpper(strings.TrimSpace(v.Verdict))
	if v.Verdict != "PASS" && v.Verdict != "FAIL" {
		v.Verdict = "UNKNOWN"
	}
	return v.Verdict, v.Reason
}

// Clip 把空白压成单个空格并截到 n 个字符；观测台重建轨迹时也用它。
func Clip(s string, n int) string { return clip(s, n) }

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

var errStopped = errors.New("评测已停止")
