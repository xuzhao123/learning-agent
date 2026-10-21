package observer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"learning-agent/internal/audit"
	"learning-agent/internal/llm"
	"learning-agent/internal/tenant"
)

// 附图：页面把原图存进当前租户的附件目录（按内容寻址，agent 写入的是同一个目录），
// 之后的消息、检查点与对话存档里只放 attachment:// 引用。缩放交给 agent 做，界面显示原图。
//
// 图是用户的私有数据：目录 0700、文件 0600（llm.StoreAttachment），读取只看当前账号的租户目录；
// 上传与别的会改变状态的操作一样先写审计，写不进审计就不保存。

// attachmentDir 是某个空间的附件目录：租户各自的 .data/tenants/<租户>/attachments，平台是 .data/attachments。
func (s *server) attachmentDir(space string) string {
	return filepath.Join(s.agentDir, tenant.Path(space, "attachments"))
}

// 上传附图：请求体是图片字节，文件名放在 ?name=。名字只给模型看的来源和界面说明用，
// 存下来的名字由内容决定，所以同名不同图、不同名同图都不会打架。
func (s *server) uploadAttachment(w http.ResponseWriter, req *http.Request) {
	data, err := io.ReadAll(http.MaxBytesReader(w, req.Body, llm.MaxImageBytes))
	if err != nil {
		http.Error(w, "图片不能超过 20 MB", http.StatusRequestEntityTooLarge)
		return
	}
	if llm.ImageMIME(data) == "" {
		http.Error(w, "只支持 PNG、JPEG、GIF、WebP", http.StatusBadRequest)
		return
	}
	name := attachmentName(req.URL.Query().Get("name"))
	u := current(req)
	record := audit.Record{Actor: "user", User: u.Name, Tenant: u.Tenant, Event: "user_attach", Target: name, Detail: "观测台页面"}
	record.Args(string(data))
	if err := audit.Write(req.Context(), record); err != nil {
		http.Error(w, "审计日志写入失败，没有保存这张图："+err.Error(), http.StatusInternalServerError)
		return
	}
	ref, err := llm.StoreAttachment(s.attachmentDir(u.Tenant), data)
	if err != nil {
		http.Error(w, "这张图存不下来："+err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ref": ref, "name": name})
}

// 读取附图：引用不合法、文件不存在、内容不是图片都是 404。别的租户的图在本租户目录里本来就找不到，
// 所以按编号访问的接口这里不需要再比对归属。Content-Type 只取按文件头认出的那一种并禁止嗅探。
func (s *server) serveAttachment(w http.ResponseWriter, req *http.Request) {
	path, err := llm.AttachmentPath(s.attachmentDir(current(req).Tenant), "attachment://"+req.PathValue("name"))
	if err != nil {
		http.NotFound(w, req)
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		http.NotFound(w, req)
		return
	}
	mime := llm.ImageMIME(data)
	if mime == "" {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Write(data)
}

// attachmentName 把上传时给的文件名收成一层、无控制字符、最长 128 字节；空的就是 image。
func attachmentName(raw string) string {
	name := strings.Map(func(c rune) rune {
		if unicode.IsControl(c) {
			return -1
		}
		return c
	}, strings.TrimSpace(raw))
	// 只要最后一层：浏览器给的是文件名，传路径也只会留下名字本身。
	if name = filepath.Base(name); name == "." || name == ".." || name == string(filepath.Separator) || !utf8.ValidString(name) {
		name = ""
	}
	if len(name) > 128 {
		name = strings.ToValidUTF8(name[:128], "")
	}
	if name == "" {
		return "image"
	}
	return name
}

// attachRef 是消息里的一张图：字段名与 agent turn/start 的 images 一致。
type attachRef struct {
	Ref  string `json:"ref"`
	Name string `json:"name"`
}

// checkImages 校验一条消息带的图：最多 llm.MaxImages 张，每个引用都要是本租户附件目录里的文件。
func (s *server) checkImages(w http.ResponseWriter, space string, images []attachRef) bool {
	if len(images) > llm.MaxImages {
		http.Error(w, fmt.Sprintf("每条消息最多 %d 张图", llm.MaxImages), http.StatusBadRequest)
		return false
	}
	for _, img := range images {
		path, err := llm.AttachmentPath(s.attachmentDir(space), img.Ref)
		if err != nil {
			http.Error(w, "不是有效的附件引用："+img.Ref, http.StatusBadRequest)
			return false
		}
		if _, err := os.Stat(path); err != nil {
			http.Error(w, "找不到这张图，请重新上传："+img.Ref, http.StatusBadRequest)
			return false
		}
	}
	return true
}

// visionModel 回答“这家供应商的这个模型能不能看图”，规则与 agent 完全一致（llm.Config.Vision）。
// model 为空表示用这家供应商的默认值（.env 或路由表决定），与 agent 不传 -model 时一样。
func visionModel(provider, model string) (string, bool) {
	config, _ := llm.LoadConfig(provider) // 没有密钥时仍会解析出模型名
	if model != "" {
		config.Model = model
	}
	return config.Model, config.Vision()
}

// archiveImages 只在存档时把请求体里的图片 data URL 换成 attachment:// 引用：同一张图每次请求都带，
// 内联进按请求存的存档会成倍膨胀。转发给上游的仍是原请求体，一个字节都不改。
//
// 没有图片的请求原样返回（纯文字请求的存档与之前逐字节一致）；解析或存储任何一步不顺就保留原来的
// data URL：存档宁可大一点，也要如实反映模型当时看到了什么。
func (s *server) archiveImages(space string, body []byte) []byte {
	if !bytes.Contains(body, []byte(`"data:image/`)) {
		return body
	}
	var request map[string]json.RawMessage
	var messages []map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil || json.Unmarshal(request["messages"], &messages) != nil {
		return body
	}
	dir := s.attachmentDir(space)
	changed := false
	for _, message := range messages {
		var parts []map[string]json.RawMessage
		// content 是字符串的消息（纯文字、工具结果文字）不用看。
		if json.Unmarshal(message["content"], &parts) != nil {
			continue
		}
		dirty := false
		for _, part := range parts {
			var kind string
			var image struct {
				URL string `json:"url"`
			}
			if json.Unmarshal(part["type"], &kind) != nil || kind != "image_url" ||
				json.Unmarshal(part["image_url"], &image) != nil || !strings.HasPrefix(image.URL, "data:") {
				continue
			}
			ref, err := llm.StoreDataURL(dir, image.URL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "存档：有一张图没有换成引用（%v），按原样记录\n", err)
				continue
			}
			part["image_url"], _ = json.Marshal(map[string]string{"url": ref})
			dirty = true
		}
		if !dirty {
			continue
		}
		if data, err := json.Marshal(parts); err == nil {
			message["content"], changed = data, true
		}
	}
	if !changed {
		return body
	}
	data, err := json.Marshal(messages)
	if err != nil {
		return body
	}
	request["messages"] = data
	if data, err = json.Marshal(request); err != nil {
		return body
	}
	return data
}
