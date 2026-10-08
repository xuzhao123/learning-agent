package eval

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"learning-agent/internal/agent"
)

// D16：一道评测题。固定的是题目、参考答案和判定标准，不是模型的回答或工具调用顺序。
type Case struct {
	ID        string   `json:"id"`
	Suite     string   `json:"suite"`   // regression：D15 用过、调试时看过的题；capability：新写的题
	Holdout   bool     `json:"holdout"` // 留出：调试时不跑，只在最后报告时运行（-set holdout|all）
	Group     string   `json:"group"`
	Tags      []string `json:"tags"` // env:network：依赖外网，单独统计，不计入主分
	Question  string   `json:"question"`
	Args      []string `json:"args"`
	Reference string   `json:"reference"` // 参考答案：证明题目可解、打分器配对，也给评审模型参照
	Ablation  string   `json:"ablation"`  // 消融：再跑一组去掉这个参数的对照，如 -rag
	Graders   []Grader `json:"graders"`
}

// Grader 是一项检查。每道题至少一项看结果（答案），一项看过程（工具、结局）；过程只查关键约束，不查顺序。
type Grader struct {
	Type     string   `json:"type"` // 结果：number、contains、regex、weekday、minutes_left、judge；过程：tool_used、outcome
	Value    float64  `json:"value,omitempty"`
	Tol      float64  `json:"tol,omitempty"`
	All      []string `json:"all,omitempty"`
	Any      []string `json:"any,omitempty"`
	Pattern  string   `json:"pattern,omitempty"`
	Not      bool     `json:"not,omitempty"`
	Tool     string   `json:"tool,omitempty"` // * 表示任意工具
	Min      *int     `json:"min,omitempty"`
	Max      *int     `json:"max,omitempty"`
	Outcomes []string `json:"outcomes,omitempty"`
	Rubric   string   `json:"rubric,omitempty"` // judge：写成具体的 PASS / FAIL 条件
	WithOnly bool     `json:"with_only,omitempty"`
}

func (g Grader) Process() bool { return g.Type == "tool_used" || g.Type == "outcome" }

// Name 是给人看的一句话，报告与观测台都用它。
func (g Grader) Name() string {
	switch g.Type {
	case "number":
		return fmt.Sprintf("答案含数值 %g（±%g）", g.Value, g.Tol)
	case "contains":
		parts := []string{}
		if len(g.All) > 0 {
			parts = append(parts, "都包含 "+strings.Join(g.All, "、"))
		}
		if len(g.Any) > 0 {
			parts = append(parts, "包含其一 "+strings.Join(g.Any, " / "))
		}
		return "答案" + strings.Join(parts, "，")
	case "regex":
		if g.Not {
			return "答案不匹配 /" + g.Pattern + "/"
		}
		return "答案匹配 /" + g.Pattern + "/"
	case "weekday":
		return "星期与工具返回的时间一致"
	case "minutes_left":
		return fmt.Sprintf("剩余分钟数与工具返回的时间一致（±%g）", g.Tol)
	case "tool_used":
		tool := g.Tool
		if tool == "*" {
			tool = "任何工具"
		}
		lo, hi := g.bounds()
		switch {
		case hi == 0:
			return "不调用 " + tool
		case hi < 0:
			return fmt.Sprintf("调用 %s 至少 %d 次", tool, lo)
		}
		return fmt.Sprintf("调用 %s %d–%d 次", tool, lo, hi)
	case "outcome":
		return "结局为 " + strings.Join(g.outcomes(), " 或 ")
	case "judge":
		return "评审：" + g.Rubric
	}
	return g.Type
}

// 都不写时表示“至少调用一次”；max 为 -1 表示不设上限。
func (g Grader) bounds() (int, int) {
	lo, hi := 1, -1
	if g.Min != nil {
		lo = *g.Min
	}
	if g.Max != nil {
		hi = *g.Max
		if g.Min == nil {
			lo = 0
		}
	}
	return lo, hi
}

func (g Grader) outcomes() []string {
	if len(g.Outcomes) == 0 {
		return []string{"no_tool_calls"}
	}
	return g.Outcomes
}

func (c Case) Network() bool { return slices.Contains(c.Tags, "env:network") }

// 消融的对照组：去掉被消融的参数，其余不变。
func (c Case) ArmArgs(arm string) []string {
	if arm == "with" || c.Ablation == "" {
		return c.Args
	}
	return slices.DeleteFunc(slices.Clone(c.Args), func(a string) bool { return a == c.Ablation })
}

// LoadCases 读评测集：每行一个 JSON，空行和 # 开头的行跳过。返回用例和文件内容的哈希（写进运行记录，说明跑的是哪一版题）。
func LoadCases(path string) ([]Case, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	var cases []Case
	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var c Case
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&c); err != nil {
			return nil, "", fmt.Errorf("%s 第%d行：%w", path, line, err)
		}
		if err := c.check(); err != nil {
			return nil, "", fmt.Errorf("%s 第%d行（%s）：%w", path, line, c.ID, err)
		}
		if seen[c.ID] {
			return nil, "", fmt.Errorf("%s 第%d行：用例 %s 重复", path, line, c.ID)
		}
		seen[c.ID] = true
		cases = append(cases, c)
	}
	return cases, hex.EncodeToString(sum[:6]), scanner.Err()
}

var knownTypes = []string{"number", "contains", "regex", "weekday", "minutes_left", "judge", "tool_used", "outcome"}

func (c Case) check() error {
	// 用例ID会拼进任务ID：eval-<评测ID>-<用例>-<组>-<序号>。
	if !agent.ValidID(c.ID) || len(c.ID) > 40 || strings.Contains(c.ID, "-sub-") {
		return fmt.Errorf("用例ID只能包含字母、数字和 ._-，最长40")
	}
	if c.Suite != "regression" && c.Suite != "capability" {
		return fmt.Errorf("suite 须为 regression 或 capability")
	}
	if strings.TrimSpace(c.Question) == "" || strings.TrimSpace(c.Reference) == "" {
		return fmt.Errorf("需要 question 和 reference")
	}
	if c.Ablation != "" && !slices.Contains(c.Args, c.Ablation) {
		return fmt.Errorf("ablation %s 不在 args 里", c.Ablation)
	}
	result, process := false, false
	for _, g := range c.Graders {
		if !slices.Contains(knownTypes, g.Type) {
			return fmt.Errorf("未知的打分器 %s", g.Type)
		}
		if g.Type == "regex" {
			if _, err := regexp.Compile(g.Pattern); err != nil {
				return err
			}
		}
		if (g.Type == "judge" && strings.TrimSpace(g.Rubric) == "") || (g.Type == "tool_used" && g.Tool == "") || (g.Type == "contains" && len(g.All)+len(g.Any) == 0) {
			return fmt.Errorf("打分器 %s 缺少参数", g.Type)
		}
		result, process = result || !g.Process(), process || g.Process()
	}
	if !result || !process {
		return fmt.Errorf("每道题至少一项看结果的打分器、一项看过程的打分器")
	}
	return nil
}
