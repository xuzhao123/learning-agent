package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"time"

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
}

func newEmbedder(provider, cache string) (*embedder, error) {
	switch provider {
	case "ark":
		config, err := loadConfig()
		if err != nil {
			return nil, err
		}
		return &embedder{Model: arkModel, Dimensions: 1024,
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
		return &embedder{Model: modelName, Dimensions: 384, Key: modelName + revision + "hugot-v0.7.0/mean-normalized",
			Close: func() { _ = session.Destroy() }, Encode: func(ctx context.Context, text string, _ bool) ([]float32, error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				n := len(pipeline.Model.Tokenizer.GoTokenizer.Tokenizer.EncodeWithAnnotations(text).IDs)
				if n > tokenLimit {
					return nil, fmt.Errorf("文本有%d个模型token，超过%d，请按语义拆分或缩短查询", n, tokenLimit)
				}
				out, err := pipeline.RunPipeline([]string{text})
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
