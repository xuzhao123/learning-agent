package eval

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

const usage = `用法：
  go run . eval [-set dev|holdout|all] [-k 3] [-workers 3] [-ablation=true] [-case a,b] [-baseline 评测ID] [-judge deepseek] [-- 公共agent参数]
  go run . eval list
  go run . eval report <评测ID> [-baseline 评测ID] [-regrade]
  go run . eval resume <评测ID>                       续跑中途停下的试次
  go run . eval triage <评测ID>                       D17 归因卡片
  go run . eval note <评测ID> <任务ID> -text '…' -label '类别'
  go run . eval replay <评测ID> <任务ID> -rounds N -note '纠正' [-n 3]`

// Run 是 go run . eval 的入口。评测记录写在 .data/eval/，观测台的“D16 评测”页面读同一份记录。
func Run(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	command := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	switch command {
	case "":
		return runNew(ctx, args)
	case "list":
		for _, rec := range List() {
			s := Summarize(rec, nil, nil)
			fmt.Printf("%s  %-11s set=%-7s k=%d  pass@1=%.0f%%  pass^k=%.0f%%  试次 %d/%d  %s\n", rec.ID, rec.Status, rec.Set, rec.K, s.Main.Pass1*100, s.Main.PassK*100, s.Progress.Done, s.Progress.Total, rec.Commit)
		}
		return nil
	}
	if len(args) == 0 {
		return errors.New(usage)
	}
	id, args := args[0], args[1:]
	flags := flag.NewFlagSet("eval "+command, flag.ContinueOnError)
	baseline := flags.String("baseline", "", "与这次评测逐题对比")
	regrade := flags.Bool("regrade", false, "用当前的用例与评审模型重新打分（会请求评审模型，不重跑 agent）")
	judge := flags.String("judge", "deepseek", "评审模型的供应商")
	text := flags.String("text", "", "开放编码：第一个上游错误是什么")
	label := flags.String("label", "", "归类：失败类别")
	rounds := flags.Int("rounds", -1, "回放时保留前几轮")
	note := flags.String("note", "", "回放时插入的纠正")
	n := flags.Int("n", 3, "回放几次")
	workers := flags.Int("workers", 3, "续跑时同时运行的试次数")
	var task string
	if (command == "note" || command == "replay") && len(args) > 0 {
		task, args = args[0], args[1:]
	}
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New(usage)
	}
	needJudge := ""
	if *regrade || command == "replay" || command == "resume" {
		needJudge = *judge
	}
	r, err := Open(id, needJudge, nil)
	if err != nil {
		return err
	}
	switch command {
	case "report":
		if *regrade {
			if err := r.Regrade(ctx); err != nil {
				return err
			}
		}
		if *baseline == "" {
			*baseline = r.rec.Baseline
		}
		return report(r, *baseline)
	case "resume":
		fmt.Printf("续跑 %d 个试次\n", r.Reopen())
		if err := r.Run(ctx, *workers); err != nil {
			return err
		}
		return report(r, r.rec.Baseline)
	case "triage":
		printCards(Cards(r.Snapshot(), r.cases))
		return nil
	case "note":
		if task == "" {
			return errors.New(usage)
		}
		return r.Annotate(task, *text, *label)
	case "replay":
		if task == "" || *rounds < 0 {
			return errors.New(usage)
		}
		rp, err := r.StartReplay(task, *rounds, *note, *n)
		if err != nil {
			return err
		}
		if err := r.RunReplay(ctx, rp); err != nil {
			return err
		}
		printReplay(rp)
		return nil
	}
	return errors.New(usage)
}

func runNew(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("eval", flag.ContinueOnError)
	o := Options{}
	flags.StringVar(&o.CasesFile, "cases", "eval/cases.jsonl", "评测集文件")
	flags.StringVar(&o.Set, "set", "dev", "dev：不含留出题；holdout：只跑留出题；all：全部")
	flags.IntVar(&o.K, "k", 3, "每道题跑几次（pass^k 的 k），1 到 5")
	flags.IntVar(&o.Workers, "workers", 3, "同时运行的试次数，1 到 8")
	flags.BoolVar(&o.Ablation, "ablation", true, "带 ablation 字段的题再跑一组去掉该参数的对照")
	flags.StringVar(&o.Judge, "judge", "deepseek", "评审模型的供应商（不要和被测 agent 相同，避免自评偏差）")
	flags.StringVar(&o.Baseline, "baseline", "", "与这次评测逐题对比，出现回归时退出码非零")
	only := flags.String("case", "", "只跑这些用例，逗号分隔")
	if err := flags.Parse(args); err != nil {
		return errors.New(usage)
	}
	rest := flags.Args()
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	o.Args = rest
	if *only != "" {
		o.Only = strings.Split(*only, ",")
	}
	r, err := Prepare(o)
	if err != nil {
		return err
	}
	fmt.Printf("Eval: id=%s trials=%d set=%s k=%d judge=%s record=%s log=%s.log\n", r.rec.ID, len(r.rec.Trials), o.Set, o.K, r.rec.Judge, path(r.rec.ID), strings.TrimSuffix(path(r.rec.ID), ".json"))
	if err := r.Run(ctx, o.Workers); err != nil {
		return err
	}
	return report(r, o.Baseline)
}

func report(r *Runner, baselineID string) error {
	rec := r.Snapshot()
	var baseline *Record
	if baselineID != "" {
		var err error
		if baseline, err = Load(baselineID); err != nil {
			return err
		}
	}
	s := Summarize(rec, r.cases, baseline)
	pct := func(v float64) string { return fmt.Sprintf("%.0f%%", v*100) }
	fmt.Printf("\n评测 %s（%s）· commit %s · 题目 %s@%s · 模型 %s · 评审 %s\n", rec.ID, rec.Status, rec.Commit, rec.CasesFile, rec.CasesHash, rec.Model, rec.Judge)
	line := func(sc Score) {
		fmt.Printf("  %-8s %2d 题 × k=%d：pass@1 %s ± %s（95%% 区间 ±1.96 SE）  pass^%d %s  完成率 %s  每试次 %.0f tokens  每次答对 %.0f tokens  P50 %.1fs\n",
			sc.Name, sc.Cases, sc.K, pct(sc.Pass1), pct(1.96*sc.SE), sc.K, pct(sc.PassK), pct(sc.Completion), sc.Tokens, sc.TokensPass, sc.Seconds)
	}
	line(s.Main)
	for _, sc := range s.Suites {
		line(sc)
	}
	if s.Network != nil {
		line(*s.Network)
	}
	fmt.Println("\n逐题：")
	for _, c := range s.Cases {
		mark := "✓"
		if !c.AllPass {
			mark = "✗"
			if c.Flaky {
				mark = "~"
			}
		}
		fmt.Printf("  %s %-18s %-8s %d/%d  %s\n", mark, c.ID, c.Group, c.Passed, c.Trials, clip(c.Question, 50))
	}
	if len(s.Ablation) > 0 {
		fmt.Println("\n消融（不计只有实验组可能通过的检查）：")
		for _, d := range s.Ablation {
			fmt.Printf("  %-18s 有 %s %.2f  无 %.2f  Δ %+.2f\n", d.Case, d.Flag, d.With, d.Without, d.Delta)
		}
	}
	if c := s.Compare; c != nil {
		fmt.Printf("\n与基线 %s 配对比较（%d 道共同的题）：平均差 %+.1f 个百分点 ± %.1f（1.96 SE）\n  回归：%v\n  修复：%v\n  其他变化：%v\n",
			c.Baseline, c.Common, c.MeanDiff*100, 1.96*c.SE*100, c.Regressed, c.Fixed, c.Changed)
	}
	fmt.Printf("\n评审模型：%d 次请求，%d tokens。归因：go run . eval triage %s\n", rec.JudgeUse.Calls, rec.JudgeUse.Input+rec.JudgeUse.Output, rec.ID)
	if s.Compare != nil && len(s.Compare.Regressed) > 0 {
		return fmt.Errorf("%d 道题相对基线回归", len(s.Compare.Regressed))
	}
	return nil
}

func printReplay(rp *Replay) {
	passed := 0
	for _, t := range rp.Trials {
		if t.Pass {
			passed++
		}
	}
	fmt.Printf("回放 %s：从 %s 保留前 %d 轮，插入「%s」，%d 次中 %d 次通过\n", rp.ID, rp.From, rp.Rounds, rp.Note, len(rp.Trials), passed)
	for _, t := range rp.Trials {
		fmt.Printf("  %s %s 结局 %s：%s\n", map[bool]string{true: "✓", false: "✗"}[t.Pass], t.TaskID, t.Outcome, clip(t.Answer, 80))
	}
}
