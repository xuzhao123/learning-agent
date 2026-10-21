package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"learning-agent/internal/llm"
	"learning-agent/internal/sandbox"
	"learning-agent/internal/workspace"
)

// 图片输入第二步：view_image 让模型自己看工作区或 bash 沙箱里的图（照 Codex 的 view_image）。
// 读到的字节和用户附图走同一个 llm.PrepareImage：按文件头只收 PNG、JPEG、GIF、WebP，大图等比缩小，
// 按内容寻址存进附件目录。结果就是那张 llm.Image：主循环把它放进 tool 消息的 Images，
// 观测 JSON、结果日志、终端和协议事件里只有引用与尺寸，没有图片字节。
// 不能看图的模型不提供这个工具（main 按路由表的 Vision 决定；Codex 是提供后再拒绝）。
var ViewImageDefinition = map[string]any{
	"name":        "view_image",
	"description": "查看一张本地图片（PNG、JPEG、GIF、WebP），图片随工具结果发给你。大图会等比缩小，结果里有发给你的尺寸和原尺寸。",
	"parameters": map[string]any{"type": "object", "required": []string{"path"}, "properties": map[string]any{
		"path": map[string]string{"type": "string", "description": "工作区里的相对路径；开了 bash 时也可以写沙箱里的路径（/workspace/…，有工作区时还有 /scratch/…）"},
	}},
}

func viewImage(arguments string) (any, error) {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return nil, llm.Permanent(fmt.Errorf("参数类型不对：%v", err))
	}
	dir, rel, err := imageLocation(strings.TrimSpace(args.Path))
	if err != nil {
		return nil, err
	}
	// 和文件工具一样经 os.Root 读：../ 和指向目录外的符号链接都出不去。
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, llm.Permanent(fmt.Errorf("文件不存在：%s", args.Path))
	}
	defer root.Close()
	info, err := root.Stat(rel)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, llm.Permanent(fmt.Errorf("文件不存在：%s", args.Path))
	case err != nil:
		return nil, llm.Permanent(fmt.Errorf("无法访问 %s：%v", args.Path, err))
	case info.IsDir():
		return nil, llm.Permanent(fmt.Errorf("%s 是目录，view_image 只看单个图片文件", args.Path))
	case info.Size() > llm.MaxImageBytes:
		return nil, llm.Permanent(fmt.Errorf("%s 有 %d 字节，图片不能超过 %d MB", args.Path, info.Size(), llm.MaxImageBytes>>20))
	}
	data, err := root.ReadFile(rel)
	if err != nil {
		return nil, llm.Permanent(fmt.Errorf("无法读取 %s：%v", args.Path, err))
	}
	img, err := llm.PrepareImage(data, args.Path)
	if err != nil {
		return nil, llm.Permanent(fmt.Errorf("%s 不能作为图片查看：%v", args.Path, err))
	}
	return img, nil
}

// imageLocation 把模型给的路径换成“在哪个目录里读、相对路径是什么”。规则和文件工具、bash 看到的一致：
// 有工作区时相对路径与工作区内的绝对路径都在工作区里；开了 bash 时 /workspace 是工作区（没有工作区时是本对话的沙箱目录），
// 有工作区时本对话的沙箱目录挂在 /scratch（见 sandbox 的挂载）。
func imageLocation(p string) (dir, rel string, err error) {
	if p == "" {
		return "", "", llm.Permanent(errors.New("path 不能为空"))
	}
	if sandbox.Enabled {
		if rest, ok := under(p, "/workspace"); ok {
			if workspace.Dir != "" {
				return workspace.Dir, rest, nil
			}
			return sandbox.Dir, rest, nil
		}
		if rest, ok := under(p, "/scratch"); ok && workspace.Dir != "" {
			return sandbox.Dir, rest, nil
		}
		if workspace.Dir == "" && !filepath.IsAbs(p) {
			return sandbox.Dir, filepath.Clean(p), nil
		}
	}
	if workspace.Dir != "" {
		if !filepath.IsAbs(p) {
			return workspace.Dir, filepath.Clean(p), nil
		}
		if rest, ok := under(p, workspace.Dir); ok {
			return workspace.Dir, rest, nil
		}
	}
	return "", "", llm.Permanent(fmt.Errorf("路径不在工作区或 bash 沙箱里：%s", p))
}

// under 判断 p 是不是 dir 或它下面的路径，返回相对 dir 的部分（dir 本身是 "."）。
func under(p, dir string) (string, bool) {
	rel, err := filepath.Rel(dir, filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}
