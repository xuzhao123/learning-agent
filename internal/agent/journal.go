package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"learning-agent/internal/protocol"
	"learning-agent/internal/tenant"
)

// D40（O9 的第一步）：只追加的会话记录。agent 发出的每条通知与请求（界面看到的那些：Turn、Item、审批、子 agent……，
// 不含 item/delta、终端日志行与响应）按发出的顺序追加到 .data/[tenants/<租户>/]sessions/<对话第一轮的检查点ID>.jsonl，
// 一行一条：对话内的序号（各轮接着编）、所属的轮次（检查点ID）、协议序号（可以脱离的连接上才有）、方法与参数。
// 检查点记下写到了第几条（log_seq）：检查点是“记录到第 N 条时的状态快照”。命令行、队列、评测里没有界面也照写。
// 用处：观测台重启时 agent 已经结束、内存里留着补发的消息也没了，就从这里补齐（见 observer/live.go）；
// 人也可以直接读它，看一段对话从头到尾发生了什么。子 agent 的消息经 subagent/message 包在父对话的记录里，不另写。

type JournalEntry struct {
	Seq    int64           `json:"seq"`
	Turn   string          `json:"turn"`
	ASeq   int64           `json:"aseq,omitempty"`
	Time   time.Time       `json:"time"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

var journal struct {
	sync.Mutex
	file *os.File
	seq  int64
}

// JournalPath 是一段对话的会话记录。
func JournalPath(space, root string) string { return tenant.Path(space, "sessions", root+".jsonl") }

// OpenJournal 打开本对话的会话记录（在发出 turn/started 之前调用）。子 agent 与不写检查点的实验不写。
// 同一段对话被两个进程同时接着聊（命令行上分叉）时序号可能重复：按时间与轮次区分，不加锁。
func OpenJournal() {
	if CheckpointID == "" || strings.Contains(CheckpointID, "-sub-") {
		return
	}
	path := JournalPath(tenant.Current, ConversationRoot())
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		fmt.Printf("Journal error: %v\n", err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Printf("Journal error: %v\n", err)
		return
	}
	journal.file, journal.seq = f, lastJournalSeq(f)
	protocol.Journal = appendJournal
	fmt.Printf("Journal: file=%s from_seq=%d\n", path, journal.seq+1)
}

func appendJournal(m protocol.Message) {
	journal.Lock()
	defer journal.Unlock()
	journal.seq++
	line, err := json.Marshal(JournalEntry{Seq: journal.seq, Turn: CheckpointID, ASeq: m.Seq, Time: time.Now().UTC(), Method: m.Method, Params: m.Params})
	if err == nil {
		journal.file.Write(append(line, '\n'))
	}
}

// journalSeq 是已经写到第几条，存进检查点。
func journalSeq() int64 {
	journal.Lock()
	defer journal.Unlock()
	return journal.seq
}

// lastJournalSeq 读最后一条完整记录的序号：从文件末尾往前读，单条记录再长也能找到。
func lastJournalSeq(f *os.File) int64 {
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return 0
	}
	for size := int64(64 << 10); ; size *= 4 {
		start := max(0, info.Size()-size)
		buf := make([]byte, info.Size()-start)
		if _, err := f.ReadAt(buf, start); err != nil && err != io.EOF {
			return 0
		}
		lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
		if len(lines) > 1 || start == 0 {
			var e JournalEntry
			if json.Unmarshal([]byte(lines[len(lines)-1]), &e) == nil {
				return e.Seq
			}
			return 0
		}
	}
}

// ReadJournal 读一段对话的会话记录。
func ReadJournal(space, root string) ([]JournalEntry, error) {
	data, err := os.ReadFile(JournalPath(space, root))
	if err != nil {
		return nil, err
	}
	var out []JournalEntry
	for _, line := range strings.Split(string(data), "\n") {
		var e JournalEntry
		if json.Unmarshal([]byte(line), &e) == nil && e.Seq > 0 {
			out = append(out, e)
		}
	}
	return out, nil
}
