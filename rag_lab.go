package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// 固定的是问题与人工评阅依据，不是模型回答或工具调用顺序。
// 每题独立上下文，避免前题答案泄漏；两组共享回答标准、模型、推理强度和预算。
// 系统规则如实说明本轮是否提供检索工具，避免无工具组伪造调用文本。
func runRAGLab(ctx context.Context, config modelConfig, options contextOptions, maxSteps int) error {
	var questions []struct {
		ID          string   `json:"id"`
		Question    string   `json:"question"`
		RelevantIDs []string `json:"relevant_ids"`
	}
	data, err := os.ReadFile("retrieval/questions.json")
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &questions); err != nil {
		return err
	}
	hitCount, answerable := 0, 0
	for _, q := range questions {
		result, err := searchDocs(ctx, q.Question, 3)
		if err != nil {
			return err
		}
		hit := false
		for _, candidate := range result.Hits {
			for _, want := range q.RelevantIDs {
				if candidate.ID == want && candidate.Accepted {
					hit = true
				}
			}
			fmt.Printf("Retrieval %s: id=%s score=%.4f accepted=%t\n", q.ID, candidate.ID, candidate.Score, candidate.Accepted)
		}
		if len(q.RelevantIDs) > 0 {
			answerable++
			if hit {
				hitCount++
			}
		}
		fmt.Printf("Retrieval %s: relevant_hit=%t status=%s corpus=%s\n", q.ID, hit, result.Status, result.Corpus)
	}
	fmt.Printf("Retrieval summary: hit@3=%d/%d answerable; total=%d (unanswerable=%d)\n", hitCount, answerable, len(questions), len(questions)-answerable)
	ragEnabled = true
	var failed []string
	for _, q := range questions {
		for _, mode := range []string{"baseline", "rag"} {
			// 对照组没有任何文档工具，避免原 search_notes 泄漏项目事实。
			toolDefinitions = nil
			if mode == "rag" {
				toolDefinitions = []map[string]any{searchDocsDefinition}
			}
			fmt.Printf("\nRAG case: %s mode=%s question=%s\n", q.ID, mode, q.Question)
			// 单题超预算或网络失败也是评测结果：记录后继续，不丢掉其余题目。
			if err := runAgent(ctx, config, q.Question, 4, maxSteps, 2, options, nil); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				fmt.Printf("RAG case failed: %s mode=%s error=%s\n", q.ID, mode, strings.ReplaceAll(err.Error(), "\n", " "))
				failed = append(failed, q.ID+"/"+mode)
			}
		}
	}
	fmt.Printf("\nRAG lab complete: cases=%d failed=%d %s\n", len(questions)*2, len(failed), strings.Join(failed, " "))
	fmt.Println("请按 questions.json 的依据人工核对答案正确性、有效引用和无依据断言；引用数量不等于正确率，失败题按未答对计。")
	if len(failed) > 0 {
		return fmt.Errorf("%d个用例未正常完成：%s", len(failed), strings.Join(failed, " "))
	}
	return nil
}
