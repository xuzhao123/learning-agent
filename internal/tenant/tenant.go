package tenant

import (
	"path/filepath"
	"regexp"
)

// 多租户的数据边界：每个租户一棵自己的目录，检查点、记忆、沙箱工作目录、浏览器画面、对话存档和审计都在里面。
//
//	平台空间（租户为空）  .data/…                    命令行直接运行、评测、归因，与之前的单用户用法相同
//	租户空间              .data/tenants/<租户>/…     观测台里租户用户的对话、队列里标了租户的任务
//
// 隔离靠“路径由租户决定”：agent 进程用 -tenant 启动后，所有读写都落在这个租户的目录下，
// 别的租户的任务ID、子任务、记忆编号在这里根本找不到，不需要每处再加一道比对。
// 模型密钥、调用链（.data/traces）、Skills/MCP 配置与登录账号属于平台，不按租户分。
var Current string // 本进程所属的租户；空表示平台空间。由 main 按 -tenant 设置

var pattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Valid：租户ID会拼进路径，只允许小写字母、数字和连字符。
func Valid(id string) bool { return pattern.MatchString(id) }

// Path 返回某个空间里的数据路径；观测台一个进程服务多个租户，按请求的用户传入 space。
func Path(space string, parts ...string) string {
	if space == "" {
		return filepath.Join(append([]string{".data"}, parts...)...)
	}
	return filepath.Join(append([]string{".data", "tenants", space}, parts...)...)
}

// Here 是本进程空间里的路径。
func Here(parts ...string) string { return Path(Current, parts...) }
