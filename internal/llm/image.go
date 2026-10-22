package llm

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif" // GIF 只取第一帧：image.Decode 解出来的就是第一帧
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"

	"learning-agent/internal/tenant"
)

// Image 是一张随消息发给模型的图。消息、检查点、对话存档和结果日志里都只放引用，
// 只有真正发请求前才展开成 data URL（见 prepareHistory）：同样的字节能发给多个供应商，
// 也不用把 base64 在每次请求里重复写盘。
// 消息里的图随 content 数组读写（见 Message.MarshalJSON）；JSON 标签是给 view_image 的结果用的：
// 观测 JSON、结果日志和协议事件里就是这几项，没有图片字节。
type Image struct {
	Ref       string `json:"ref"`             // attachment://<sha256>.<扩展名>
	Label     string `json:"label,omitempty"` // 模型引用的编号 [Image #1]
	Path      string `json:"path"`            // 给模型看的来源（文件名或路径）
	Width     int    `json:"width"`           // 发给模型的尺寸
	Height    int    `json:"height"`
	SrcWidth  int    `json:"source_width"` // 原尺寸；与上面相同表示没有缩放
	SrcHeight int    `json:"source_height"`
}

const (
	// 方舟的像素上限（196 到 3600 万），同时也挡住解压炸弹。
	maxImagePixels = 36_000_000
	// 高清档（照 Codex）：长边不超过 2048，32×32 块数不超过 2500。
	maxImageSide   = 2048
	maxImageBlocks = 2500
	// ImageTokens 是每张图在估算里占的 token：豆包 high 档单图固定 1280，不看 base64 长度。
	ImageTokens = 1280
	// MaxImages 是每条用户消息最多带几张图：agent（-image 与 turn/start）和观测台共用这一个上限。
	MaxImages = 8
	// MaxImageBytes 是一张原图最多多大：观测台上传与 view_image 读文件共用。
	MaxImageBytes = 20 << 20

	attachmentPrefix = "attachment://"
	imageCloseTag    = "</image>"
)

// PrepareImage 把一张图变成可发给模型的 Image（照 Codex 的高清档）。命令行、观测台和之后的
// view_image 共用它：按文件头只收 PNG、JPEG、GIF、WebP，像素过多直接拒绝；
// 超过 2048 长边或 2500 个 32×32 块时等比缩小，不需要缩放的 PNG、JPEG、WebP 原样保留字节，
// 其余重新编码（JPEG 质量 85，其他编码成 PNG，GIF 只取第一帧）。字节按内容寻址写进附件目录。
// 失败返回错误，由调用方换成文字占位：一张图处理不了不该让这一轮失败。
func PrepareImage(data []byte, name string) (Image, error) {
	format := imageFormat(data)
	if format == "" {
		return Image{}, errors.New("格式不支持（只支持 PNG、JPEG、GIF、WebP）")
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Image{}, errors.New("图片无法解码")
	}
	srcWidth, srcHeight := config.Width, config.Height
	if srcWidth <= 0 || srcHeight <= 0 {
		return Image{}, errors.New("图片尺寸无效")
	}
	if srcWidth*srcHeight > maxImagePixels {
		return Image{}, fmt.Errorf("像素太多（%d×%d，上限 %d 万）", srcWidth, srcHeight, maxImagePixels/10000)
	}
	width, height := fitImage(srcWidth, srcHeight)
	out := data
	// GIF 就算不缩放也要重编码：动图只留第一帧，模型不需要整段动画。
	if width != srcWidth || height != srcHeight || format == "gif" {
		source, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return Image{}, errors.New("图片无法解码")
		}
		scaled := image.NewRGBA(image.Rect(0, 0, width, height))
		draw.CatmullRom.Scale(scaled, scaled.Bounds(), source, source.Bounds(), draw.Over, nil)
		if out, err = encodeImage(scaled, format); err != nil {
			return Image{}, err
		}
	}
	ref, err := StoreAttachment(tenant.Here("attachments"), out)
	if err != nil {
		return Image{}, err
	}
	return Image{Ref: ref, Path: name, Width: width, Height: height, SrcWidth: srcWidth, SrcHeight: srcHeight}, nil
}

// fitImage 返回缩放后的尺寸：长边不超过 2048，32×32 块数不超过 2500。缩放是等比的，
// 块数按 32 向上取整，同一个比例算出来的块数可能还多一点：这时把比例调小再算，两边一起缩，宽高比不变。
func fitImage(width, height int) (int, int) {
	longest := max(width, height)
	if longest <= maxImageSide && imageBlocks(width, height) <= maxImageBlocks {
		return width, height
	}
	scale := math.Min(float64(maxImageSide)/float64(longest), math.Sqrt(float64(maxImageBlocks*32*32)/float64(width*height)))
	for {
		scaledWidth, scaledHeight := max(1, int(float64(width)*scale)), max(1, int(float64(height)*scale))
		if imageBlocks(scaledWidth, scaledHeight) <= maxImageBlocks {
			return scaledWidth, scaledHeight
		}
		scale *= 0.99
	}
}

func imageBlocks(width, height int) int {
	return (width + 31) / 32 * ((height + 31) / 32)
}

// imageFormat 按文件头认格式，不看扩展名：改过后缀的图片照样按内容处理。
func imageFormat(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return "jpeg"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "gif"
	case len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && string(data[8:12]) == "WEBP":
		return "webp"
	}
	return ""
}

// encodeImage 按原格式重编码：JPEG 仍编成 JPEG（质量 85，与 Codex 相同），其余编成 PNG（Go 没有 WebP 编码器）。
func encodeImage(source image.Image, format string) ([]byte, error) {
	var buf bytes.Buffer
	if format == "jpeg" {
		if err := jpeg.Encode(&buf, source, &jpeg.Options{Quality: 85}); err != nil {
			return nil, errors.New("图片重新编码失败")
		}
		return buf.Bytes(), nil
	}
	if err := png.Encode(&buf, source); err != nil {
		return nil, errors.New("图片重新编码失败")
	}
	return buf.Bytes(), nil
}

// StoreAttachment 按内容寻址把图片存进 dir（目录由调用方按租户给出）：同样的字节只存一份，
// 引用里带哈希和扩展名，扩展名由文件头决定（JPEG 写 jpg）。不是图片的字节直接报错。
// 附件是用户的私有数据，权限和 .data 里其他私有文件一样（目录 0700、文件 0600）；
// 先写临时文件再改名，并发写同一张图或写到一半失败，都不会留下半个文件。
func StoreAttachment(dir string, data []byte) (string, error) {
	format := imageFormat(data)
	if format == "" {
		return "", errors.New("格式不支持（只支持 PNG、JPEG、GIF、WebP）")
	}
	ext := format
	if format == "jpeg" {
		ext = "jpg"
	}
	sum := sha256.Sum256(data)
	name := hex.EncodeToString(sum[:]) + "." + ext
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", errors.New("附件目录无法创建")
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err == nil {
		return attachmentPrefix + name, nil
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", errors.New("附件无法保存")
	}
	_, err = tmp.Write(data)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return "", errors.New("附件无法保存")
	}
	return attachmentPrefix + name, nil
}

// attachmentRef 是合法的附件引用：内容寻址的名字（64 位十六进制）加认识的扩展名。
// 名字里不许带路径，所以引用只能在附件目录里找。
var attachmentRef = regexp.MustCompile(`^attachment://[0-9a-f]{64}\.(png|jpg|gif|webp)$`)

// AttachmentPath 校验附件引用并给出它在 dir 里的路径。dir 由调用方按租户给出，
// 别的租户的引用在这里根本找不到文件。
func AttachmentPath(dir, ref string) (string, error) {
	if !attachmentRef.MatchString(ref) {
		return "", errors.New("不是有效的附件引用")
	}
	return filepath.Join(dir, strings.TrimPrefix(ref, attachmentPrefix)), nil
}

// LoadAttachment 读取 attachment:// 引用指向的字节（本进程空间的附件目录）。
func LoadAttachment(ref string) ([]byte, error) {
	path, err := AttachmentPath(tenant.Here("attachments"), ref)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(path)
}

// StoreDataURL 把 data:<mime>;base64,<数据> 解码后按内容寻址存进 dir，返回附件引用。
// agent 发出的 data URL 就是附件文件的原字节，所以算出来的名字和 agent 写入时相同：
// 观测台存档时用它把请求体里的 base64 换回引用。
func StoreDataURL(dir, url string) (string, error) {
	rest, ok := strings.CutPrefix(url, "data:")
	if !ok {
		return "", errors.New("不是 data URL")
	}
	header, payload, ok := strings.Cut(rest, ",")
	if !ok || !strings.HasSuffix(header, ";base64") {
		return "", errors.New("只支持 base64 的 data URL")
	}
	data, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return "", errors.New("data URL 无法解码")
	}
	return StoreAttachment(dir, data)
}

var mimeTypes = map[string]string{"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif", "webp": "image/webp"}

// ImageMIME 按文件头给出图片的 MIME 类型（和存储用的扩展名同一份判断）；不是支持的图片返回空字符串。
func ImageMIME(data []byte) string {
	return mimeTypes[imageFormat(data)]
}

// dataURL 把引用展开成发给模型的 data URL：只有这一步真正读图片字节。
func (i Image) dataURL() (string, error) {
	if strings.HasPrefix(i.Ref, "data:") {
		// 从旧存档读回的请求里已经是 data URL（早期没有附件存储），原样发出去。
		return i.Ref, nil
	}
	data, err := LoadAttachment(i.Ref)
	if err != nil {
		return "", err
	}
	mime, ok := mimeTypes[strings.TrimPrefix(filepath.Ext(i.Ref), ".")]
	if !ok {
		return "", errors.New("未知的附件类型")
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// OpenTag 是图片前面的文字标签（照 Codex），缩放过的把原尺寸也写进去：
//
//	<image name=[Image #1] path="screenshot.png" size="1536x2048" original="3024x4032">
func (i Image) OpenTag() string {
	label := i.Label
	if label == "" {
		label = "[Image]"
	}
	tag := fmt.Sprintf(`<image name=%s path=%s size="%dx%d"`, label, strconv.Quote(i.Path), i.Width, i.Height)
	if i.Scaled() {
		tag += fmt.Sprintf(` original="%dx%d"`, i.SrcWidth, i.SrcHeight)
	}
	return tag + ">"
}

// Scaled 表示这张图缩过：原尺寸和发给模型的尺寸不一样。
func (i Image) Scaled() bool {
	return i.SrcWidth > 0 && i.SrcHeight > 0 && (i.SrcWidth != i.Width || i.SrcHeight != i.Height)
}

var imageLabelPattern = regexp.MustCompile(`\[Image #(\d+)\]`)

// LastImageNumber 返回历史里用过的最大图片编号：续聊时新图接着往后编，不和前几轮的图重名。
// 压缩后图片可能只剩文字标签，所以文字里出现的编号也算。
func LastImageNumber(messages []Message) int {
	n := 0
	for _, m := range messages {
		texts := []string{m.Content}
		for _, img := range m.Images {
			texts = append(texts, img.Label)
		}
		for _, text := range texts {
			for _, groups := range imageLabelPattern.FindAllStringSubmatch(text, -1) {
				if v, err := strconv.Atoi(groups[1]); err == nil {
					n = max(n, v)
				}
			}
		}
	}
	return n
}

// ImageNote 是图片处理失败时留给模型的文字占位：告诉它这里本来有哪张图、为什么没发出去。
func ImageNote(name string, err error) string {
	return "图片 " + name + " 无法处理：" + err.Error()
}

// name 里有空格（[Image #1]），所以按非贪婪匹配到 path 之前。
var imageTagPattern = regexp.MustCompile(`^<image name=(.+?) path=("(?:[^"\\]|\\.)*") size="(\d+)x(\d+)"(?: original="(\d+)x(\d+)")?>$`)

// parseImageTag 认回 OpenTag 写出的文字标签，把编号、文件名和尺寸还原成 Image。
func parseImageTag(text string) (Image, bool) {
	groups := imageTagPattern.FindStringSubmatch(strings.TrimSpace(text))
	if groups == nil {
		return Image{}, false
	}
	path, err := strconv.Unquote(groups[2])
	if err != nil {
		return Image{}, false
	}
	img := Image{Label: groups[1], Path: path}
	img.Width, _ = strconv.Atoi(groups[3])
	img.Height, _ = strconv.Atoi(groups[4])
	img.SrcWidth, img.SrcHeight = img.Width, img.Height
	if groups[5] != "" {
		img.SrcWidth, _ = strconv.Atoi(groups[5])
		img.SrcHeight, _ = strconv.Atoi(groups[6])
	}
	return img, true
}

// contentPart 是 content 数组里的一段：图片用 image_url，其余都是文字。
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// prepareHistory 是发请求前的最后一步：把 attachment:// 展开成 data URL。
// 模型不能看图时，把历史和工具结果里的图换成说明文字（照 Codex 的 strip_images_when_unsupported）：
// 这一道兜住续跑、回放和命令行，界面上的禁用与观测台的校验挡不住它们。
func prepareHistory(history []Message, vision bool) []Message {
	out := make([]Message, len(history))
	for i, m := range history {
		if m.Role == "tool" && ToolFormat == "text" {
			m.Content = ToolText(m.Content)
		}
		if len(m.Images) == 0 {
			out[i] = m
			continue
		}
		if !vision {
			out[i] = omitImages(m, "图片已省略：当前模型不支持图片输入")
			continue
		}
		m.Images = append([]Image(nil), m.Images...)
		images := make([]Image, 0, len(m.Images))
		var failed []string
		for _, img := range m.Images {
			url, err := img.dataURL()
			if err != nil {
				// 附件丢了这一轮也照常跑：这张图换成说明文字，其余照发。
				failed = append(failed, img.OpenTag()+"\n图片无法发送："+err.Error()+"\n"+imageCloseTag)
				continue
			}
			img.Ref = url
			images = append(images, img)
		}
		m.Images = images
		if len(failed) > 0 {
			if m.Content != "" {
				failed = append(failed, m.Content)
			}
			m.Content = strings.Join(failed, "\n")
		}
		out[i] = m
	}
	return out
}

// omitImages 把图片换成说明文字，开标签和闭标签留着：模型知道这里原来有一张什么图、多大。
func omitImages(m Message, note string) Message {
	lines := make([]string, 0, len(m.Images)+1)
	for _, img := range m.Images {
		lines = append(lines, img.OpenTag()+"\n"+note+"\n"+imageCloseTag)
	}
	if m.Content != "" {
		lines = append(lines, m.Content)
	}
	m.Content, m.Images = strings.Join(lines, "\n"), nil
	return m
}

// ImagesToText 把消息里的图换成开标签文字。摘要请求只发文字（压缩时图片不进摘要请求），
// 标签里编号、文件名和尺寸都在，模型知道这里原来有一张图。
func ImagesToText(messages []Message) []Message {
	out := make([]Message, len(messages))
	for i, m := range messages {
		if len(m.Images) == 0 {
			out[i] = m
			continue
		}
		tags := make([]string, 0, len(m.Images)+1)
		for _, img := range m.Images {
			tags = append(tags, img.OpenTag())
		}
		if m.Content != "" {
			tags = append(tags, m.Content)
		}
		m.Content, m.Images = strings.Join(tags, "\n"), nil
		out[i] = m
	}
	return out
}
