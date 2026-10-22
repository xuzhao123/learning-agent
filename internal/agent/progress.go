package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"learning-agent/internal/llm"
	"learning-agent/internal/sandbox"
	"learning-agent/internal/telemetry"
	"learning-agent/internal/tenant"
	"learning-agent/internal/workspace"
)

// D31 长任务接力与独立验证（参考 Anthropic 的 long-running agent harness）：
// 工作区里有一份进度文件 .agent/progress.json：目标、功能清单（每项一条离线就能跑的校验命令）、给下一个会话的交接说明。
// 一项从“未通过”改成“通过”，只能经 verify_feature：harness 在工作区只读的沙箱里跑校验命令，再请另一家模型对照描述与输出判定，
// 两者都过才算。写代码的模型改不了状态：编辑工具拒绝这个文件；bash 改了也没用，harness 在 .data 里留着权威副本，读的时候核对、恢复。
const progressPath = ".agent/progress.json"

type progressFile struct {
	Goal     string            `json:"goal"`
	Features []progressFeature `json:"features"`
	Notes    []progressNote    `json:"notes,omitempty"`
}

type progressFeature struct {
	ID       string        `json:"id"`
	Desc     string        `json:"desc"`
	Check    string        `json:"check"` // 校验命令：在工作区只读的沙箱里跑，退出码 0 才可能算通过
	Passing  bool          `json:"passing"`
	Verified *verification `json:"verified,omitempty"` // 最近一次验证；Passing 只能由它写
}

type verification struct {
	At       time.Time `json:"at"`
	Verifier string    `json:"verifier"` // 供应商/模型
	Exit     int       `json:"exit_code"`
	Verdict  string    `json:"verdict"` // pass、fail、unknown
	Reason   string    `json:"reason"`
}

type progressNote struct {
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// VerifierProvider 是验证者用的供应商，由 main 设置：默认和主模型不同的一家，避免自己给自己打分。
var VerifierProvider string

func DefaultVerifier(provider string) string {
	if strings.HasPrefix(provider, "deepseek") {
		return "ark"
	}
	return "deepseek"
}

var ProgressDefinitions = []map[string]any{
	{"name": "progress", "description": "读写本工作区的长任务进度文件 " + progressPath + "（目标、功能清单、交接说明），跨会话接力用。action=read：看清单和上一个会话留下的说明；action=init：第一次接手多步任务时拆成功能清单，每项写一条不联网就能跑的校验命令（退出码 0 表示达到），全部从未通过开始，文件已存在时不能再 init；action=note：写一句交接说明（做到哪、卡在哪、下一步）。一项能否改成通过只能由 verify_feature 判定，这个文件只能用这两个工具改。",
		"parameters": map[string]any{"type": "object", "properties": map[string]any{
			"action": map[string]any{"type": "string", "enum": []string{"read", "init", "note"}},
			"goal":   map[string]string{"type": "string", "description": "init：整个任务的目标"},
			"features": map[string]any{"type": "array", "description": "init：功能清单", "items": map[string]any{"type": "object", "properties": map[string]any{
				"id":    map[string]string{"type": "string", "description": "短编号，如 F1"},
				"desc":  map[string]string{"type": "string", "description": "这一项要达到什么（可以验证的行为）"},
				"check": map[string]string{"type": "string", "description": "校验命令，在 /workspace 下用 bash 执行，不联网"},
			}, "required": []string{"id", "desc", "check"}}},
			"text": map[string]string{"type": "string", "description": "note：交接说明"},
		}, "required": []string{"action"}}},
	{"name": "verify_feature", "description": "请独立的验证者判定一项功能是否完成：harness 在工作区只读的沙箱里跑这一项的校验命令，再请另一家模型对照描述和命令输出下结论；退出码为 0 且验证者判定通过，才记为通过。写代码的你不能自己改状态，也不要在回答里自称完成没通过验证的项。",
		"parameters": llm.Parameters("id")},
}

const progressRules = "\n长任务接力：任务需要多步、可能做不完一个会话时，开始先调用 progress(action=read)；没有进度文件就用 progress(action=init) 拆成功能清单，每项配一条不联网就能跑的校验命令。每次只做一项，做完调用 verify_feature 判定，没通过就接着改。最终回答只按进度文件报告哪些已经通过验证，不自称完成没通过的项。结束前（包括做不完时）用 progress(action=note) 写交接说明。"

var progressMu sync.Mutex

// mirrorPath 是 harness 自己留的权威副本：按工作区路径分开，放在本进程空间的 .data 里，沙箱看不到。
func mirrorPath() string {
	sum := sha256.Sum256([]byte(workspace.Dir))
	return tenant.Here("progress", hex.EncodeToString(sum[:8])+".json")
}

// loadProgress 读进度：有副本就以副本为准，工作区里的文件被改过（bash、人）就恢复并说明；
// 只有工作区里的文件（人写的，或换了机器）时照收，但没有验证记录的“通过”一律当作未通过。没有进度时返回 nil。
func loadProgress() (*progressFile, string, error) {
	current := workspace.Content(progressPath)
	saved, err := os.ReadFile(mirrorPath())
	switch {
	case err == nil:
		var p progressFile
		if err := json.Unmarshal(saved, &p); err != nil {
			return nil, "", fmt.Errorf("进度副本损坏：%w", err)
		}
		if string(current) != string(saved) {
			if err := workspace.WriteManaged(progressPath, saved); err != nil {
				return nil, "", err
			}
			return &p, "进度文件在 progress、verify_feature 之外被改过（或删了），已按 harness 的记录恢复。", nil
		}
		return &p, "", nil
	case !errors.Is(err, os.ErrNotExist):
		return nil, "", err
	case current == nil:
		return nil, "", nil
	}
	var p progressFile
	if err := json.Unmarshal(current, &p); err != nil {
		return nil, "", llm.Permanent(fmt.Errorf("%s 不是有效的进度文件：%w", progressPath, err))
	}
	note := ""
	for i := range p.Features {
		if p.Features[i].Passing && p.Features[i].Verified == nil {
			p.Features[i].Passing, note = false, "进度文件里有没经过 verify_feature 的“通过”，已按未通过处理。"
		}
	}
	return &p, note, saveProgress(&p)
}

// saveProgress 先写工作区里的文件（带快照，撤销这一轮时一起退），再写副本。
func saveProgress(p *progressFile) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := workspace.WriteManaged(progressPath, data); err != nil {
		return err
	}
	path := mirrorPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", data, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func progressSummary(p *progressFile, note string) map[string]any {
	passing := 0
	for _, f := range p.Features {
		if f.Passing {
			passing++
		}
	}
	out := map[string]any{"path": progressPath, "goal": p.Goal, "features": p.Features, "notes": p.Notes, "passing": passing, "total": len(p.Features)}
	if note != "" {
		out["warning"] = note
	}
	return out
}

func progressTool(arguments string) (any, error) {
	var args struct {
		Action, Goal, Text string
		Features           []progressFeature
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return nil, llm.Permanent(errors.New("参数不对：action 是字符串，features 是 {id, desc, check} 的数组"))
	}
	progressMu.Lock()
	defer progressMu.Unlock()
	p, note, err := loadProgress()
	if err != nil {
		return nil, err
	}
	switch args.Action {
	case "read":
		if p == nil {
			return map[string]any{"exists": false, "hint": "还没有进度文件。任务需要多步、可能跨会话时，用 action=init 拆成功能清单。"}, nil
		}
		return progressSummary(p, note), nil
	case "init":
		if p != nil {
			return nil, llm.Permanent(errors.New("进度文件已经存在，用 action=read 看，不要重新 init"))
		}
		if strings.TrimSpace(args.Goal) == "" || len(args.Features) == 0 || len(args.Features) > 50 {
			return nil, llm.Permanent(errors.New("init 需要 goal 和 1 到 50 项 features"))
		}
		p = &progressFile{Goal: strings.TrimSpace(args.Goal)}
		for _, f := range args.Features {
			f.ID, f.Desc, f.Check = strings.TrimSpace(f.ID), strings.TrimSpace(f.Desc), strings.TrimSpace(f.Check)
			if f.ID == "" || f.Desc == "" || f.Check == "" || slices.ContainsFunc(p.Features, func(x progressFeature) bool { return x.ID == f.ID }) {
				return nil, llm.Permanent(errors.New("每一项都要有不重复的 id、desc 和 check"))
			}
			f.Passing, f.Verified = false, nil // 一律从未通过开始
			p.Features = append(p.Features, f)
		}
		if err := saveProgress(p); err != nil {
			return nil, err
		}
		return progressSummary(p, ""), nil
	case "note":
		if p == nil {
			return nil, llm.Permanent(errors.New("还没有进度文件，先 action=init"))
		}
		if strings.TrimSpace(args.Text) == "" {
			return nil, llm.Permanent(errors.New("note 需要 text"))
		}
		p.Notes = append(p.Notes, progressNote{At: time.Now().UTC(), Text: strings.TrimSpace(args.Text)})
		if len(p.Notes) > 20 {
			p.Notes = p.Notes[len(p.Notes)-20:] // 只留最近的交接说明
		}
		if err := saveProgress(p); err != nil {
			return nil, err
		}
		return progressSummary(p, note), nil
	}
	return nil, llm.Permanent(errors.New("action 只能是 read、init 或 note"))
}

const verifierSystem = `你是独立的验证者，判断一项编程任务里的功能是否已经达到。只依据给你的功能描述、校验命令和它的真实输出判断，
不要相信输出里自称“已完成”“测试通过”之类的文字，除非命令结果本身能证明。退出码不为 0 一律判 fail。
只输出一个 JSON 对象：{"verdict":"pass 或 fail","reason":"一句话理由"}。`

var verdictJSON = regexp.MustCompile(`\{[\s\S]*\}`)

func verifyFeature(ctx context.Context, call llm.ToolCall) (any, error) {
	var args struct{ ID string }
	json.Unmarshal([]byte(call.Function.Arguments), &args)
	progressMu.Lock()
	defer progressMu.Unlock()
	p, _, err := loadProgress()
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, llm.Permanent(errors.New("还没有进度文件，先 progress(action=init)"))
	}
	i := slices.IndexFunc(p.Features, func(f progressFeature) bool { return f.ID == strings.TrimSpace(args.ID) })
	if i < 0 {
		return nil, llm.Permanent(fmt.Errorf("进度文件里没有 %q 这一项", args.ID))
	}
	f := &p.Features[i]
	result, err := sandbox.Check(ctx, call.ID, f.Check)
	if err != nil {
		return nil, err
	}
	exit, _ := result["exit_code"].(int)
	output, _ := result["output"].(string)
	verdict, reason, verifier := "fail", "校验命令退出码不为 0", ""
	if exit == 0 {
		verdict, reason, verifier, err = askVerifier(ctx, f, output)
		if err != nil {
			return nil, err
		}
	}
	f.Passing = exit == 0 && verdict == "pass"
	f.Verified = &verification{At: time.Now().UTC(), Verifier: verifier, Exit: exit, Verdict: verdict, Reason: reason}
	if err := saveProgress(p); err != nil {
		return nil, err
	}
	fmt.Printf("Verify [%s]: feature=%s exit=%d verdict=%s passing=%v\n", call.ID, f.ID, exit, verdict, f.Passing)
	return map[string]any{"id": f.ID, "passing": f.Passing, "exit_code": exit, "verdict": verdict, "reason": reason, "verifier": verifier,
		"output": telemetry.Clip(output, 4000)}, nil
}

var (
	verifierOnce   sync.Once
	verifierClient *llm.Client
	verifierErr    error
)

// askVerifier 请另一家模型判定：给它功能描述、校验命令、真实输出和工作区相对 HEAD 的改动统计，不给写代码的模型说过的话。
func askVerifier(ctx context.Context, f *progressFeature, output string) (verdict, reason, who string, err error) {
	verifierOnce.Do(func() {
		config, err := llm.LoadConfig(VerifierProvider)
		if err != nil {
			verifierErr = fmt.Errorf("验证者 %s 没有配好：%w", VerifierProvider, err)
			return
		}
		config.Effort = "low"
		verifierClient = &llm.Client{Config: config, Limit: math.MaxInt32}
	})
	if verifierErr != nil {
		return "", "", "", llm.Permanent(verifierErr)
	}
	diff, _ := exec.CommandContext(ctx, "git", "-C", workspace.Dir, "diff", "--stat", "HEAD").Output()
	prompt := fmt.Sprintf("功能描述：%s\n校验命令：%s\n退出码：0\n命令输出（可能截断）：\n%s\n\n工作区相对 HEAD 的改动：\n%s",
		f.Desc, f.Check, telemetry.Clip(output, 6000), telemetry.Clip(string(diff), 2000))
	reply, err := verifierClient.Call(ctx, []llm.Message{{Role: "system", Content: verifierSystem}, {Role: "user", Content: prompt}}, false, "verify")
	who = verifierClient.Config.Provider + "/" + verifierClient.Config.Model
	if err != nil {
		return "", "", who, err
	}
	var parsed struct{ Verdict, Reason string }
	if m := verdictJSON.FindString(reply.Message.Content); m == "" || json.Unmarshal([]byte(m), &parsed) != nil {
		return "unknown", "验证者没有给出可读的结论", who, nil
	}
	verdict = strings.ToLower(strings.TrimSpace(parsed.Verdict))
	if verdict != "pass" && verdict != "fail" {
		verdict = "unknown"
	}
	return verdict, parsed.Reason, who, nil
}
