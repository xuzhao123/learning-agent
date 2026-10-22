package observer

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"learning-agent/internal/agent"
	"learning-agent/internal/protocol"
	"learning-agent/internal/tenant"
)

// B0 接管：观测台启动的 agent 在 .data/[tenants/<租户>/]live/<对话ID>.sock 上等观测台连进来（见 protocol/session.go），
// 观测台退出时它照常运行。同目录的 <对话ID>.json 记着进程号和代理令牌（0600，不提交），
// 观测台重启后据此重新连上、补齐没收到的消息，并让这个 agent 的模型请求继续经过代理；
// <对话ID>.err 接 agent 的文件描述符 2，只有 Go 运行时崩溃这类没经过协议的输出会写进去，结束时并进存档。
// 这一轮结束时三个文件都删掉。

type liveInfo struct {
	PID   int    `json:"pid"`
	Token string `json:"token"`
}

// livePath 是相对项目根目录的路径：agent 与观测台都在项目根目录运行，用相对路径也免得超出 Unix socket 路径 108 字节的上限。
func livePath(r *run, ext string) string { return tenant.Path(r.Tenant, "live", r.ID+ext) }

func (s *server) abs(path string) string { return filepath.Join(s.agentDir, path) }

// connect 连上 agent 的 socket 并说明已经收到第几条；agent 刚启动时 socket 可能还没建好（-dev 模式要先编译），连到为止，进程先退出了就放弃。
// note 不为空时，连上之后、补发之前先记一行，存档里它排在补齐的消息前面。
func (s *server) connect(r *run, token string, after int64, exited <-chan struct{}, note string) *protocol.Conn {
	r.mu.Lock()
	r.after = after
	r.mu.Unlock()
	for {
		conn, err := protocol.Dial(livePath(r, ".sock"), r.handle)
		if err == nil {
			r.mu.Lock()
			r.conn = conn
			r.mu.Unlock()
			if note != "" {
				r.add(event{Kind: "stdout", Text: note})
				note = ""
			}
			if conn.Attach(token, after) == nil {
				return conn
			}
			conn.Close()
			r.mu.Lock()
			r.conn = nil
			r.mu.Unlock()
		}
		select {
		case <-exited:
			return nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// adopt 在观测台启动时接管上次还在运行的 agent：live 文件在、存档里这一轮还没有 exit，就重新连上它。
// 进程已经不在或者连不上时，等它结束后记下 exit，存档里不会留下一轮没有结局的对话。
func (s *server) adopt(r *run) {
	data, err := os.ReadFile(s.abs(livePath(r, ".json")))
	if err != nil {
		return
	}
	var info liveInfo
	if json.Unmarshal(data, &info) != nil || info.PID <= 0 {
		s.cleanLive(r)
		return
	}
	// 这一轮从最后一个 start/continue 开始：已经存下的最后一条协议消息、父 agent 的任务ID。
	var after int64
	root, last := "", -1
	for i, e := range r.events {
		if e.Kind == "start" || e.Kind == "continue" {
			last = i
		}
	}
	if last < 0 || r.events[len(r.events)-1].Kind == "exit" {
		s.cleanLive(r)
		return
	}
	open := map[int]event{} // 观测台退出时还在转发的请求：响应没有记下
	root, turn := turnIDs(r.events, last)
	for _, e := range r.events[last:] {
		after = max(after, e.ASeq)
		if e.Kind == "request" && !e.Sub && root == "" {
			root = e.Task
		}
		if e.Kind == "request" {
			open[e.Call] = e
		} else if e.Kind == "response" {
			delete(open, e.Call)
		}
	}
	f, err := os.OpenFile(filepath.Join(s.runsDir(r.Tenant), r.ID+".jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	proc, _ := os.FindProcess(info.PID)
	r.file, r.Done, r.token, r.rootTask, r.proc = f, false, info.Token, root, proc
	r.approvals, r.toolWaits = map[string][]int64{}, map[string]toolWait{}
	// 给它们补一条失败的响应，页面不会一直显示“思考中”；agent 那边流断了会重试，重试是新的一次请求。
	for _, call := range slices.Sorted(maps.Keys(open)) {
		e := open[call]
		r.add(event{Kind: "response", Call: call, Status: http.StatusBadGateway, Text: "观测台在转发这次请求时退出，响应没有记下；agent 会重试", Purpose: e.Purpose, Task: e.Task, Sub: e.Sub})
	}
	gone := make(chan struct{})
	go func() {
		for syscall.Kill(info.PID, 0) == nil {
			time.Sleep(500 * time.Millisecond)
		}
		close(gone)
	}()
	conn := s.connect(r, info.Token, after, closedAfter(gone, 2*time.Second),
		fmt.Sprintf("观测台重启：重新接管正在运行的 agent（进程 %d），补齐第 %d 条之后的协议消息", info.PID, after))
	go func() {
		if conn != nil {
			<-conn.Closed()
		}
		<-gone
		if conn == nil {
			s.recoverJournal(r, root, turn, after)
		}
		r.mu.Lock()
		text := r.exitStatus
		r.mu.Unlock()
		if text == "" {
			text = "agent 在观测台停止期间结束，没有收到退出状态；这一轮的结局以检查点为准"
		}
		s.finish(r, text)
	}()
}

// turnIDs 从存档里找出这段对话第一轮的检查点ID（会话记录按它命名）和这一轮的检查点ID，都来自 turn/started。
func turnIDs(events []event, last int) (root, turn string) {
	for i, e := range events {
		if e.Kind != "notify" || e.Method != "turn/started" {
			continue
		}
		var p struct {
			TurnID string `json:"turn_id"`
		}
		json.Unmarshal(e.Body, &p)
		if root == "" {
			root = p.TurnID
		}
		if i >= last {
			turn = p.TurnID
		}
	}
	return root, turn
}

// recoverJournal：agent 在观测台停止期间就结束了，它内存里留着补发的消息也没了；从它的会话记录（D40）里补齐这一轮
// 存档之后的部分。补进来的都记成通知（这时已经没人能回答审批了）；退出状态从 process/exit 取。终端日志行不在会话记录里。
func (s *server) recoverJournal(r *run, root, turn string, after int64) {
	if root == "" || turn == "" {
		return
	}
	entries, err := agent.ReadJournal(r.Tenant, root)
	if err != nil {
		return
	}
	var missing []agent.JournalEntry
	for _, e := range entries {
		if e.Turn == turn && e.ASeq > after {
			missing = append(missing, e)
		}
	}
	if len(missing) == 0 {
		return
	}
	r.add(event{Kind: "stdout", Text: fmt.Sprintf("观测台重启：agent 已经结束，从会话记录 %s 补齐 %d 条协议消息", agent.JournalPath(r.Tenant, root), len(missing))})
	for _, e := range missing {
		if e.Method == "process/exit" {
			var p struct{ Status string }
			json.Unmarshal(e.Params, &p)
			r.mu.Lock()
			r.exitStatus = p.Status
			r.mu.Unlock()
			continue
		}
		r.add(event{Kind: "notify", Method: e.Method, Body: e.Params, ASeq: e.ASeq})
	}
}

// closedAfter：done 关闭或过了 d 都算放弃，用来给接管时的连接重试设上限。
func closedAfter(done <-chan struct{}, d time.Duration) <-chan struct{} {
	out := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-time.After(d):
		}
		close(out)
	}()
	return out
}

// finish 在 agent 进程结束后收尾：并入没经过协议的输出，删掉 live 文件，记 exit。
func (s *server) finish(r *run, text string) {
	r.mu.Lock()
	r.proc, r.conn, r.approvals, r.toolWaits, r.asks = nil, nil, nil, nil, nil
	r.mu.Unlock()
	if data, err := os.ReadFile(s.abs(livePath(r, ".err"))); err == nil && len(bytes.TrimSpace(data)) > 0 {
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 64*1024), 4<<20)
		for scanner.Scan() {
			r.add(event{Kind: "stdout", Text: scanner.Text()})
		}
	}
	s.cleanLive(r)
	r.add(event{Kind: "exit", Text: text})
}

func (s *server) cleanLive(r *run) {
	for _, ext := range []string{".sock", ".json", ".err"} {
		os.Remove(s.abs(livePath(r, ext)))
	}
}
