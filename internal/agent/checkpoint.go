package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"learning-agent/internal/llm"
	"learning-agent/internal/tenant"
)

// D9 检查点：一次运行的全部可恢复状态。每到一个边界就整份原子写回 .data/checkpoints/<id>.json：
// 问题写入后、模型决定调用工具后（写前日志）、这批工具结果写回后、运行结束时。
// 续跑只依赖这个文件和其中保存的启动参数，不依赖进程内存，所以 kill -9 之后也能接着跑。
type Checkpoint struct {
	Version  int      `json:"version"` // 格式版本，见 CheckpointVersion；加版本号之前写的检查点读出来是 0
	ID       string   `json:"id"`
	Args     []string `json:"args"`     // 首次启动的命令行参数：续跑时原样恢复工具开关、预算等配置
	Question string   `json:"question"` // 也是幂等校验的依据：同一个ID不能换问题
	// 续接链：这一轮接着哪一轮的对话（上一轮的检查点ID）。每一轮都有自己的ID，旧的检查点不会被覆盖；
	// spawn_agent 按 task_id 引用子任务时，接受链上任何一轮派出的子任务。
	Continues string `json:"continues,omitempty"`
	Status    string `json:"status"`           // running：运行中，或进程已经死掉；stopped：停下了，见 reason；done：完成
	Reason    string `json:"reason,omitempty"` // 与终端 Termination 一致：no_tool_calls、cancelled、timeout、max_steps…
	Answer    string `json:"answer,omitempty"`
	Error     string `json:"error,omitempty"`
	Step      int    `json:"step"`  // 已完整结束的轮次
	Calls     int    `json:"calls"` // 已用掉的模型请求：预算跨续跑累计
	// 模型看到的 View。最后一条若是带 tool_calls 的 assistant 而后面没有结果，
	// 说明进程死在执行这批工具的途中：续跑时由 replayBatch 补齐。
	Messages []llm.Message `json:"messages"`
	// D15 指标的依据：评测的结局、耗时和成本以检查点为准（kill -9 也不会丢），trace 只用来归因（见 Q19）。
	StartedAt time.Time `json:"started_at,omitempty"` // 首次开始；续跑不变，所以耗时包含中断的那段时间
	Input     int       `json:"input_tokens"`         // 累计用量，跨续跑累加，包含摘要与记忆辅助调用
	Output    int       `json:"output_tokens"`
	Cached    int       `json:"cached_tokens"`
	Traces    []string  `json:"traces,omitempty"` // 每次运行（首次与各次续跑）的 trace_id
	Fork      *Fork     `json:"fork,omitempty"`   // D17 回放验证：从哪个检查点的第几轮分叉、注入了什么
	Summary   []string  `json:"summary"`
	Progress  progress  `json:"progress"` // 循环检测：最近的动作与见过的结果，跨续跑累计
	State     State     `json:"state"`
	// 版本 2 之前，State 里的前三项写在顶层；读的时候搬进 State（见 LoadCheckpointIn），写出时不再有。
	OldPrivate []string          `json:"private,omitempty"`
	OldGranted []string          `json:"granted,omitempty"`
	OldSeen    map[string]string `json:"seen,omitempty"`
	// O12：不正常结束时 harness 自己跑的收尾检查（复用 Stop hook 的命令）；正常结束或没配 Stop hook 时为空。
	FinalCheck *FinalCheck `json:"final_check,omitempty"`
	// D40：写这份检查点时会话记录写到了第几条：检查点是“记录到第 N 条时的状态快照”（见 journal.go）。
	LogSeq    int64     `json:"log_seq,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// O7（D38）：State 是影响下一步、要带进下一轮（续聊）和续跑的状态，收在一处，由检查点整体保存。
// 续跑取自己检查点里的；续聊取上一轮的；回放分叉只继承私有来源（只会更严），其余从空开始。
// 以前前三项是检查点的几个顶层字段，环境与用量提醒则从视图里的提醒文字反推。
// 子目录项目指令有没有进上下文仍按视图判断（见 workspace.Prime）：它描述的就是视图里有什么。
type State struct {
	Private []string `json:"private,omitempty"` // D20：已读入的私有数据来源；非空时外发类工具要用户确认
	Granted []string `json:"granted,omitempty"` // D20：用户选了“本对话内都允许”的外发范围（如 ds_search、open_page · github.com）
	// D23：“读过才能改”的账本（相对路径 → 读取时的内容哈希）。只恢复“读过”这个事实和当时的哈希，编辑前仍比对当前内容。
	Seen map[string]string `json:"seen,omitempty"`
	// D24：上一次告诉模型的环境，按字段（工作区、分支、未提交改动、日期、权限……）。续聊时只把变了的字段发出去；
	// 压缩后清空（那份提醒已经并进摘要），下一轮重发整份。
	Env map[string]string `json:"env,omitempty"`
	// D30：已经提醒过的用量档位（50、75）。续聊时上下文是接着的，档位也接着；压缩后清零。
	Announced int `json:"announced,omitempty"`
}

// FinalCheck 是收尾检查的结论：Passed 为 false 时 Detail 是 hook 给出的原因（比如编译错误）。
type FinalCheck struct {
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// CheckpointVersion 是本程序写的检查点格式。字段的含义变了（不是单纯加一个可以缺省的字段）才加一；
// 读到比它新的版本就拒绝续跑：旧程序会把不认识的字段悄悄丢掉，下一次写回时数据就没了。
// 版本 2（D38）：私有来源、外发授权与读后才写账本收进 state；旧版本读的时候搬过去。
const CheckpointVersion = 2

// CheckVersion 判断这个程序能不能读写某个版本的检查点。0 是加版本号之前的格式，与 1 相同；1 与 2 的差别在 LoadCheckpointIn 里搬。
func CheckVersion(id string, version int) error {
	if version > CheckpointVersion {
		return fmt.Errorf("检查点 %s 的格式版本是 %d，这个程序只认识到 %d：请用新版程序续跑或接着聊", id, version, CheckpointVersion)
	}
	return nil
}

// 由 main 设置：CheckpointID 为空表示不写检查点（Day 3/4 实验）；Resume 不为空表示本次是续跑。
var (
	CheckpointID   string
	CheckpointArgs []string
	Continues      string // 本轮接着的上一轮检查点ID，见 Checkpoint.Continues
	Resume         *Checkpoint
)

// 检查点在本进程所属租户的空间里（见 internal/tenant）；RunChild 替别的空间启动任务时用 …In 版本。
func checkpointDir(space string) string { return tenant.Path(space, "checkpoints") }

var validID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// ID 会拼进文件路径，只允许字母、数字和 ._-，不能含路径分隔符。
func ValidID(id string) bool { return validID.MatchString(id) && id != "." && id != ".." }

// 不给 -task-id 时用随机 UUID（v4）：不含时间，不受时区影响，几个进程同时启动也不会撞号。
// 子任务ID在它后面加 -sub- 和 task 哈希（见 subagent.go），仍是确定性的，续跑时能找回同一个子任务。
func NewCheckpointID() string { return uuid.NewString() }

func CheckpointPath(id string) string { return checkpointPathIn(tenant.Current, id) }

func checkpointPathIn(space, id string) string {
	return filepath.Join(checkpointDir(space), id+".json")
}

func LoadCheckpoint(id string) (*Checkpoint, error) { return LoadCheckpointIn(tenant.Current, id) }

// LoadCheckpointIn 读某个空间里的检查点。别的租户的任务ID在这里读不到：路径不同。
func LoadCheckpointIn(space, id string) (*Checkpoint, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("任务ID %q 只能包含字母、数字和 ._-，最长128", id)
	}
	data, err := os.ReadFile(checkpointPathIn(space, id))
	if err != nil {
		return nil, err
	}
	cp := &Checkpoint{}
	if err := json.Unmarshal(data, cp); err != nil {
		return nil, fmt.Errorf("%s 不是有效的检查点：%w", checkpointPathIn(space, id), err)
	}
	if err := CheckVersion(id, cp.Version); err != nil {
		return nil, err
	}
	if cp.Version < 2 {
		cp.State.Private, cp.State.Granted, cp.State.Seen = cp.OldPrivate, cp.OldGranted, cp.OldSeen
		cp.OldPrivate, cp.OldGranted, cp.OldSeen = nil, nil, nil
	}
	return cp, nil
}

// 先写临时文件再改名：任何时刻读到的都是一份完整的旧版本或新版本，不会是写了一半的文件。
// 没有 fsync：进程崩溃（包括 kill -9）不丢已写的数据，整机断电可能丢最后一次写入。
func (cp *Checkpoint) write() error {
	cp.Version, cp.UpdatedAt = CheckpointVersion, time.Now()
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(checkpointDir(tenant.Current), 0o700); err != nil {
		return err
	}
	path := CheckpointPath(cp.ID)
	if err := os.WriteFile(path+".tmp", append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

var ErrBusy = errors.New("这个任务正在另一个进程中运行")

// 同一个任务同一时刻只允许一个进程执行：非阻塞 flock 一直持有到进程退出。
// 进程无论怎么退出（包括 kill -9），内核都会释放锁，所以“状态是 running 但锁空着”就说明上个进程已经死了。
func LockCheckpoint(id string) (unlock func(), err error) {
	return lockCheckpointIn(tenant.Current, id)
}

func lockCheckpointIn(space, id string) (unlock func(), err error) {
	if err := os.MkdirAll(checkpointDir(space), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(checkpointDir(space), id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w：%s", ErrBusy, id)
		}
		return nil, err
	}
	return func() { lock.Close() }, nil
}

// 能续跑：进程死在中途（状态停在 running），或因取消、超时、模型请求失败而停下。
// 预算耗尽、熔断、无效调用这类原因，续跑一次结果相同，不续跑。
func Resumable(cp *Checkpoint) bool {
	return cp.Status == "running" || slices.Contains([]string{"cancelled", "timeout", "model_error", "checkpoint_error"}, cp.Reason)
}

// 从检查点重建上下文；末尾没有结果的 tool_calls 单独取出，作为待补齐的一批。
func resumeContext(cp *Checkpoint, options ContextOptions) (*Context, *llm.Message, error) {
	messages := cp.Messages
	var pending *llm.Message
	if n := len(messages); n > 0 && messages[n-1].Role == "assistant" && len(messages[n-1].ToolCalls) > 0 {
		last := messages[n-1]
		pending, messages = &last, messages[:n-1]
	}
	if _, err := llm.Groups(messages); err != nil {
		return nil, nil, fmt.Errorf("检查点中的消息无效：%w", err)
	}
	return rebuildContext(HistoryInput{Messages: messages, WithTools: true}, options), pending, nil
}

func pendingCount(m *llm.Message) int {
	if m == nil {
		return 0
	}
	return len(m.ToolCalls)
}

// Lineage 是从本轮往前的续接链（含本轮），最多回溯 64 轮；链上某一轮的检查点读不到就停在那里。
func Lineage(id string) []string {
	chain := []string{id}
	for len(chain) < 64 {
		cp, err := LoadCheckpoint(chain[len(chain)-1])
		if err != nil || cp.Continues == "" || !ValidID(cp.Continues) {
			break
		}
		chain = append(chain, cp.Continues)
	}
	return chain
}

// ConversationRoot 是本轮所在对话的第一轮检查点ID：沙箱工作目录按它存放，续聊各轮看到同一批文件。
// 新的一轮从上一轮（Continues）往前找；续跑时本轮的检查点已经存在，从它往前找。
// 子 agent、回放分叉没有续接关系，各自就是起点。
func ConversationRoot() string {
	start := CheckpointID
	if Resume == nil && Continues != "" {
		start = Continues
	}
	chain := Lineage(start)
	return chain[len(chain)-1]
}

// Fork 记录回放验证的来源。
type Fork struct {
	From   string `json:"from"`
	Rounds int    `json:"rounds"` // 保留了来源的前几轮（模型决定 + 工具结果）
	Note   string `json:"note"`   // 在保留的轮次之后插入的一条纠正（以用户消息出现）
}

// ForkCheckpoint 是 D17 的回放验证：保留来源检查点的前 rounds 轮，在其后插入一条纠正，存成一个新的检查点（状态 running）。
// 之后用 RunChild 启动它，就是从第 rounds+1 轮接着跑：前面的步骤原样保留，只有这一处不同；
// 结局翻转了，才说明怀疑的那一步是决定性的。新检查点有自己的ID，来源不变。
func ForkCheckpoint(src *Checkpoint, id string, rounds int, note string) (*Checkpoint, error) {
	if !ValidID(id) || id == src.ID {
		return nil, fmt.Errorf("回放的任务ID %q 无效", id)
	}
	if strings.TrimSpace(note) == "" {
		return nil, errors.New("回放需要一条纠正提示")
	}
	if src.Continues != "" {
		return nil, errors.New("只支持单轮任务的检查点（续聊的上下文含有之前各轮）")
	}
	// 按轮截断要求 View 里还是逐条原文；压缩过的上下文已经把前面的轮次换成了摘要。
	keep, seen := 0, 0
	for i, m := range src.Messages {
		if m.Role == "user" && strings.HasPrefix(m.Content, llm.SummaryPrefix) {
			return nil, errors.New("这个检查点的上下文压缩过，无法按轮截断")
		}
		// 下一个模型决定（第 rounds+1 轮的工具调用，或最终回答）就是要被替换掉的那一步，从这里截断。
		if m.Role == "assistant" {
			if seen == rounds {
				break
			}
			if len(m.ToolCalls) > 0 {
				seen++
			}
		}
		keep = i + 1
	}
	messages := slices.Clone(src.Messages[:keep])
	// 第 rounds 轮的工具结果要完整：熔断拦下的那一批只有模型决定、没有结果，不能保留。
	if seen < rounds || messages[len(messages)-1].Role == "assistant" {
		return nil, fmt.Errorf("检查点没有 %d 轮完整的工具调用，不能保留这么多轮", rounds)
	}
	return ForkSnapshot(src, id, messages, rounds, rounds, note)
}

// ForkSnapshot 从一份完整的上下文快照分叉：messages 是第 step+1 次模型决定当时看到的输入（观测台代理记录的那次请求），
// calls 是在它之前已经用掉的模型请求（含压缩），续跑的预算因此和原运行在那一刻相同。
// 检查点里只有最后的 View，压缩过就无法按轮截断；请求快照没有这个问题，而且就是模型当时看到的原文。
func ForkSnapshot(src *Checkpoint, id string, messages []llm.Message, step, calls int, note string) (*Checkpoint, error) {
	if !ValidID(id) || id == src.ID {
		return nil, fmt.Errorf("回放的任务ID %q 无效", id)
	}
	if strings.TrimSpace(note) == "" {
		return nil, errors.New("回放需要一条纠正提示")
	}
	if len(messages) == 0 || messages[len(messages)-1].Role == "assistant" {
		return nil, errors.New("快照必须停在模型决定之前")
	}
	if _, err := llm.Groups(messages); err != nil {
		return nil, fmt.Errorf("截断后的消息无效：%w", err)
	}
	rounds := step
	messages = append(slices.Clone(messages), llm.Message{Role: "user", Content: note})
	// 循环检测按保留的调用与结果重新算一遍，和原来的运行在这一点上一致；私有数据来源沿用原任务（只会更严）。
	// D23 的账本不继承：分叉后的 View 被截断过，账本里可能有截断点之后才读的文件，凭它改文件等于改一个
	// 自己没看过的版本。从空账本开始，代价最多是重新读一次，方向是安全的一侧。
	cp := &Checkpoint{ID: id, Args: src.Args, Question: src.Question, Status: "running", Step: step, Calls: calls, Messages: messages,
		StartedAt: time.Now().UTC(), Fork: &Fork{From: src.ID, Rounds: rounds, Note: note}, Progress: replayProgress(messages), State: State{Private: src.State.Private}}
	for _, line := range src.Summary {
		var n int
		if _, err := fmt.Sscanf(line, "- 第%d轮", &n); err == nil && n <= rounds {
			cp.Summary = append(cp.Summary, line)
		}
	}
	if _, err := os.Stat(CheckpointPath(id)); err == nil {
		return nil, fmt.Errorf("任务ID %s 已有检查点", id)
	}
	return cp, cp.write()
}
