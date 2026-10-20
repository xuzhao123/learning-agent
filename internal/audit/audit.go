package audit

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"learning-agent/internal/tenant"

	"go.opentelemetry.io/otel/trace"
)

// D21 审计日志：回答“谁、在什么时候、对什么做了什么决定、结果如何”。
// 与终端日志、trace 的区别：只记关键动作与决定；只追加；每条带上一条的哈希，改动或删除中间任何一条，校验都会失败。
//
// 写入方有两个：agent 进程记录程序规则的判断、执行结果与记忆变更；观测台记录人点了什么。
// 两边写同一个文件，用文件锁保证一次只追加一条，链不会断。参数只存哈希和长度，不存原文，审计本身不成为泄露源；
// 要核对“批准的是不是这次执行的参数”，用检查点或观测台存档里的原文重新算哈希比对。
//
// 多租户：每个租户一条自己的链（.data/tenants/<租户>/audit/audit.jsonl），平台一条（.data/audit/audit.jsonl）。
// 记录写进哪条链由 Record.Tenant 决定：agent 进程默认是自己的租户，观测台按对话所属的租户填。
func path(space string) string { return tenant.Path(space, "audit", "audit.jsonl") }

// 由 main 设置：本进程的任务ID 与对话ID；观测台写入时在记录里自己填。
// User 是发起本次运行的登录用户（观测台经环境变量 AGENT_USER 传入），命令行运行时为空。
var Task, Conversation, User = "", "", os.Getenv("AGENT_USER")

type Record struct {
	Seq          int64  `json:"seq"`
	Time         string `json:"time"`
	Actor        string `json:"actor"`          // user：人；model：模型提议；program：程序规则或模块自己做的
	User         string `json:"user,omitempty"` // 登录用户名：观测台里的操作才有
	Tenant       string `json:"tenant,omitempty"`
	Event        string `json:"event"`
	Task         string `json:"task,omitempty"`
	Conversation string `json:"conversation,omitempty"`
	Trace        string `json:"trace,omitempty"`
	CallID       string `json:"call_id,omitempty"`
	Tool         string `json:"tool,omitempty"`
	Target       string `json:"target,omitempty"` // 不敏感的对象标识：域名、记忆编号
	Risk         string `json:"risk,omitempty"`
	Decision     string `json:"decision,omitempty"` // allow、allow_session（本对话内同一范围都允许）、deny、unavailable、timeout
	Status       string `json:"status,omitempty"`   // 执行结局：ok、error、unknown、denied…
	ArgsHash     string `json:"args_sha256,omitempty"`
	ArgsBytes    int    `json:"args_bytes,omitempty"`
	Detail       string `json:"detail,omitempty"` // 程序生成的简短说明，不含工具输出与记忆原文
	Prev         string `json:"prev"`             // 上一条的 hash；第一条为空
	Hash         string `json:"hash"`
}

// Args 填入参数的哈希与长度。
func (r *Record) Args(arguments string) {
	sum := sha256.Sum256([]byte(arguments))
	r.ArgsHash, r.ArgsBytes = hex.EncodeToString(sum[:]), len(arguments)
}

// hash 覆盖除 hash 本身以外的全部字段（含 prev 与 seq），所以改内容、删一条、调换顺序都会让后面的链对不上。
func (r Record) hash() string {
	r.Hash = ""
	data, _ := json.Marshal(r)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Write 追加一条记录。ctx 里有 span 时记下 trace_id，审计记录能对回调用链。
// 返回错误时调用方自己决定：高风险动作写不进审计就不执行（D20 的确认流程），其他记录打印后继续。
func Write(ctx context.Context, r Record) error {
	if r.Task == "" {
		r.Task = Task
	}
	if r.Tenant == "" {
		r.Tenant = tenant.Current
	}
	if r.User == "" && r.Actor == "user" {
		r.User = User
	}
	if r.Tenant != "" && !tenant.Valid(r.Tenant) {
		return errors.New("审计记录的租户ID无效")
	}
	if r.Conversation == "" {
		r.Conversation = Conversation
	}
	if sc := trace.SpanContextFromContext(ctx); sc.HasTraceID() && r.Trace == "" {
		r.Trace = sc.TraceID().String()
	}
	if err := os.MkdirAll(filepath.Dir(path(r.Tenant)), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path(r.Tenant), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	// 观测台和多个 agent 进程会同时写：加锁 → 读最后一条 → 接上链 → 写入并落盘 → 解锁。
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	last, err := lastRecord(file)
	if err != nil {
		return err
	}
	r.Seq, r.Prev = last.Seq+1, last.Hash
	r.Time = time.Now().UTC().Format(time.RFC3339Nano)
	r.Hash = r.hash()
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	// 审计记录写完就落盘：断电也不丢已经承认发生过的决定。检查点没有这样做，它丢了可以从头再跑。
	return file.Sync()
}

// Log 写入失败时只打印：用于不决定能否执行的记录（执行结果、记忆变更）。
func Log(ctx context.Context, r Record) {
	if err := Write(ctx, r); err != nil {
		fmt.Printf("Audit error: event=%s error=%v\n", r.Event, err)
	}
}

// 只读文件末尾一段找最后一条完整记录；单条记录很短，64KB 足够。
func lastRecord(file *os.File) (Record, error) {
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return Record{}, err
	}
	size := min(info.Size(), 64<<10)
	buf := make([]byte, size)
	if _, err := file.ReadAt(buf, info.Size()-size); err != nil && err != io.EOF {
		return Record{}, err
	}
	lines := bytes.Split(bytes.TrimRight(buf, "\n"), []byte("\n"))
	var r Record
	if err := json.Unmarshal(lines[len(lines)-1], &r); err != nil || r.Hash == "" {
		// 最后一行坏了（例如写到一半被截断）：不在坏记录后面继续接链，交给人检查。
		return Record{}, errors.New(file.Name() + " 最后一条记录损坏，请先运行 go run . audit -verify 检查")
	}
	return r, nil
}

// Result 是一次校验的结论。Broken 为 0 表示整条链完整。
type Result struct {
	Records []Record `json:"records"`
	Total   int      `json:"total"`
	Broken  int64    `json:"broken_seq,omitempty"`
	Reason  string   `json:"reason,omitempty"`
}

// Read 读出一个空间的全部记录并逐条校验；task 不为空时只返回这个任务（及其子任务）的记录，但校验始终覆盖整条链。
func Read(space, task string) (Result, error) {
	var result Result
	file, err := os.Open(path(space))
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	defer file.Close()
	// 共享锁：读的时候不会撞上一条写到一半的记录。
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH); err != nil {
		return result, err
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	prev, seq := "", int64(0)
	for scanner.Scan() {
		var r Record
		seq++
		fail := func(reason string) {
			if result.Broken == 0 {
				result.Broken, result.Reason = seq, reason
			}
		}
		switch {
		case json.Unmarshal(scanner.Bytes(), &r) != nil:
			fail("不是有效 JSON")
		case r.Seq != seq:
			fail(fmt.Sprintf("序号应为 %d，实际 %d（中间有记录被删除或插入）", seq, r.Seq))
		case r.Prev != prev:
			fail("prev 与上一条的 hash 不一致（上一条被修改或删除）")
		case r.hash() != r.Hash:
			fail("hash 与内容不一致（这一条被修改）")
		}
		prev = r.Hash
		result.Total++
		if task == "" || r.Task == task || strings.HasPrefix(r.Task, task+"-sub-") {
			result.Records = append(result.Records, r)
		}
	}
	return result, scanner.Err()
}

// Run 是 go run . audit：列出记录并校验哈希链。只读 .data/audit，不请求模型。
func Run(args []string) error {
	flags := flag.NewFlagSet("audit", flag.ContinueOnError)
	task := flags.String("task", "", "只列出这个任务（及其子任务）的记录；校验仍覆盖整条链")
	space := flags.String("tenant", "", "读哪个租户的链；不给时是平台的链")
	verify := flags.Bool("verify", false, "只校验，不列出记录")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *space != "" && !tenant.Valid(*space) {
		return errors.New("tenant 只能是小写字母、数字和连字符")
	}
	result, err := Read(*space, *task)
	if err != nil {
		return err
	}
	if !*verify {
		for _, r := range result.Records {
			line := fmt.Sprintf("#%d %s %-7s %-18s", r.Seq, r.Time[:19], r.Actor, r.Event)
			for _, kv := range [][2]string{{"user", r.User}, {"task", r.Task}, {"tool", r.Tool}, {"target", r.Target}, {"risk", r.Risk}, {"decision", r.Decision}, {"status", r.Status}, {"detail", r.Detail}} {
				if kv[1] != "" {
					line += " " + kv[0] + "=" + kv[1]
				}
			}
			fmt.Println(line)
		}
	}
	if result.Broken != 0 {
		return fmt.Errorf("审计链校验失败：第 %d 条，%s（共 %d 条）", result.Broken, result.Reason, result.Total)
	}
	fmt.Printf("审计链完整：共 %d 条记录，文件 %s\n", result.Total, path(*space))
	return nil
}
