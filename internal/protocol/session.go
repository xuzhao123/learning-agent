package protocol

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strings"
	"time"
)

// B0 接管：可以脱离的连接。观测台启动的 agent 不用 stdio，而是在 AGENT_APP_SOCKET 指定的 Unix socket 上等界面连进来。
// 观测台退出或重启时连接断开，agent 照常运行：发出的通知与请求（item/delta 除外）按顺序编号留在内存里，
// 界面重新连上时先发 session/attach，说出已经收到第几条（after）；agent 补发之后的各条，
// 以及还在等答复的请求（哪怕界面收到过：新的界面要重新登记它们）。
// 脱离超过 DetachGrace 仍没有界面连回，Closed 关闭，按界面断开处理：中断、写检查点、退出。
// 一个进程只跑一轮，留下的消息随这一轮结束而释放；每次连回时丢掉界面已经收到的部分。

const DetachGrace = 10 * time.Minute

type session struct {
	token    string             // 连回时要出示的令牌（与模型代理的令牌相同）
	seq      int64              // 已经发出的最后一条的编号
	sent     []Message          // 界面可能还没收到的消息
	asked    map[int64]struct{} // 发出了、还没收到答复的请求
	cur      net.Conn           // 当前连着的界面；nil 表示脱离中
	detached time.Time          // 从什么时候开始没有界面
	logs     *os.File           // 日志管道的写端（见 CaptureLogs）
	logsDone chan struct{}
}

// keep 给要留档的消息编号并留下（调用方持有 c.mu）。
func (s *session) keep(m *Message) {
	if (m.ID != nil && m.Method == "") || IsDelta(*m) {
		return
	}
	s.seq++
	m.Seq = s.seq
	s.sent = append(s.sent, *m)
	if m.IsRequest() {
		s.asked[*m.ID] = struct{}{}
	}
}

// Listen 在 path 上等界面连进来，返回 agent 一侧的连接。之后每个连进来的界面都要先发 session/attach。
func Listen(path, token string, handle func(Message)) (*Conn, error) {
	os.Remove(path) // 上一次运行留下的 socket 文件
	// 谁能连进来由所在目录决定：观测台把它建在 0700 的 .data/…/live/ 下（有的文件系统不支持对 socket 文件 chmod）。
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	c := &Conn{pending: map[int64]chan Message{}, closed: make(chan struct{}), handle: handle,
		session: &session{token: token, asked: map[int64]struct{}{}, detached: time.Now()}}
	go func() {
		for {
			nc, err := l.Accept()
			if err != nil {
				return
			}
			go c.serve(nc)
		}
	}()
	// 脱离太久就放弃：关闭 Closed，让 agent 按界面断开收尾。
	go func() {
		for range time.Tick(time.Second) {
			c.mu.Lock()
			gone := c.cur == nil && time.Since(c.detached) > DetachGrace
			c.mu.Unlock()
			if gone {
				l.Close()
				os.Remove(path)
				close(c.closed)
				return
			}
		}
	}()
	return c, nil
}

// serve 处理一个连进来的界面：核对 session/attach，补发，然后照常读它发来的消息，直到它断开。
func (c *Conn) serve(nc net.Conn) {
	attached := false
	c.read(nc, func(m Message) bool {
		var params struct {
			After int64  `json:"after"`
			Token string `json:"token"`
		}
		if m.Method != "session/attach" || m.ID == nil || json.Unmarshal(m.Params, &params) != nil ||
			subtle.ConstantTimeCompare([]byte(params.Token), []byte(c.token)) != 1 {
			return false
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.cur != nil {
			c.cur.Close() // 新的界面接管，旧连接作废
		}
		c.cur, c.w, attached = nc, nc, true
		// 界面已经收到的不再留着；还在等答复的请求留着，每次连回都重发。
		c.sent = slices.DeleteFunc(c.sent, func(m Message) bool {
			_, waiting := c.asked[ptr(m.ID)]
			return m.Seq <= params.After && !(m.IsRequest() && waiting)
		})
		out := bufio.NewWriter(nc)
		reply, _ := json.Marshal(Message{JSONRPC: "2.0", ID: m.ID, Result: json.RawMessage(`{}`)})
		out.Write(append(reply, '\n'))
		for _, m := range c.sent {
			data, _ := json.Marshal(m)
			out.Write(append(data, '\n'))
		}
		out.Flush()
		return true
	})
	nc.Close()
	if attached {
		c.mu.Lock()
		if c.cur == nc {
			c.cur, c.w, c.detached = nil, nil, time.Now()
		}
		c.mu.Unlock()
	}
}

func ptr(id *int64) int64 {
	if id == nil {
		return 0
	}
	return *id
}

// Dial 是界面一侧：连上 agent 的 socket。调用方先把返回的连接记好（handle 里要用它答复请求），再调 Attach。
func Dial(path string, handle func(Message)) (*Conn, error) {
	nc, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	c := NewConn(nc, nc, handle)
	go func() { <-c.closed; nc.Close() }()
	return c, nil
}

// Attach 出示令牌，说明已经收到第几条；agent 答复后补发之后的各条与还在等答复的请求，都交给 handle。
func (c *Conn) Attach(token string, after int64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.Call(ctx, "session/attach", map[string]any{"after": after, "token": token}, nil); err != nil {
		return fmt.Errorf("session/attach：%w", err)
	}
	return nil
}

// Close 断开界面一侧的连接（Dial 得到的）。
func (c *Conn) Close() {
	if closer, ok := c.w.(io.Closer); ok {
		closer.Close()
	}
}

// CaptureLogs：可以脱离的连接上，给人看的终端日志也走协议（log 通知，一行一条），界面重启后照样能补齐。
// 只换掉 os.Stdout、os.Stderr 两个变量；文件描述符 2 仍是启动时给的文件，Go 运行时崩溃的输出写在那里。
func CaptureLogs() {
	r, w, err := os.Pipe()
	if err != nil {
		return
	}
	os.Stdout, os.Stderr = w, w
	Client.logs, Client.logsDone = w, make(chan struct{})
	// 一直读到写端关闭：读的一方停下，管道写满后整个 agent 的打印都会卡住，所以再长的行也照收。
	go func() {
		defer close(Client.logsDone)
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadString('\n')
			if line != "" {
				Notify("log", map[string]any{"text": strings.TrimSuffix(line, "\n")})
			}
			if err != nil {
				return
			}
		}
	}()
}

// Finish 在进程退出前调用：用 process/exit 请求报告退出状态（观测台重启后接管的进程，它拿不到退出码）。
// 界面按顺序处理消息，它答复了，说明之前的各条也都记下了，这时才断开退出；界面在答复前断开，
// 这个请求留着，连回时随补发重发；最多等到 DetachGrace。stdio 连接什么都不做。
func Finish(status string) {
	c := Client
	if c == nil || c.session == nil {
		return
	}
	if c.logs != nil {
		c.logs.Close()
		<-c.logsDone
	}
	if c.Call(context.Background(), "process/exit", map[string]any{"status": status}, nil) == nil {
		c.mu.Lock()
		if c.cur != nil {
			c.cur.Close()
		}
		c.mu.Unlock()
	}
}
