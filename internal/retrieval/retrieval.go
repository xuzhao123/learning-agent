package retrieval

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"learning-agent/internal/telemetry"

	"go.opentelemetry.io/otel/attribute"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

var Enabled bool
var Docs *Retriever

// 建索引进度默认随终端轨迹输出；-search-docs 改写到stderr，让stdout只有JSON结果。
var IndexLog io.Writer = os.Stdout

var SearchDefinition = map[string]any{
	"name":        "search_docs",
	"description": "按语义检索本项目 Day 1–4 知识库。返回最多 k 条片段、来源、余弦相似度及 accepted 标记；只用 accepted=true 且正文支持结论的片段作证据，以 [D编号] 引用。无充分证据时说明资料不足。",
	"parameters": map[string]any{
		"type": "object", "required": []string{"query"},
		"properties": map[string]any{
			"query": map[string]any{"type": "string", "description": "简短的语义查询，保留关键数字、标识符"},
			"k":     map[string]any{"type": "integer", "minimum": 1, "maximum": 6, "description": "最多返回的片段数量，省略时为3"},
		},
	},
}

type Hit struct {
	ID       string  `json:"id"`
	Title    string  `json:"title"`
	Source   string  `json:"source"`
	Text     string  `json:"text"`
	Score    float64 `json:"score"`
	Accepted bool    `json:"accepted"`
}

type Result struct {
	Query     string  `json:"query"`
	K         int     `json:"k"`
	Threshold float64 `json:"threshold"`
	Model     string  `json:"model"`
	Backend   string  `json:"backend"`
	Corpus    string  `json:"corpus"`
	Status    string  `json:"status"`
	Hits      []Hit   `json:"hits"`
}

const modelName = "sentence-transformers/paraphrase-multilingual-MiniLM-L12-v2"
const revision = "e8f8c211226b894fcb81acc59f3b34ba3efd5f42"
const MinScore = 0.40 // 教学起点，不是答案正确率；更换模型或语料后需重新评估。
const TokenLimit = 128

type document struct {
	ID     string    `json:"id"`
	Title  string    `json:"title"`
	Source string    `json:"source"`
	Text   string    `json:"text"`
	Vector []float32 `json:"vector,omitempty"`
}

// 检索与Agent在同一进程；索引只初始化一次，工具调用直接读取它。
type Retriever struct {
	embedding             *Embedder
	docs                  []document
	provider, fingerprint string
	threshold             float64
}

func New(ctx context.Context, provider string, threshold float64) (*Retriever, error) {
	// 向量模型按进程共享（长期记忆也用它），由 main 退出时统一释放。
	embedding, err := SharedEmbedder(provider)
	if err != nil {
		return nil, err
	}
	docs, fingerprint, err := buildIndex(ctx, filepath.Join(".cache", "retrieval-go"), embedding)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(IndexLog, "Index ready: documents=%d dimensions=%d model=%s corpus=%s backend=%s\n", len(docs), embedding.Dimensions, embedding.Model, fingerprint[:12], provider)
	return &Retriever{embedding: embedding, docs: docs, provider: provider, fingerprint: fingerprint, threshold: threshold}, nil
}

func Search(ctx context.Context, query string, k int) (result Result, err error) {
	// D13：一次检索一个 retrieval span：查询向量化（embeddings）是它的子 span，打分排序在本进程内完成。
	ctx, span := telemetry.Begin(ctx, "retrieval docs", trace.SpanKindInternal, semconv.GenAIOperationNameRetrieval, semconv.GenAIDataSourceID("docs"), attribute.Int("gen_ai.retrieval.top_k", k))
	telemetry.Content(span, semconv.GenAIRetrievalQueryTextKey, query)
	defer func() {
		accepted := 0
		for _, h := range result.Hits {
			if h.Accepted {
				accepted++
			}
		}
		span.SetAttributes(attribute.String("agent.retrieval.status", result.Status), attribute.Int("agent.retrieval.hits", len(result.Hits)), attribute.Int("agent.retrieval.accepted", accepted))
		telemetry.End(span, err, "retrieval_error")
	}()
	if strings.TrimSpace(query) == "" || utf8.RuneCountInString(query) > 1000 || k < 1 || k > 6 {
		return Result{}, errors.New("query需为1–1000字符，k需为1–6的整数")
	}
	r := Docs
	if r == nil {
		return Result{}, errors.New("知识库未初始化，请使用-rag或-search-docs")
	}
	vector, err := r.embedding.Embed(ctx, query, true)
	if err != nil {
		return Result{}, err
	}
	// 单位向量的点积就是余弦相似度；24条文档直接遍历即可。
	hits := make([]Hit, 0, len(r.docs))
	for _, doc := range r.docs {
		var score float64
		for i, x := range vector {
			score += float64(x) * float64(doc.Vector[i])
		}
		hits = append(hits, Hit{ID: doc.ID, Title: doc.Title, Source: doc.Source, Text: doc.Text, Score: score})
	}
	slices.SortStableFunc(hits, func(a, b Hit) int {
		if a.Score > b.Score {
			return -1
		}
		if a.Score < b.Score {
			return 1
		}
		return 0
	})
	hits = hits[:min(k, len(hits))]
	status := "insufficient"
	for i := range hits {
		h := &hits[i]
		h.Accepted = h.Score >= r.threshold
		h.Score = math.Round(h.Score*10000) / 10000
		if h.Accepted {
			status = "ok"
		} else {
			h.Text = ""
		} // 低分正文不进入模型上下文。
	}
	return Result{Query: query, K: k, Threshold: r.threshold, Model: r.embedding.Model, Backend: r.provider, Corpus: r.fingerprint[:12], Status: status, Hits: hits}, nil
}

func buildIndex(ctx context.Context, cache string, embedding *Embedder) ([]document, string, error) {
	text, err := os.ReadFile("retrieval/corpus.md")
	if err != nil {
		return nil, "", err
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(append([]byte(embedding.Key), text...)))
	var saved struct {
		Fingerprint string
		Documents   []document
	}
	path := filepath.Join(cache, "index-"+fingerprint[:12]+".json")
	if data, err := os.ReadFile(path); err == nil && json.Unmarshal(data, &saved) == nil && saved.Fingerprint == fingerprint && len(saved.Documents) > 0 {
		valid := true
		for _, doc := range saved.Documents {
			valid = valid && len(doc.Vector) == embedding.Dimensions
		}
		if valid {
			return saved.Documents, fingerprint, nil
		}
	}
	var docs []document
	for _, section := range strings.Split(string(text), "\n## ")[1:] {
		lines := strings.SplitN(section, "\n", 3)
		if len(lines) != 3 {
			return nil, "", fmt.Errorf("FAQ格式有误")
		}
		id, title, ok := strings.Cut(lines[0], " | ")
		if !ok {
			return nil, "", fmt.Errorf("FAQ标题缺少编号")
		}
		docs = append(docs, document{ID: id, Title: title, Source: strings.TrimPrefix(lines[1], "来源："), Text: strings.TrimSpace(lines[2])})
	}
	if len(docs) == 0 {
		return nil, "", fmt.Errorf("知识库为空")
	}
	// 逐条建向量，CPU和内存占用更平稳；只在语料或模型配置变化时执行。
	for i := range docs {
		vector, err := embedding.Embed(ctx, docs[i].Title+"\n"+docs[i].Text, false)
		if err != nil {
			return nil, "", fmt.Errorf("%s: %w", docs[i].ID, err)
		}
		docs[i].Vector = vector
		fmt.Fprintf(IndexLog, "Index: %d/%d %s\n", i+1, len(docs), docs[i].ID)
	}
	saved.Fingerprint, saved.Documents = fingerprint, docs
	data, err := json.Marshal(saved)
	if err != nil {
		return nil, "", err
	}
	if err = os.WriteFile(path+".tmp", data, 0600); err == nil {
		err = os.Rename(path+".tmp", path)
	}
	return docs, fingerprint, err
}

func downloadModel(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for _, name := range []string{"config.json", "tokenizer.json", "onnx/model.onnx"} {
		target := filepath.Join(dir, filepath.Base(name))
		if info, err := os.Stat(target); err == nil && info.Size() > 0 {
			continue
		}
		fmt.Fprintln(IndexLog, "Download:", name, "（首次下载，后续复用本地缓存）")
		client := &http.Client{Timeout: 10 * time.Minute}
		resp, err := client.Get("https://huggingface.co/" + modelName + "/resolve/" + revision + "/" + name)
		if err != nil {
			return err
		}
		if resp.StatusCode != 200 {
			resp.Body.Close()
			return fmt.Errorf("模型下载 HTTP %d", resp.StatusCode)
		}
		file, err := os.Create(target + ".tmp")
		if err != nil {
			resp.Body.Close()
			return err
		}
		_, err = io.Copy(file, resp.Body)
		closeErr := file.Close()
		resp.Body.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = os.Rename(target+".tmp", target); err != nil {
			return err
		}
	}
	return nil
}
