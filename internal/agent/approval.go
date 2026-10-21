package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"learning-agent/internal/audit"
	"learning-agent/internal/llm"
	"learning-agent/internal/mcp"
	"learning-agent/internal/protocol"
	"learning-agent/internal/sandbox"
	"learning-agent/internal/tenant"
)

// D20：高风险操作的人工确认。执行器在执行前按程序规则判断风险，命中就经协议向界面上的人发 tool/requestApproval，
// 在这里暂停等待；只有人点了“允许”才执行。规则写在程序里，模型、工具结果和网页文字都改不了它。
// 没有界面（命令行、队列、评测）时没有人能确认，按拒绝处理：宁可少做一步，也不替用户做不可撤回或外发的决定。
//
//	irreversible  删除长期记忆：删了就没有了
//	destructive   MCP server 声明工具会做破坏性修改（注解只用来收紧，不用来放宽：server 自称只读也不免检）
//	outbound      本任务已读入私有数据后，再调用会把参数发到外部的工具（浏览器、MCP server）
//	redo          续跑时上一个进程中断了一项有副作用的调用、又没有它的完成记录，重做可能重复副作用
var PrivateInput bool // -private-input：父 agent 在已含私有数据时派出的子任务，从一开始就算含有私有数据

// 本任务已经读入的私有数据来源（image、bash、memory、subagent、parent）。主循环在两批工具之间更新，批内只读：
// 同一批里模型给出参数时还没看到这批的结果，所以按批开始时的状态判断即可。
var privateSources []string

func markPrivate(source string) {
	if !slices.Contains(privateSources, source) {
		privateSources = append(privateSources, source)
		fmt.Printf("Private data: source=%s（之后外发类工具需用户确认）\n", source)
	}
}

// 本对话里用户点过“本对话内都允许”的外发范围。续跑取检查点，续聊取上一轮；子 agent 和回放分叉不继承，照样要问。
// 同一批工具并行等确认，所以要加锁。
var (
	grantMu sync.Mutex
	granted []string
)

// grantScope 是外发类调用可以整段对话放行的范围：搜索按工具（搜索词都发给同一个服务），读网页按工具和主机，
// MCP 工具按工具。空串表示给不出范围（网址解析不了），只能单次允许。
func grantScope(call llm.ToolCall) string {
	name := call.Function.Name
	if name != "open_page" && name != "ds_fetch" {
		return name
	}
	var args struct {
		URL string `json:"url"`
	}
	json.Unmarshal([]byte(call.Function.Arguments), &args)
	u, err := url.Parse(strings.TrimSpace(args.URL))
	if err != nil || u.Hostname() == "" {
		return ""
	}
	return name + " · " + strings.ToLower(u.Hostname())
}

// 外发类工具：调用参数会离开本机（请求 URL 与搜索词发给网站；MCP 参数发给 server 进程或远程服务）。
func outbound(name string) bool {
	return name == "web_search" || name == "open_page" || name == "ds_search" || name == "ds_fetch" || strings.HasPrefix(name, "mcp_")
}

// risk 返回这次调用需要确认的原因；空串表示不需要确认。
func risk(call llm.ToolCall) (kind, reason string) {
	name := call.Function.Name
	switch {
	case name == "forget_memory":
		return "irreversible", "删除一条长期记忆，删除后无法恢复"
	case strings.HasPrefix(name, "mcp_") && mcp.Destructive(name):
		return "destructive", "MCP server 声明这个工具会做破坏性修改"
	case outbound(name) && len(privateSources) > 0:
		return "outbound", "本任务已读入私有数据（" + strings.Join(privateSources, "、") + "），这次调用会把参数发到外部"
	}
	return "", ""
}

// approve 向界面上的人请求确认，返回 allow、deny、unavailable、timeout 或 audit_error。
// D21：请求与答复都写审计。请求写不进审计就不发出、不执行：没有记录的高风险动作事后无法追责。
// 外发类调用的范围本对话已经被允许过，就不再问人，但同样先写审计（写不进去就不执行）。
func approve(ctx context.Context, call llm.ToolCall, kind, reason string) string {
	scope := ""
	if kind == "outbound" {
		scope = grantScope(call)
	}
	grantMu.Lock()
	known := scope != "" && slices.Contains(granted, scope)
	grantMu.Unlock()
	if known {
		record := audit.Record{Actor: "program", Event: "approval_decision", CallID: call.ID, Tool: call.Function.Name, Risk: kind, Decision: "allow_session", Detail: "本对话已允许：" + scope}
		record.Args(call.Function.Arguments)
		if err := audit.Write(ctx, record); err != nil {
			fmt.Printf("Audit error [%s]: %v\n", call.ID, err)
			return "audit_error"
		}
		fmt.Printf("Approval skipped [%s]: tool=%s scope=%s（本对话已允许）\n", call.ID, call.Function.Name, scope)
		return "allow"
	}
	fmt.Printf("Approval request [%s]: tool=%s risk=%s reason=%s\n", call.ID, call.Function.Name, kind, reason)
	record := audit.Record{Actor: "program", Event: "approval_request", CallID: call.ID, Tool: call.Function.Name, Risk: kind, Detail: reason}
	record.Args(call.Function.Arguments)
	if err := audit.Write(ctx, record); err != nil {
		fmt.Printf("Audit error [%s]: %v\n", call.ID, err)
		return "audit_error"
	}
	waitCtx, cancel := context.WithTimeout(ctx, sandbox.ApprovalWait)
	defer cancel()
	var answer struct {
		Decision string `json:"decision"`
	}
	params := map[string]string{"call_id": call.ID, "tool": call.Function.Name, "arguments": call.Function.Arguments, "risk": kind, "reason": reason}
	if scope != "" {
		params["scope"] = scope // 界面据此多给一个“本对话内都允许”
	}
	err := protocol.Call(waitCtx, "tool/requestApproval", params, &answer)
	decision := answer.Decision
	switch {
	case errors.Is(err, protocol.ErrNoClient):
		decision = "unavailable"
	case err != nil && waitCtx.Err() != nil && ctx.Err() == nil:
		decision = "timeout"
	case err != nil:
		decision = "unavailable"
	case decision == "allow_session" && scope == "":
		decision = "allow" // 没给范围的请求只能单次允许
	}
	fmt.Printf("Approval decision [%s]: %s\n", call.ID, decision)
	// allow / allow_session / deny 来自界面上的人（观测台另记一条它收到的点击）；unavailable / timeout 是程序按规则处理的。
	record.Event, record.Decision, record.Actor, record.Detail = "approval_decision", decision, "program", ""
	if decision == "allow" || decision == "allow_session" || decision == "deny" {
		record.Actor, record.Detail = "user", "经界面答复"
	}
	if decision == "allow_session" {
		record.Detail += "；本对话内都允许：" + scope
		grantMu.Lock()
		if !slices.Contains(granted, scope) {
			granted = append(granted, scope)
		}
		grantMu.Unlock()
		decision = "allow"
	}
	audit.Log(ctx, record)
	return decision
}

// 没得到“允许”时的结果：如实写明谁没同意，告诉模型不要换个说法再试。
func denied(call llm.ToolCall, kind, decision string) llm.Observation {
	text := map[string]string{
		"deny":        "用户拒绝了这项操作。",
		"timeout":     "等待用户确认超时，操作没有执行。",
		"unavailable": "这项操作需要用户在界面上确认，当前没有可以确认的界面，操作没有执行。",
		"audit_error": "审计日志写入失败，高风险操作不能在没有记录的情况下执行，操作没有执行。",
	}[decision]
	if text == "" {
		text = "操作没有得到确认，没有执行。"
	}
	return llm.Observation{ID: call.ID, Tool: call.Function.Name, Status: "denied",
		Error: text + "（风险：" + kind + "）不要换参数或换工具重试同一目的；在回答里说明需要用户确认的原因。"}
}

// 结果日志：每项调用一结束就把它的 Observation 单独落盘，不等整批写回检查点。
// 进程若死在同一批的其他调用上，续跑时已完成的调用直接取回记录，不再重做、也不再是“结果未知”。
// 剩下的窗口是“工具已经做完、记录还没写下”的一瞬间：这时仍按结果未知处理。
func journalPath(callID string) string {
	sum := sha256.Sum256([]byte(callID)) // 调用ID来自模型供应商，不直接拼进路径
	return filepath.Join(checkpointDir(tenant.Current), CheckpointID+".calls", hex.EncodeToString(sum[:8])+".json")
}

func writeJournal(o llm.Observation) {
	if CheckpointID == "" || o.Status == "not_run" {
		return
	}
	data, err := json.Marshal(o)
	if err == nil {
		path := journalPath(o.ID)
		err = os.MkdirAll(filepath.Dir(path), 0o700)
		if err == nil {
			err = os.WriteFile(path+".tmp", data, 0o600)
		}
		if err == nil {
			err = os.Rename(path+".tmp", path)
		}
	}
	if err != nil {
		fmt.Printf("Journal error [%s]: %v\n", o.ID, err)
	}
}

func readJournal(call llm.ToolCall) (llm.Observation, bool) {
	var o llm.Observation
	data, err := os.ReadFile(journalPath(call.ID))
	if err != nil || json.Unmarshal(data, &o) != nil || o.ID != call.ID || o.Tool != call.Function.Name {
		return llm.Observation{}, false
	}
	return o, true
}

// ---------- 循环检测 ----------

// 滑动窗口：最近 6 个动作里同一工具与参数出现 3 次就熔断。连续重复、A/B 交替都算。
const actionWindow, actionLimit = 6, 3

func pushAction(recent []string, key string) ([]string, int) {
	recent = append(recent, key)
	if len(recent) > actionWindow {
		recent = recent[len(recent)-actionWindow:]
	}
	count := 0
	for _, k := range recent {
		if k == key {
			count++
		}
	}
	return recent, count
}

// 无进展：一轮工具结果的指纹全都出现过，就是没有拿到新信息（换着关键词查到同样的空结果，也算）。
// 连续 2 轮给模型附一句提示，连续 3 轮熔断。指纹只看工具、状态、结果和错误，不看参数。
const staleHint, staleLimit = 2, 3

const staleNote = "最近几轮的工具结果和之前完全相同，没有新信息。不要再换说法重复查询或重复调用；根据已有结果回答用户，资料不足就直接说明。"

func fingerprint(o llm.Observation) string {
	var result any
	data, _ := json.Marshal(o.Result)
	json.Unmarshal(data, &result) // 结构体与 map 统一成 map：键排序后再比较，检查点里读回的结果也能对上
	data, _ = json.Marshal([]any{o.Tool, o.Status, result, o.Error})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// progress 是检查点里保存的循环检测状态；分叉回放时按保留的消息重新算一遍。
type progress struct {
	Recent []string `json:"recent_actions,omitempty"`
	Seen   []string `json:"seen_results,omitempty"`
	Stale  int      `json:"stale_rounds,omitempty"`
}

// observe 记下一轮的结果，返回更新后的无进展轮数。
func (p *progress) observe(observations []llm.Observation) int {
	fresh := false
	for _, o := range observations {
		if f := fingerprint(o); !slices.Contains(p.Seen, f) {
			p.Seen, fresh = append(p.Seen, f), true
		}
	}
	if fresh {
		p.Stale = 0
	} else {
		p.Stale++
	}
	return p.Stale
}

func replayProgress(messages []llm.Message) progress {
	var p progress
	for i, m := range messages {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		for _, call := range m.ToolCalls {
			p.Recent, _ = pushAction(p.Recent, actionKey(call))
		}
		var batch []llm.Observation
		for _, t := range messages[i+1:] {
			if t.Role != "tool" {
				break
			}
			var o llm.Observation
			if json.Unmarshal([]byte(t.Content), &o) == nil {
				batch = append(batch, o)
			}
		}
		if len(batch) > 0 {
			p.observe(batch)
		}
	}
	return p
}
