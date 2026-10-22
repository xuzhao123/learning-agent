package chat

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"learning-agent/internal/agent"
	"learning-agent/internal/audit"
	"learning-agent/internal/protocol"
	"learning-agent/internal/tenant"
)

// D34：终端客户端 go run . chat。它和观测台一样只是 app-server 协议的一个界面：每一轮启动一个 agent 子进程（-app-server，stdio），
// 用 turn/start 交问题和上一轮的检查点，读通知显示过程，回答 agent 发来的请求（联网审批、高风险确认、ask_user 的提问）。
// 页面能做的这里都能做，说明协议本身够用；另外加了运行中插话和几条会话命令（见 help）。
//
// 输入只有一个来源（stdin 的行），按当时的状态解释：有待回答的请求就是回答；一轮在跑就是插话（turn/steer）；空闲时是新问题或命令。

const help = `命令：
  /help                      这份说明
  /turns                     列出这条对话的各轮（从第一轮到最新）
  /mode [plan|ask|edit|full] 查看或切换之后各轮的权限模式
  /approve [补充]            批准计划：切到 edit，并让 agent 按计划开始执行
  /compact                   下一条消息发出前先压缩上下文
  /fork <n>                  从第 n 轮之后另起一支（后面各轮留在原处，文件不动）
  /rewind <n> [both|files|chat]  撤销第 n 轮及之后：文件与对话一起退（默认）、只退文件、只退对话
  /resume <检查点ID>          接着某一轮聊（比如观测台或命令行跑过的一轮）
  /new                       开始新对话
  /exit                      退出
运行中：直接输入的话会排进 agent 的下一次模型请求（插话）；Ctrl+C 停止这一轮，再按一次强制结束。
agent 的终端日志写在 `

type session struct {
	args    []string // 传给每一轮 agent 的参数（不含 -permission-mode）
	mode    string   // 权限模式；空表示 agent 的默认值（edit）
	last    string   // 对话最新一轮的检查点ID；空表示新对话
	notes   []string // 下一轮要以提醒告诉模型的情况
	compact bool
	next    []string // 一轮结束后要接着发的话（没用上的插话）
	logPath string
	lines   chan string
	sigs    chan os.Signal
}

// Run 解析 chat 自己的参数（-continue），其余原样传给 agent。
func Run(args []string) error {
	s := &session{lines: make(chan string), sigs: make(chan os.Signal, 2)}
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(strings.TrimLeft(args[i], "-"), "=")
		takes := func() (string, error) {
			if hasValue {
				return value, nil
			}
			if i+1 >= len(args) {
				return "", fmt.Errorf("-%s 需要一个值", name)
			}
			i++
			return args[i], nil
		}
		switch {
		case !strings.HasPrefix(args[i], "-"):
			return fmt.Errorf("chat 不接受位置参数：%s（问题在启动后输入）", args[i])
		case name == "continue":
			v, err := takes()
			if err != nil {
				return err
			}
			s.last = v
		case name == "permission-mode":
			v, err := takes()
			if err != nil {
				return err
			}
			s.mode = v
		case slices.Contains([]string{"question", "app-server", "continues", "history-stdin", "resume", "task-id", "image"}, name):
			return fmt.Errorf("chat 自己管理 -%s，不要传入", name)
		case name == "tenant":
			v, err := takes()
			if err != nil {
				return err
			}
			tenant.Current = v
			s.args = append(s.args, "-tenant", v)
		default:
			// 其余是 agent 的参数：不带 = 时，后面那个不以 - 开头的就是它的值（chat 本身不收位置参数）。
			s.args = append(s.args, args[i])
			if !hasValue && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				s.args = append(s.args, args[i])
			}
		}
	}
	if tenant.Current != "" && !tenant.Valid(tenant.Current) {
		return errors.New("tenant 只能是小写字母、数字和连字符")
	}
	if s.last != "" {
		if err := s.resume(s.last); err != nil {
			return err
		}
	}
	s.logPath = tenant.Here("chat.log")
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		for scanner.Scan() {
			s.lines <- scanner.Text()
		}
		close(s.lines)
	}()
	signal.Notify(s.sigs, os.Interrupt)
	fmt.Println("learning-agent 终端客户端。输入问题开始；/help 查看命令。")
	for {
		if len(s.next) > 0 {
			question := strings.Join(s.next, "\n")
			s.next = nil
			fmt.Printf("› %s\n", question)
			s.turn(question)
			continue
		}
		fmt.Print("› ")
		select {
		case line, ok := <-s.lines:
			if !ok {
				fmt.Println()
				return nil
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "/") {
				if quit := s.command(line); quit {
					return nil
				}
				continue
			}
			s.turn(line)
		case <-s.sigs:
			fmt.Println()
			return nil
		}
	}
}

// ---------- 会话命令 ----------

func (s *session) command(line string) (quit bool) {
	fields := strings.Fields(line)
	switch fields[0] {
	case "/help":
		fmt.Println(help + s.logPath)
	case "/exit", "/quit":
		return true
	case "/new":
		s.last, s.notes, s.compact = "", nil, false
		fmt.Println("开始新对话。")
	case "/mode":
		if len(fields) == 1 {
			fmt.Println("权限模式：" + cmp.Or(s.mode, "edit（默认）"))
			break
		}
		if !slices.Contains([]string{"plan", "ask", "edit", "full"}, fields[1]) {
			fmt.Println("权限模式取 plan、ask、edit 或 full")
			break
		}
		s.mode = fields[1]
		fmt.Println("之后各轮的权限模式：" + s.mode)
	case "/approve":
		if s.last == "" {
			fmt.Println("还没有计划可以批准")
			break
		}
		// 计划模式的批准就是下一轮切到 edit（D27）；这句话是用户按下“批准”的意思，作为这一轮的问题交给模型。
		s.mode = "edit"
		question := "计划已批准，切到 edit 模式，按计划开始执行。"
		if extra := strings.TrimSpace(strings.TrimPrefix(line, "/approve")); extra != "" {
			question += "\n" + extra
		}
		fmt.Println("权限模式：edit")
		s.turn(question)
	case "/compact":
		if s.last == "" {
			fmt.Println("新对话没有可压缩的上下文")
			break
		}
		s.compact = true
		fmt.Println("下一条消息发出前会先压缩上下文。")
	case "/turns":
		chain := s.chain()
		if len(chain) == 0 {
			fmt.Println("还没有轮次")
		}
		for i, cp := range chain {
			fmt.Printf("%2d. %s  %-8s %s\n", i+1, cp.ID[:8], status(cp), clip(cp.Question, 70))
		}
	case "/resume":
		if len(fields) != 2 {
			fmt.Println("用法：/resume <检查点ID>")
			break
		}
		if err := s.resume(fields[1]); err != nil {
			fmt.Println(err)
		}
	case "/fork":
		n, chain, ok := s.pick(fields, 2)
		if !ok {
			break
		}
		// 另起一支：接在第 n 轮后面；之后各轮的检查点不动，仍可以 /resume 回去。文件也不动，所以把它们改过的文件告诉模型。
		if n < len(chain) {
			if res, err := agent.Rewind(chain[n].ID, true, "chat"); err == nil {
				s.noteLeftovers(res, fmt.Sprintf("对话接在第 %d 轮之后另起了一支", n))
			}
		}
		s.last = chain[n-1].ID
		fmt.Printf("接在第 %d 轮之后继续；原来后面的 %d 轮留在原处。\n", n, len(chain)-n)
	case "/rewind":
		n, chain, ok := s.pick(fields, 3)
		if !ok {
			break
		}
		mode := "both"
		if len(fields) == 3 {
			mode = fields[2]
		}
		s.rewind(chain, n, mode)
	default:
		fmt.Println("不认识的命令；/help 查看")
	}
	return false
}

// pick 解析 /fork、/rewind 的轮次编号（1 起）。
func (s *session) pick(fields []string, most int) (int, []*agent.Checkpoint, bool) {
	chain := s.chain()
	if len(fields) < 2 || len(fields) > most {
		fmt.Printf("用法：%s <轮次>（/turns 查看编号）\n", fields[0])
		return 0, nil, false
	}
	n, err := strconv.Atoi(fields[1])
	if err != nil || n < 1 || n > len(chain) {
		fmt.Printf("轮次取 1 到 %d\n", len(chain))
		return 0, nil, false
	}
	return n, chain, true
}

func (s *session) rewind(chain []*agent.Checkpoint, n int, mode string) {
	target := chain[n-1].ID
	preview, err := agent.Rewind(target, true, mode)
	if err != nil {
		fmt.Println(err)
		return
	}
	switch mode {
	case "chat":
		fmt.Printf("只退对话：退到第 %d 轮之前；%d 轮标成已撤销，文件不动。\n", n, len(preview.Turns))
	case "files":
		fmt.Printf("只退文件：恢复 %d 个、删除 %d 个；对话不退。\n", len(preview.Restore), len(preview.Delete))
	default:
		fmt.Printf("撤销第 %d 轮及之后共 %d 轮：恢复 %d 个文件、删除 %d 个，对话退到第 %d 轮之前。\n", n, len(preview.Turns), len(preview.Restore), len(preview.Delete), n)
	}
	if mode != "chat" && len(preview.Conflicts) > 0 {
		fmt.Println("这些文件在最后一次写入后又被改过，整次不做：" + strings.Join(preview.Conflicts, "、"))
		return
	}
	if len(preview.Unsnapshotted) > 0 {
		fmt.Println("bash 改过、没有快照、不会还原：" + strings.Join(preview.Unsnapshotted, "、"))
	}
	fmt.Print("确认？[y/N] ")
	if answer, ok := <-s.lines; !ok || strings.ToLower(strings.TrimSpace(answer)) != "y" {
		fmt.Println("没有撤销。")
		return
	}
	audit.Conversation = chain[0].ID
	res, err := agent.Rewind(target, false, mode)
	if err != nil {
		fmt.Println("撤销失败：", err)
		return
	}
	switch mode {
	case "files":
		if changed := append(slices.Clone(res.Restore), res.Delete...); len(changed) > 0 {
			s.notes = append(s.notes, fmt.Sprintf("用户撤销了第 %d 轮及之后对这些文件的改动，磁盘上已恢复到那一轮之前：%s。对话没有退回：上面记录里对这些文件的修改已经不在磁盘上，改之前先重新读。", n, strings.Join(changed, "、")))
		}
	case "chat":
		s.noteLeftovers(res, fmt.Sprintf("对话退回到了第 %d 轮之前", n))
		s.last = res.Continues
	default:
		s.last = res.Continues
	}
	fmt.Println("已撤销。")
}

// noteLeftovers：对话退了而文件没退时，把那几轮改过的文件告诉模型（下一轮的提醒）。
func (s *session) noteLeftovers(res *agent.RewindResult, what string) {
	changed := slices.Concat(res.Restore, res.Delete, res.Unsnapshotted)
	if len(changed) == 0 {
		return
	}
	s.notes = append(s.notes, fmt.Sprintf("%s，但之后各轮对这些文件的改动仍在磁盘上，没有撤销：%s。改之前先读当前内容。", what, strings.Join(changed, "、")))
}

func (s *session) resume(id string) error {
	if !agent.ValidID(id) {
		return errors.New("检查点ID无效：" + id)
	}
	cp, err := agent.LoadCheckpoint(id)
	if err != nil {
		return err
	}
	if agent.Rewound(id) {
		return errors.New("这一轮已经撤销，不能接着聊")
	}
	s.last = cp.ID
	fmt.Printf("接着这一轮聊（%s）：%s\n", status(cp), clip(cp.Question, 70))
	return nil
}

// chain 从最新一轮沿 continues 往回找，返回从第一轮到最新的检查点。
func (s *session) chain() []*agent.Checkpoint {
	var chain []*agent.Checkpoint
	for id := s.last; id != "" && len(chain) < 500; {
		cp, err := agent.LoadCheckpoint(id)
		if err != nil {
			break
		}
		chain = append(chain, cp)
		id = cp.Continues
	}
	slices.Reverse(chain)
	return chain
}

// ---------- 一轮 ----------

// turn 启动一个 agent 子进程跑这一轮：交问题与上一轮的检查点，显示通知，回答请求，转发插话与停止。
func (s *session) turn(question string) {
	var history *agent.HistoryInput
	if s.last != "" {
		cp, err := agent.LoadCheckpoint(s.last)
		if err != nil {
			fmt.Println("读不到上一轮的检查点：", err)
			return
		}
		history = &agent.HistoryInput{Messages: cp.Messages, WithTools: true}
	}
	args := append([]string{"-app-server", "-ask-user"}, s.args...)
	if s.mode != "" {
		args = append(args, "-permission-mode", s.mode)
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Println(err)
		return
	}
	logFile, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		fmt.Println("打不开日志文件：", err)
		return
	}
	defer logFile.Close()
	fmt.Fprintf(logFile, "\n===== %s %s\n", time.Now().Format(time.RFC3339), clip(question, 80))
	cmd := exec.Command(self, args...)
	fromChild, childOut, _ := os.Pipe()
	childIn, toChild, _ := os.Pipe()
	defer fromChild.Close()
	defer toChild.Close()
	cmd.Stdout, cmd.Stdin, cmd.Stderr = childOut, childIn, logFile
	// 自己的进程组：终端的 Ctrl+C 只到客户端，由客户端经协议停止；客户端死了，Linux 上内核给 agent 发 SIGINT。
	cmd.SysProcAttr = agent.ChildAttr()
	inbox := make(chan protocol.Message, 4096)
	conn := protocol.NewConn(fromChild, toChild, func(m protocol.Message) { inbox <- m })
	runtime.LockOSThread() // Pdeathsig 跟创建子进程的线程走（见 agent.RunChild）
	defer runtime.UnlockOSThread()
	if err := cmd.Start(); err != nil {
		childOut.Close()
		childIn.Close()
		fmt.Println("启动 agent 失败：", err)
		return
	}
	childOut.Close()
	childIn.Close()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	params := map[string]any{"question": question, "continues": s.last, "compact": s.compact, "notes": s.notes}
	if history != nil {
		params["history"] = history
	}
	s.compact, s.notes = false, nil
	go func() {
		if err := conn.Call(context.Background(), "turn/start", params, nil); err != nil {
			inbox <- protocol.Message{Method: "client/error", Params: json.RawMessage(strconv.Quote("turn/start 失败：" + err.Error()))}
		}
	}()
	v := &view{}
	var asks []protocol.Message // agent 发来、等人回答的请求，按到达顺序一个一个问
	stops, started := 0, ""
	finish := func() {
		v.newline()
		if started != "" {
			if _, err := agent.LoadCheckpoint(started); err == nil {
				s.last = started
			}
		}
	}
	for {
		select {
		case m := <-inbox:
			switch {
			case m.IsRequest():
				asks = append(asks, m)
				if len(asks) == 1 {
					v.prompt(m)
				}
			case m.Method == "turn/started":
				var p struct {
					TurnID string `json:"turn_id"`
				}
				json.Unmarshal(m.Params, &p)
				started = p.TurnID
			case m.Method == "turn/completed":
				var p struct{ Queued []string }
				json.Unmarshal(m.Params, &p)
				s.next = append(s.next, p.Queued...)
				v.show(m)
			default:
				v.show(m)
			}
		case line, ok := <-s.lines:
			if !ok {
				syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
				continue
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if len(asks) > 0 {
				if s.answer(conn, asks[0], line) {
					asks = asks[1:]
					if len(asks) > 0 {
						v.prompt(asks[0])
					}
				}
				continue
			}
			go func(text string) {
				if err := conn.Call(context.Background(), "turn/steer", map[string]string{"text": text}, nil); err != nil {
					inbox <- protocol.Message{Method: "client/late", Params: json.RawMessage(strconv.Quote(text))}
				}
			}(line)
		case <-s.sigs:
			stops++
			if stops == 1 {
				v.newline()
				fmt.Println("■ 正在停止：agent 回填结果、写好检查点后退出（再按一次强制结束）")
				go conn.Call(context.Background(), "turn/interrupt", nil, nil)
			} else {
				syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case err := <-exited:
			// 读完退出前写出的最后几条通知（turn/completed 等）。
			select {
			case <-conn.Closed():
			case <-time.After(2 * time.Second):
			}
			for len(inbox) > 0 {
				m := <-inbox
				if m.Method == "turn/completed" {
					var p struct{ Queued []string }
					json.Unmarshal(m.Params, &p)
					s.next = append(s.next, p.Queued...)
				}
				if !m.IsRequest() {
					v.show(m)
				}
			}
			if err != nil && !v.completed {
				fmt.Printf("agent 异常退出（%v），日志见 %s\n", err, s.logPath)
			}
			finish()
			return
		}
	}
}

// answer 把一行输入当作对 asks[0] 的回答；看不懂时再问一遍（返回 false）。人的决定先写审计，写不进去就不回答（D21）。
func (s *session) answer(conn *protocol.Conn, m protocol.Message, line string) bool {
	var p struct {
		CallID    string   `json:"call_id"`
		Domain    string   `json:"domain"`
		Tool      string   `json:"tool"`
		Risk      string   `json:"risk"`
		Arguments string   `json:"arguments"`
		Scope     string   `json:"scope"`
		Task      string   `json:"task_id"`
		Options   []string `json:"options"`
	}
	json.Unmarshal(m.Params, &p)
	switch m.Method {
	case "user/requestInput":
		answer := line
		if n, err := strconv.Atoi(line); err == nil && n >= 1 && n <= len(p.Options) {
			answer = p.Options[n-1]
		}
		conn.Reply(*m.ID, map[string]string{"answer": answer}, nil)
		return true
	case "network/requestApproval", "tool/requestApproval":
		decision := map[string]string{"y": "allow", "n": "deny", "": "deny", "s": "allow_session"}[strings.ToLower(line)]
		if decision == "" || (decision == "allow_session" && p.Scope == "") {
			fmt.Print("回答 y 或 n" + map[bool]string{true: "（s：本对话内都允许）", false: ""}[p.Scope != ""] + "：")
			return false
		}
		record := audit.Record{Actor: "user", Event: "user_decision", Task: p.Task, CallID: p.CallID, Tool: p.Tool, Risk: p.Risk, Decision: decision, Detail: "终端客户端"}
		if m.Method == "network/requestApproval" {
			record = audit.Record{Actor: "user", Event: "user_network_decision", Task: p.Task, Target: p.Domain, Risk: "network", Decision: decision, Detail: "终端客户端"}
		} else {
			record.Args(p.Arguments)
		}
		if err := audit.Write(context.Background(), record); err != nil {
			fmt.Println("审计日志写入失败，没有回答：", err)
			return false
		}
		conn.Reply(*m.ID, map[string]string{"decision": decision}, nil)
		return true
	}
	conn.Reply(*m.ID, nil, &protocol.Error{Code: -32601, Message: "终端客户端不支持 " + m.Method})
	return true
}

// ---------- 显示 ----------

// view 把通知画成终端上的文字：正文与推理边收边打，工具调用一行开始、一行结局。
type view struct {
	field     string // 正在打印的增量：content、reasoning 或空（行首）
	completed bool   // 收到过 turn/completed
}

const dim, reset = "\x1b[2m", "\x1b[0m"

func (v *view) newline() {
	if v.field != "" {
		fmt.Println(reset)
		v.field = ""
	}
}

func (v *view) show(m protocol.Message) {
	var p map[string]any
	json.Unmarshal(m.Params, &p)
	switch m.Method {
	case "item/delta":
		field, _ := p["field"].(string)
		delta, _ := p["delta"].(string)
		if p["purpose"] != "main" {
			return
		}
		if field == "reset" {
			v.newline()
			fmt.Println(dim + "（模型请求中断，重试）" + reset)
			return
		}
		if field != v.field {
			v.newline()
			if field == "reasoning" {
				fmt.Print(dim)
			}
			v.field = field
		}
		fmt.Print(delta)
	case "item/started":
		item, _ := p["item"].(map[string]any)
		data, _ := item["data"].(map[string]any)
		switch item["type"] {
		case "toolCall":
			v.newline()
			fmt.Printf("▸ %s %s\n", data["tool"], clip(fmt.Sprint(data["arguments"]), 100))
		case "compaction":
			v.newline()
			fmt.Println(dim + "（正在压缩上下文…）" + reset)
		}
	case "item/completed":
		item, _ := p["item"].(map[string]any)
		data, _ := item["data"].(map[string]any)
		switch item["type"] {
		case "toolCall":
			o, _ := data["observation"].(map[string]any)
			if o["status"] == "ok" {
				fmt.Printf("  %s✓ %s%s\n", dim, clip(compact(o["result"]), 100), reset)
			} else {
				fmt.Printf("  ✗ %s %s\n", o["status"], clip(fmt.Sprint(o["error"]), 160))
			}
		case "userMessage":
			if data["steer"] == true {
				v.newline()
				fmt.Printf("↳ 插话已送达：%s\n", clip(fmt.Sprint(data["text"]), 100))
			}
		case "compaction":
			if data["error"] != nil {
				fmt.Printf("%s（没有压缩：%s）%s\n", dim, data["error"], reset)
			} else {
				fmt.Printf("%s（上下文已压缩：%s → %s token）%s\n", dim, num(data["before"]), num(data["after"]), reset)
			}
		case "agentMessage":
			v.newline()
		}
	case "subagent/started":
		v.newline()
		fmt.Printf("  ⧉ 子 agent 开始：%s\n", clip(fmt.Sprint(p["task"]), 80))
	case "subagent/completed":
		fmt.Printf("  ⧉ 子 agent 结束：%v\n", p["state"])
	case "turn/completed":
		v.newline()
		v.completed = true
		if outcome := p["outcome"]; outcome != "no_tool_calls" {
			fmt.Printf("■ 这一轮停止：%v %v\n", outcome, p["error"])
		}
		if fc, ok := p["final_check"].(map[string]any); ok {
			fmt.Printf("■ 收尾检查：通过=%v %s\n", fc["passed"], clip(fmt.Sprint(fc["detail"]), 200))
		}
		if u, ok := p["usage"].(map[string]any); ok {
			fmt.Printf("%s模型请求 %s 次 · 输入 %s · 输出 %s · 缓存 %s%s\n", dim, num(p["model_calls"]), num(u["input"]), num(u["output"]), num(u["cached"]), reset)
		}
		if q, ok := p["queued"].([]any); ok && len(q) > 0 {
			fmt.Printf("（%d 条插话没赶上这一轮，接着作为下一条消息发送）\n", len(q))
		}
	case "client/error":
		var text string
		json.Unmarshal(m.Params, &text)
		v.newline()
		fmt.Println(text)
	case "client/late":
		v.newline()
		fmt.Println("（这一轮已经结束，这句话作为下一条消息发送）")
	}
}

// prompt 显示一个等人回答的请求。
func (v *view) prompt(m protocol.Message) {
	var p struct {
		Domain    string   `json:"domain"`
		Reason    string   `json:"reason"`
		Tool      string   `json:"tool"`
		Risk      string   `json:"risk"`
		Arguments string   `json:"arguments"`
		Scope     string   `json:"scope"`
		Question  string   `json:"question"`
		Options   []string `json:"options"`
	}
	json.Unmarshal(m.Params, &p)
	v.newline()
	switch m.Method {
	case "network/requestApproval":
		fmt.Printf("? 沙箱请求访问 %s（%s）。允许？[y/N] ", p.Domain, p.Reason)
	case "tool/requestApproval":
		fmt.Printf("? %s 要确认：%s\n  参数：%s\n  允许？[y/N%s] ", p.Tool, p.Risk, clip(p.Arguments, 300), map[bool]string{true: "/s=本对话内同一范围都允许", false: ""}[p.Scope != ""])
	case "user/requestInput":
		fmt.Printf("? %s\n", p.Question)
		for i, o := range p.Options {
			fmt.Printf("  %d. %s\n", i+1, o)
		}
		if len(p.Options) > 0 {
			fmt.Print("  输入编号，或直接写回答：")
		} else {
			fmt.Print("  回答：")
		}
	default:
		fmt.Printf("? agent 发来不支持的请求 %s，回车拒绝 ", m.Method)
	}
}

// ---------- 小工具 ----------

func status(cp *agent.Checkpoint) string {
	if cp.Status == "stopped" {
		return "stopped:" + cp.Reason
	}
	return cp.Status
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// num 打印 JSON 里的整数：解码成 float64 后 %v 会写成 2.035675e+06。
func num(v any) string {
	if f, ok := v.(float64); ok {
		return strconv.FormatFloat(f, 'f', 0, 64)
	}
	return fmt.Sprint(v)
}

func compact(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}
