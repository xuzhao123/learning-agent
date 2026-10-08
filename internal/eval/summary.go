package eval

import (
	"math"
	"slices"
	"sort"
)

// Summary 由记录现算，不存盘：改了打分器、重新打分后，报告自然跟着变。
type Summary struct {
	Main     Score       `json:"main"`               // 主分：不含 env:network 题、不含消融对照组
	Network  *Score      `json:"network,omitempty"`  // 依赖外网的题，单独报告
	Suites   []Score     `json:"suites"`             // 回归集、能力集、留出题分别统计
	Cases    []CaseScore `json:"cases"`              // 逐题
	Ablation []Delta     `json:"ablation,omitempty"` // 消融：有 / 无某个能力的分差
	Compare  *Compare    `json:"compare,omitempty"`  // 与基线逐题对比
	Labels   []Count     `json:"labels,omitempty"`   // D17 人工归类的计数
	Progress struct {
		Done  int `json:"done"`
		Total int `json:"total"`
	} `json:"progress"`
}

// Score：pass@1 是逐题通过率的平均；SE 按题聚类（同一道题的几次尝试不独立，先在题内平均再算方差）；
// pass^k 是 k 次全部通过的题所占比例。完成率只看 agent 有没有给出最终回答，和答对分开报告。
type Score struct {
	Name       string  `json:"name"`
	Cases      int     `json:"cases"`
	Trials     int     `json:"trials"`
	Passed     int     `json:"passed"`
	Pass1      float64 `json:"pass1"`
	SE         float64 `json:"se"`
	PassK      float64 `json:"passk"`
	K          int     `json:"k"`
	Completion float64 `json:"completion"`
	Tokens     float64 `json:"tokens_per_trial"`
	TokensPass float64 `json:"tokens_per_pass"` // 每答对一次花的 token（答错的花费也算进去）
	Seconds    float64 `json:"p50_seconds"`
}

type CaseScore struct {
	ID       string  `json:"id"`
	Group    string  `json:"group"`
	Suite    string  `json:"suite"`
	Holdout  bool    `json:"holdout"`
	Network  bool    `json:"network"`
	Question string  `json:"question"`
	Trials   int     `json:"trials"`
	Passed   int     `json:"passed"`
	Pass1    float64 `json:"pass1"`
	AllPass  bool    `json:"all_pass"`
	Flaky    bool    `json:"flaky"` // 有过有不过：偶发失败，和稳定失败分开看
}

type Delta struct {
	Case    string  `json:"case"`
	Flag    string  `json:"flag"`
	With    float64 `json:"with"`    // 实验组平均分（不计 with_only 检查）
	Without float64 `json:"without"` // 对照组平均分
	Delta   float64 `json:"delta"`
}

// Compare 是与基线的配对比较：只比两边都跑过的题，逐题求差，题目难度的差异因此抵消。
type Compare struct {
	Baseline  string   `json:"baseline"`
	Common    int      `json:"common"`
	MeanDiff  float64  `json:"mean_diff"`
	SE        float64  `json:"se"`
	Regressed []string `json:"regressed"` // 基线 k 次全过，这次没有全过
	Fixed     []string `json:"fixed"`     // 基线一次都没过，这次全过
	Changed   []string `json:"changed"`   // 其余有变化的题
}

type Count struct {
	Label string `json:"label"`
	N     int    `json:"n"`
}

func Summarize(rec *Record, cases []Case, baseline *Record) Summary {
	s := Summary{Suites: []Score{}}
	info := map[string]Case{}
	for _, c := range cases {
		info[c.ID] = c
	}
	for _, t := range rec.Trials {
		s.Progress.Total++
		if t.Status != "pending" && t.Status != "running" {
			s.Progress.Done++
		}
	}
	with := func(t *Trial) bool { return t.Arm == "with" && finished(t) }
	s.Cases = caseScores(rec.Trials, info, with)
	s.Main = score("主分", rec, s.Cases, func(c CaseScore) bool { return !c.Network })
	if slices.ContainsFunc(s.Cases, func(c CaseScore) bool { return c.Network }) {
		network := score("依赖外网", rec, s.Cases, func(c CaseScore) bool { return c.Network })
		s.Network = &network
	}
	for _, part := range []struct {
		name string
		keep func(CaseScore) bool
	}{
		{"回归集", func(c CaseScore) bool { return !c.Network && c.Suite == "regression" }},
		{"能力集", func(c CaseScore) bool { return !c.Network && c.Suite == "capability" && !c.Holdout }},
		{"留出题", func(c CaseScore) bool { return !c.Network && c.Holdout }},
	} {
		if sc := score(part.name, rec, s.Cases, part.keep); sc.Cases > 0 {
			s.Suites = append(s.Suites, sc)
		}
	}
	s.Ablation = ablation(rec, info)
	if baseline != nil {
		s.Compare = compare(baseline, s.Cases, info)
	}
	labels := map[string]int{}
	for _, n := range rec.Notes {
		if n.Label != "" {
			labels[n.Label]++
		}
	}
	for label, n := range labels {
		s.Labels = append(s.Labels, Count{label, n})
	}
	slices.SortFunc(s.Labels, func(a, b Count) int { return b.N - a.N })
	return s
}

func finished(t *Trial) bool { return t.Status == "done" || t.Status == "failed" }

func caseScores(trials []*Trial, info map[string]Case, keep func(*Trial) bool) []CaseScore {
	byCase := map[string]*CaseScore{}
	var order []string
	for _, t := range trials {
		if !keep(t) {
			continue
		}
		cs := byCase[t.Case]
		if cs == nil {
			c := info[t.Case]
			cs = &CaseScore{ID: t.Case, Group: c.Group, Suite: c.Suite, Holdout: c.Holdout, Network: c.Network(), Question: c.Question}
			byCase[t.Case] = cs
			order = append(order, t.Case)
		}
		cs.Trials++
		if t.Pass {
			cs.Passed++
		}
	}
	list := []CaseScore{}
	for _, id := range order {
		cs := byCase[id]
		cs.Pass1 = float64(cs.Passed) / float64(cs.Trials)
		cs.AllPass = cs.Passed == cs.Trials
		cs.Flaky = cs.Passed > 0 && !cs.AllPass
		list = append(list, *cs)
	}
	return list
}

func score(name string, rec *Record, cases []CaseScore, keep func(CaseScore) bool) Score {
	sc := Score{Name: name, K: rec.K}
	var rates []float64
	ids := map[string]bool{}
	for _, c := range cases {
		if !keep(c) {
			continue
		}
		ids[c.ID] = true
		sc.Cases++
		sc.Trials += c.Trials
		sc.Passed += c.Passed
		rates = append(rates, c.Pass1)
		if c.AllPass {
			sc.PassK++
		}
	}
	if sc.Cases == 0 {
		return sc
	}
	sc.Pass1, sc.SE = meanSE(rates)
	sc.PassK /= float64(sc.Cases)
	done, tokens := 0, 0
	var seconds []float64
	for _, t := range rec.Trials {
		if t.Arm != "with" || !ids[t.Case] || !finished(t) {
			continue
		}
		tokens += t.Tokens
		seconds = append(seconds, t.Seconds)
		if t.Status == "done" {
			done++
		}
	}
	sc.Completion = float64(done) / float64(max(1, sc.Trials))
	sc.Tokens = float64(tokens) / float64(max(1, sc.Trials))
	if sc.Passed > 0 {
		sc.TokensPass = float64(tokens) / float64(sc.Passed)
	}
	sort.Float64s(seconds)
	if len(seconds) > 0 {
		sc.Seconds = seconds[(len(seconds)-1)/2]
	}
	return sc
}

// 均值与标准误：SE = sqrt(样本方差 / n)。每个值已经是一道题内几次尝试的平均，所以这就是按题聚类的标准误。
func meanSE(values []float64) (float64, float64) {
	n := float64(len(values))
	if n == 0 {
		return 0, 0
	}
	mean := 0.0
	for _, v := range values {
		mean += v
	}
	mean /= n
	if n < 2 {
		return mean, 0
	}
	variance := 0.0
	for _, v := range values {
		variance += (v - mean) * (v - mean)
	}
	return mean, math.Sqrt(variance / (n - 1) / n)
}

// 消融的分数不计 with_only 检查（例如“调用了 search_docs”）：对照组根本没有这个工具，计入只会虚增分差。
func ablation(rec *Record, info map[string]Case) []Delta {
	sums := map[string]*[4]float64{} // with 总分、with 次数、without 总分、without 次数
	var order []string
	for _, t := range rec.Trials {
		c := info[t.Case]
		if c.Ablation == "" || !finished(t) || (t.Arm != "with" && t.Arm != "without") {
			continue
		}
		total, passed := 0, 0
		for _, check := range t.Checks {
			if !check.WithOnly {
				total++
				if check.Pass {
					passed++
				}
			}
		}
		if total == 0 {
			continue
		}
		v := sums[t.Case]
		if v == nil {
			v = &[4]float64{}
			sums[t.Case] = v
			order = append(order, t.Case)
		}
		i := 0
		if t.Arm == "without" {
			i = 2
		}
		v[i] += float64(passed) / float64(total)
		v[i+1]++
	}
	var list []Delta
	for _, id := range order {
		v := sums[id]
		if v[1] == 0 || v[3] == 0 {
			continue
		}
		d := Delta{Case: id, Flag: info[id].Ablation, With: v[0] / v[1], Without: v[2] / v[3]}
		d.Delta = d.With - d.Without
		list = append(list, d)
	}
	return list
}

func compare(baseline *Record, current []CaseScore, info map[string]Case) *Compare {
	before := caseScores(baseline.Trials, info, func(t *Trial) bool { return t.Arm == "with" && finished(t) })
	old := map[string]CaseScore{}
	for _, c := range before {
		old[c.ID] = c
	}
	cmp := &Compare{Baseline: baseline.ID, Regressed: []string{}, Fixed: []string{}, Changed: []string{}}
	var diffs []float64
	for _, c := range current {
		b, ok := old[c.ID]
		if !ok || c.Network {
			continue // 和主分一样不比依赖外网的题：外部网站的变化不算代码回归
		}
		cmp.Common++
		diffs = append(diffs, c.Pass1-b.Pass1)
		switch {
		case b.AllPass && !c.AllPass:
			cmp.Regressed = append(cmp.Regressed, c.ID)
		case b.Passed == 0 && c.AllPass:
			cmp.Fixed = append(cmp.Fixed, c.ID)
		case b.Pass1 != c.Pass1:
			cmp.Changed = append(cmp.Changed, c.ID)
		}
	}
	cmp.MeanDiff, cmp.SE = meanSE(diffs)
	return cmp
}
