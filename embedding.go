package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"
)

const arkModel = "doubao-embedding-vision-251215"
const arkURL = "https://ark.cn-beijing.volces.com/api/v3/embeddings/multimodal"
const documentInstruction = "Target_modality: text.\nInstruction:Compress the text into one word.\nQuery:"
const queryInstruction = "Target_modality: text.\nInstruction:根据这个问题，找到能回答这个问题的相应文本。\nQuery:"

type embedder struct {
	Model, Key string
	Dimensions int
	Encode     func(context.Context, string, bool) ([]float32, error) // bool区分query和入库文档。
	Close      func()
	// Day 5 切块用：Count 按该模型的输入单位计数，Limit 是单条输入上限。
	Count func(string) int
	Limit int
}

// 同一进程只加载一次向量模型：search_docs 与长期记忆共用，进程退出时由 closeEmbedders 释放。
var embedders = map[string]*embedder{}
var embedderErrors = map[string]error{}
var embeddersMu sync.Mutex

func sharedEmbedder(provider string) (*embedder, error) {
	embeddersMu.Lock()
	defer embeddersMu.Unlock()
	if e := embedders[provider]; e != nil {
		return e, nil
	}
	if err := embedderErrors[provider]; err != nil {
		return nil, err // 本地模型加载失败时不在每次检索里反复重试
	}
	cache := filepath.Join(".cache", "retrieval-go")
	if err := os.MkdirAll(cache, 0700); err != nil {
		return nil, err
	}
	e, err := newEmbedder(provider, cache)
	if err != nil {
		embedderErrors[provider] = err
		return nil, err
	}
	embedders[provider] = e
	return e, nil
}

func closeEmbedders() {
	embeddersMu.Lock()
	defer embeddersMu.Unlock()
	for name, e := range embedders {
		e.Close()
		delete(embedders, name)
	}
}

func newEmbedder(provider, cache string) (*embedder, error) {
	switch provider {
	case "ark":
		config, err := loadConfig()
		if err != nil {
			return nil, err
		}
		// 方舟单条输入上限远大于记忆片段；本项目未实测上限，Count 用字符数作保守估计。
		return &embedder{Model: arkModel, Dimensions: 1024, Limit: 4096, Count: utf8.RuneCountInString,
			Key:   arkURL + arkModel + "1024/dense-only/normalized/" + documentInstruction + queryInstruction,
			Close: func() {}, Encode: func(ctx context.Context, text string, query bool) ([]float32, error) {
				return arkEmbedding(ctx, config.APIKey, text, query)
			}}, nil
	case "local":
		path := filepath.Join(cache, "model")
		if err := downloadModel(path); err != nil {
			return nil, err
		}
		session, err := hugot.NewGoSession() // 纯Go CPU后端；CGO_ENABLED=0也可编译运行。
		if err != nil {
			return nil, err
		}
		pipeline, err := hugot.NewPipeline(session, hugot.FeatureExtractionConfig{
			ModelPath: path, Name: "docs", OnnxFilename: "model.onnx",
			Options: []hugot.FeatureExtractionOption{pipelines.WithNormalization()},
		})
		if err != nil {
			session.Destroy()
			return nil, err
		}
		// 本地 pipeline 不保证并发安全：推理与分词都串行；线上 embedding 请求可并行。
		var mu sync.Mutex
		count := func(text string) int {
			mu.Lock()
			defer mu.Unlock()
			return len(pipeline.Model.Tokenizer.GoTokenizer.Tokenizer.EncodeWithAnnotations(text).IDs)
		}
		return &embedder{Model: modelName, Dimensions: 384, Key: modelName + revision + "hugot-v0.7.0/mean-normalized", Limit: tokenLimit, Count: count,
			Close: func() { mu.Lock(); defer mu.Unlock(); _ = session.Destroy() }, Encode: func(ctx context.Context, text string, _ bool) ([]float32, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				if n := count(text); n > tokenLimit {
					return nil, fmt.Errorf("文本有%d个模型token，超过%d，请按语义拆分或缩短查询", n, tokenLimit)
				}
				mu.Lock()
				out, err := pipeline.RunPipeline([]string{text})
				mu.Unlock()
				if err != nil {
					return nil, err
				}
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return normalize(out.Embeddings[0], 384)
			}}, nil
	default:
		return nil, fmt.Errorf("embedding必须为ark或local")
	}
}

func arkEmbedding(ctx context.Context, key, text string, query bool) ([]float32, error) {
	instruction := documentInstruction
	if query {
		instruction = queryInstruction
	}
	// multimodal 的 input 是一次融合输入，不是多条独立文档的batch；每份文档单独请求。
	// 只请求用到的稠密向量；开启multi/sparse会让响应从约16KB变成约2MB，耗时约翻倍。
	body, _ := json.Marshal(map[string]any{
		"model": arkModel, "instructions": instruction,
		"input":      []map[string]string{{"type": "text", "text": text}},
		"dimensions": 1024, "encoding_format": "float",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, arkURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	client := &http.Client{Timeout: 25 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("方舟embedding请求失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 错误响应体含方舟错误码与说明，不含密钥；截断后带出便于排查。
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("方舟embedding返回HTTP %d：%s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	var result struct {
		Data struct {
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("无法解析方舟embedding响应")
	}
	// 当前课程只用data.embedding稠密向量；请求开启的多向量/稀疏向量不冒充已做混合检索。
	return normalize(result.Data.Embedding, 1024)
}

func normalize(vector []float32, dimensions int) ([]float32, error) {
	if len(vector) != dimensions {
		return nil, fmt.Errorf("向量维度为%d，预期%d", len(vector), dimensions)
	}
	var norm float64
	for _, x := range vector {
		norm += float64(x) * float64(x)
	}
	if norm == 0 || math.IsNaN(norm) || math.IsInf(norm, 0) {
		return nil, fmt.Errorf("向量范数无效")
	}
	norm = math.Sqrt(norm)
	for i := range vector {
		vector[i] /= float32(norm)
	}
	return vector, nil
}
