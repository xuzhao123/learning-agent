package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"syscall"
	"time"

	"github.com/google/uuid"

	"learning-agent/internal/llm"
)

// D9 检查点：一次运行的全部可恢复状态。每到一个边界就整份原子写回 .data/checkpoints/<id>.json：
// 问题写入后、模型决定调用工具后（写前日志）、这批工具结果写回后、运行结束时。
// 续跑只依赖这个文件和其中保存的启动参数，不依赖进程内存，所以 kill -9 之后也能接着跑。
type Checkpoint struct {
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
	StartedAt  time.Time `json:"started_at,omitempty"` // 首次开始；续跑不变，所以耗时包含中断的那段时间
	Input      int       `json:"input_tokens"`         // 累计用量，跨续跑累加，包含摘要与记忆辅助调用
	Output     int       `json:"output_tokens"`
	Cached     int       `json:"cached_tokens"`
	Traces     []string  `json:"traces,omitempty"` // 每次运行（首次与各次续跑）的 trace_id
	Summary    []string  `json:"summary"`
	LastAction string    `json:"last_action"`
	Repeated   int       `json:"repeated"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// 由 main 设置：CheckpointID 为空表示不写检查点（Day 3/4 实验）；Resume 不为空表示本次是续跑。
var (
	CheckpointID   string
	CheckpointArgs []string
	Continues      string // 本轮接着的上一轮检查点ID，见 Checkpoint.Continues
	Resume         *Checkpoint
)

const checkpointDir = ".data/checkpoints"

var validID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// ID 会拼进文件路径，只允许字母、数字和 ._-，不能含路径分隔符。
func ValidID(id string) bool { return validID.MatchString(id) && id != "." && id != ".." }

// 不给 -task-id 时用随机 UUID（v4）：不含时间，不受时区影响，几个进程同时启动也不会撞号。
// 子任务ID在它后面加 -sub- 和 task 哈希（见 subagent.go），仍是确定性的，续跑时能找回同一个子任务。
func NewCheckpointID() string { return uuid.NewString() }

func CheckpointPath(id string) string { return filepath.Join(checkpointDir, id+".json") }

func LoadCheckpoint(id string) (*Checkpoint, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("任务ID %q 只能包含字母、数字和 ._-，最长128", id)
	}
	data, err := os.ReadFile(CheckpointPath(id))
	if err != nil {
		return nil, err
	}
	cp := &Checkpoint{}
	if err := json.Unmarshal(data, cp); err != nil {
		return nil, fmt.Errorf("%s 不是有效的检查点：%w", CheckpointPath(id), err)
	}
	return cp, nil
}

// 先写临时文件再改名：任何时刻读到的都是一份完整的旧版本或新版本，不会是写了一半的文件。
// 没有 fsync：进程崩溃（包括 kill -9）不丢已写的数据，整机断电可能丢最后一次写入。
func (cp *Checkpoint) write() error {
	cp.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(checkpointDir, 0o700); err != nil {
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
	if err := os.MkdirAll(checkpointDir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(checkpointDir, id+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
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
