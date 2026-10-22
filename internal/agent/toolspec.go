package agent

import (
	"strings"
	"time"

	"learning-agent/internal/mcp"
	"learning-agent/internal/sandbox"
)

// D32：工具的性质只在这里声明一次。调度（并行还是串行）、续跑（能不能重做）、私有来源、外发与不可撤回的判断、
// 权限分类和单次时限都从这里读；新增一个工具，在 dispatch 里接上执行、在这里写一行就够了。
// 没写的工具取零值：可以并行，不能重做，不算私有，不算外发，权限按“其他工具”处理。
type Spec struct {
	ReadOnly     bool          // 不改任何状态
	Serial       bool          // 同一批里按调用顺序一个接一个执行：后一个常常建立在前一个之上（bash 的 a; b、连续两处编辑）
	Repeatable   bool          // 中断后可以按原参数放心重做：只读，或带幂等键（spawn_agent 的子任务ID）
	Private      string        // 成功后表示读入了用户的私有数据，值是来源名（D20）；空表示不算
	Outbound     bool          // 会把参数发到本机之外：读过私有数据后要确认（D20）
	Irreversible bool          // 做了就撤不回：每次都要确认（D20）
	Permission   string        // D25 规则里的类别：Bash、Edit、Read、WebFetch、WebSearch、Write；空表示按工具名
	Timeout      time.Duration // 单次尝试的时限；0 表示用 -tool-timeout
}

var specs = map[string]Spec{
	"calculator":             {ReadOnly: true, Repeatable: true},
	"get_current_datetime":   {ReadOnly: true, Repeatable: true},
	"search_notes":           {ReadOnly: true, Repeatable: true},
	"search_docs":            {ReadOnly: true, Repeatable: true},
	"read_security_sample":   {ReadOnly: true, Repeatable: true},
	"load_skill":             {ReadOnly: true, Repeatable: true},
	"read_output":            {ReadOnly: true, Repeatable: true},
	"check_task_status":      {ReadOnly: true, Repeatable: true},
	"search_memory":          {ReadOnly: true, Repeatable: true, Private: "memory"},
	"remember_memory":        {Permission: "Write"},
	"forget_memory":          {Irreversible: true, Permission: "Write"},
	"read_file":              {ReadOnly: true, Repeatable: true, Private: "workspace", Permission: "Read"},
	"grep":                   {ReadOnly: true, Repeatable: true, Private: "workspace", Permission: "Read"},
	"glob":                   {ReadOnly: true, Repeatable: true, Permission: "Read"}, // 只列文件名，不读内容
	"view_image":             {ReadOnly: true, Repeatable: true, Private: "workspace", Permission: "Read"},
	"edit_file":              {Serial: true, Permission: "Edit"},
	"write_file":             {Serial: true, Permission: "Edit"},
	"bash":                   {Serial: true, Private: "bash", Permission: "Bash"}, // 只在沙箱能读到用户文件时才算私有，见 trackPrivate
	"web_search":             {ReadOnly: true, Repeatable: true, Outbound: true, Permission: "WebSearch"},
	"open_page":              {ReadOnly: true, Repeatable: true, Outbound: true, Permission: "WebFetch"},
	"ds_search":              {ReadOnly: true, Repeatable: true, Outbound: true, Permission: "WebSearch"},
	"ds_fetch":               {ReadOnly: true, Repeatable: true, Outbound: true, Permission: "WebFetch"},
	"bash_output":            {ReadOnly: true, Repeatable: true, Private: "bash"},                              // 读后台命令的输出，和 bash 同样算私有
	"bash_kill":              {Repeatable: true},                                                               // 停掉已经停掉的命令什么也不做
	"progress":               {Serial: true, Private: "workspace"},                                             // D31：进度文件只由 harness 写
	"verify_feature":         {Serial: true, Repeatable: true, Private: "workspace", Timeout: 6 * time.Minute}, // 跑校验命令再请验证者判定
	"spawn_agent":            {Repeatable: true, Private: "subagent"},                                          // 子任务ID是幂等键；只在子 agent 读过私有数据时才算，见 trackPrivate
	"request_network_access": {Repeatable: true},
	"ask_user":               {Serial: true, Repeatable: true},                                    // D34：一次问一个；中断后再问一遍无妨
	"todo_write":             {Repeatable: true},                                                  // D37：每次写完整清单，重做一样
	"agent_wait":             {Repeatable: true, Private: "subagent", Timeout: 330 * time.Second}, // D39：只读取后台子 agent 的结论
	"agent_send":             {Serial: true},                                                      // 插两次话就是两句
	"agent_stop":             {Repeatable: true, Timeout: time.Minute},
}

// specOf 取一个工具的性质。时限跟着命令行参数走的，取的时候再填。
// MCP 工具按 server 的标注推出来：只读、幂等用于调度和续跑；外发与权限只收紧不放宽（D20：server 自称只读也不免检），
// 所以一律算外发、按“会写”的类别要权限；声明了 destructiveHint 的另算破坏性（见 risk）。没有标注就按“会写、不可重做”。
func specOf(name string) Spec {
	if strings.HasPrefix(name, "mcp_") {
		hints := mcp.HintsOf(name)
		return Spec{ReadOnly: hints.ReadOnly, Serial: !hints.ReadOnly, Repeatable: hints.ReadOnly || hints.Idempotent, Outbound: true, Permission: "Write"}
	}
	spec := specs[name]
	switch name {
	case "spawn_agent":
		spec.Timeout = SubagentTimeout
	case "request_network_access":
		spec.Timeout = sandbox.ApprovalWait + 30*time.Second // 等人点按钮，工具自己会在 ApprovalWait 到时返回 pending
	case "ask_user":
		spec.Timeout = AskWait + 30*time.Second // 等人回答，工具自己会在 AskWait 到时返回
	}
	return spec
}
