package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"learning-agent/internal/audit"
	"learning-agent/internal/tenant"
	"learning-agent/internal/workspace"
)

// D29 按轮撤销：把某一轮以及它之后各轮改过的文件放回那一轮开始之前的样子，对话也退回到它的上一轮。
// 文件和对话一起退：之后接着聊时，模型看到的历史（上一轮的检查点）和磁盘上的文件是同一个时刻的。
// 只撤销文件、保留对话，模型会以为自己改过的东西还在；只退对话、不动文件，模型会对着一份它没见过的代码继续改。
//
// 被撤销的轮次不删除，在快照目录里留一个 rewound.json：检查点、调用链、审计都还在，只是不能再续跑或接着聊。

// SnapshotDir 是某一轮的快照目录，和检查点放在一起。
func SnapshotDir(id string) string { return filepath.Join(checkpointDir(tenant.Current), id+".files") }

type rewindMark struct {
	At     time.Time `json:"at"`
	Target string    `json:"target"` // 撤销从哪一轮开始（这一轮及之后都撤销了）
}

// Rewound 判断这一轮是否已经被撤销。
func Rewound(id string) bool {
	_, err := os.Stat(filepath.Join(SnapshotDir(id), "rewound.json"))
	return err == nil
}

// RewindResult 是一次撤销（或预览）的结果，观测台据此显示和确认。
type RewindResult struct {
	Turns     []string `json:"turns"`     // 撤销的各轮，从新到旧
	Continues string   `json:"continues"` // 撤销后对话接着的那一轮；空表示连第一轮也撤销了
	DryRun    bool     `json:"dry_run"`
	*workspace.Plan
}

// successors 找出接着 id 聊的、没被撤销的下一轮。只读每个检查点的 continues 字段。
func successors(id string) []string {
	files, _ := filepath.Glob(filepath.Join(checkpointDir(tenant.Current), "*.json"))
	next := []string{}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			continue
		}
		var cp struct {
			ID        string `json:"id"`
			Continues string `json:"continues"`
		}
		if json.Unmarshal(data, &cp) == nil && cp.Continues == id && !Rewound(cp.ID) {
			next = append(next, cp.ID)
		}
	}
	return next
}

// Rewind 撤销 target 这一轮和它之后的各轮。之后的轮次沿续接链往后找，不用调用方给出；
// 同一轮被接着聊过两次（命令行上分了叉）时不知道磁盘上是哪一支的结果，拒绝。
func Rewind(target string, dryRun bool) (*RewindResult, error) {
	cp, err := LoadCheckpoint(target)
	if err != nil {
		return nil, fmt.Errorf("找不到这一轮的检查点 %s：%w", target, err)
	}
	if strings.Contains(target, "-sub-") {
		return nil, errors.New("子 agent 的改动属于派出它的那一轮，请撤销那一轮")
	}
	if Rewound(target) {
		return nil, fmt.Errorf("%s 已经撤销过了", target)
	}
	turns := []string{target}
	for {
		next := successors(turns[len(turns)-1])
		if len(next) > 1 {
			return nil, fmt.Errorf("%s 之后有 %d 个分支（%s），不知道磁盘上是哪一支的结果", turns[len(turns)-1], len(next), strings.Join(next, "、"))
		}
		if len(next) == 0 {
			break
		}
		turns = append(turns, next[0])
	}
	slices.Reverse(turns)
	// 撤销期间这几轮都不能有进程在跑：在跑的那一轮会接着写文件、写检查点。锁一直拿到撤销做完。
	for _, id := range turns {
		unlock, err := LockCheckpoint(id)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	dirs := make([]string, len(turns))
	for i, id := range turns {
		dirs[i] = SnapshotDir(id)
	}
	plan, err := workspace.PlanRewind(dirs)
	if err != nil {
		return nil, err
	}
	result := &RewindResult{Turns: turns, Continues: cp.Continues, DryRun: dryRun, Plan: plan}
	if dryRun {
		return result, nil
	}
	if len(plan.Conflicts) > 0 {
		return result, plan.Apply()
	}
	// D21：撤销改的是用户的仓库，先记下谁要求的，写不进审计就不做；结局另记一条。
	ctx := context.Background()
	request := audit.Record{Actor: "user", Event: "rewind_request", Task: target, Target: target,
		Detail: fmt.Sprintf("撤销 %d 轮：恢复 %d 个文件，删除 %d 个新建文件", len(turns), len(plan.Restore), len(plan.Delete))}
	if err := audit.Write(ctx, request); err != nil {
		return nil, fmt.Errorf("审计日志写入失败，没有撤销：%w", err)
	}
	if err := plan.Apply(); err != nil {
		audit.Log(ctx, audit.Record{Actor: "program", Event: "rewind", Task: target, Target: target, Status: "error", Detail: err.Error()})
		return result, err
	}
	// 文件都恢复了才标记：中途失败时这几轮仍是未撤销，可以再执行一次，已恢复的文件会跳过。
	mark, _ := json.Marshal(rewindMark{At: time.Now().UTC(), Target: target})
	for _, id := range turns {
		if err := os.MkdirAll(SnapshotDir(id), 0o700); err != nil {
			return result, err
		}
		if err := os.WriteFile(filepath.Join(SnapshotDir(id), "rewound.json"), append(mark, '\n'), 0o600); err != nil {
			return result, err
		}
	}
	audit.Log(ctx, audit.Record{Actor: "program", Event: "rewind", Task: target, Target: target, Status: "ok",
		Detail: fmt.Sprintf("撤销 %d 轮，对话退回到 %s", len(turns), cmp.Or(cp.Continues, "第一轮之前"))})
	return result, nil
}

// RewindCommand 是 go run . rewind：命令行与观测台（“撤销”按钮）共用。结果以 JSON 输出。
func RewindCommand(args []string) error {
	flags := flag.NewFlagSet("rewind", flag.ContinueOnError)
	flags.StringVar(&tenant.Current, "tenant", "", "这一轮所属的租户；不给时是平台空间 .data/")
	dryRun := flags.Bool("dry-run", false, "只列出会恢复、删除哪些文件以及冲突，不改动")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 1 || !ValidID(flags.Arg(0)) {
		return errors.New("用法：go run . rewind [-tenant 租户] [-dry-run] <轮次的检查点ID>")
	}
	if tenant.Current != "" && !tenant.Valid(tenant.Current) {
		return errors.New("tenant 只能是小写字母、数字和连字符")
	}
	audit.Task, audit.Conversation = flags.Arg(0), os.Getenv("AGENT_CONVERSATION_ID")
	result, err := Rewind(flags.Arg(0), *dryRun)
	if result != nil {
		data, _ := json.MarshalIndent(result, "", "  ")
		fmt.Println(string(data))
	}
	return err
}
