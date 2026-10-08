package eval

import (
	"fmt"
	"strings"
)

// D17 归因卡片：每个没通过的试次一张，只摆线索，不替人下结论。
// 自动定位“第一个出错的步骤”并不可靠（Who&When 里最好的方法也只有约 14% 找对步骤），
// 所以卡片给出第一个异常信号和完整的工具序列，根因由人读轨迹后写进开放编码，再归类计数。
type Card struct {
	TaskID   string   `json:"task_id"`
	Run      string   `json:"run,omitempty"`
	Case     string   `json:"case"`
	Arm      string   `json:"arm"`
	N        int      `json:"n"`
	Question string   `json:"question"`
	Outcome  string   `json:"outcome"`
	Failed   []string `json:"failed"`  // 没通过的检查与原因
	Signal   string   `json:"signal"`  // 轨迹里第一个异常信号
	Round    int      `json:"round"`   // 信号出现在第几轮；0 表示轨迹里没有异常（静默失败）
	Steps    []string `json:"steps"`   // 工具序列：第几轮、调用、结局
	Rounds   int      `json:"rounds"`  // 完整轮数：回放时可以保留 0 到 Rounds 轮
	Sibling  string   `json:"sibling"` // 同一道题其余尝试的结果：稳定失败还是偶发
	Note     *Note    `json:"note,omitempty"`
}

func Cards(rec *Record, cases []Case) []Card {
	info := map[string]Case{}
	for _, c := range cases {
		info[c.ID] = c
	}
	cards := []Card{} // 页面直接读 length：没有失败时也要是 []，不是 null
	for _, t := range rec.Trials {
		if !finished(t) || t.Pass || t.Arm != "with" {
			continue
		}
		card := Card{TaskID: t.TaskID, Run: t.Run, Case: t.Case, Arm: t.Arm, N: t.N, Question: info[t.Case].Question, Outcome: t.Outcome}
		for _, check := range t.Checks {
			if !check.Pass {
				card.Failed = append(card.Failed, check.Grader+" —— "+check.Detail)
			}
		}
		for _, u := range t.Tools {
			card.Steps = append(card.Steps, fmt.Sprintf("第%d轮 %s(%s) → %s", u.Round, u.Name, clip(u.Args, 80), u.Status))
			card.Rounds = max(card.Rounds, u.Round)
		}
		// 熔断拦下的那一批没有执行、不产生 toolCall，所以工具序列里最大的轮次就是最后一轮完整的工具调用。
		card.Signal, card.Round = firstSignal(t)
		passed, total := 0, 0
		for _, other := range rec.Trials {
			if other.Case == t.Case && other.Arm == "with" && finished(other) {
				total++
				if other.Pass {
					passed++
				}
			}
		}
		card.Sibling = fmt.Sprintf("这道题 %d 次尝试通过 %d 次", total, passed)
		if passed == 0 {
			card.Sibling += "：稳定失败"
		} else {
			card.Sibling += "：偶发失败"
		}
		if n, ok := rec.Notes[t.TaskID]; ok {
			card.Note = &n
		}
		cards = append(cards, card)
	}
	return cards
}

// 按时间顺序找第一个异常：工具没成功、同一调用重复、结局不是正常回答；都没有就是“静默失败”——
// 过程看起来正常，答案却不对，这类最难归因，需要对照参考答案读模型的推理。
func firstSignal(t *Trial) (string, int) {
	seen := map[string]int{}
	for _, u := range t.Tools {
		if u.Status != "ok" {
			return fmt.Sprintf("第%d轮 %s 返回 %s：%s", u.Round, u.Name, u.Status, clip(u.Result, 160)), u.Round
		}
		key := u.Name + u.Args
		if seen[key]++; seen[key] == 2 {
			return fmt.Sprintf("第%d轮 再次以相同参数调用 %s", u.Round, u.Name), u.Round
		}
	}
	if t.Outcome != "no_tool_calls" {
		return "结局 " + t.Outcome + "：" + clip(t.Detail, 160), 0
	}
	for _, check := range t.Checks {
		if check.Process && !check.Pass {
			return "过程不符：" + check.Grader + "（" + check.Detail + "）", 0
		}
	}
	return "静默失败：轨迹没有报错，答案未通过检查", 0
}

// 命令行输出卡片。
func printCards(cards []Card) {
	if len(cards) == 0 {
		fmt.Println("没有失败的试次。")
		return
	}
	for _, c := range cards {
		fmt.Printf("\n■ %s（%s #%d）\n  问题：%s\n  结局：%s；%s\n  第一个异常信号：%s\n", c.TaskID, c.Case, c.N, clip(c.Question, 80), c.Outcome, c.Sibling, c.Signal)
		for _, f := range c.Failed {
			fmt.Println("  ✗", f)
		}
		for _, s := range c.Steps {
			fmt.Println("   ", s)
		}
		if c.Note != nil {
			fmt.Printf("  开放编码：%s ｜ 归类：%s\n", c.Note.Text, c.Note.Label)
		}
	}
	fmt.Println("\n" + strings.Repeat("─", 40))
	fmt.Println("读轨迹，只记第一个上游错误：go run . eval note <评测ID> <任务ID> -text '…' -label '类别'")
}
