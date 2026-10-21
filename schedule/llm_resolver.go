package schedule

import (
	"context"

	"github.com/memory-overflow/flowagent/config"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
	"github.com/memory-overflow/flowagent/pkg/llmreg"
)

// modelConfigLoader 把 dao.ModelConfigDAO 适配成 llmreg.ModelLoader。
type modelConfigLoader struct {
	dao *dao.ModelConfigDAO
}

func (l *modelConfigLoader) LoadSpec(ctx context.Context, id string) (*llmreg.ModelSpec, error) {
	m, err := l.dao.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, nil
	}
	return modelToSpec(m), nil
}

func (l *modelConfigLoader) LoadDefaultSpec(ctx context.Context) (*llmreg.ModelSpec, error) {
	m, err := l.dao.GetDefault(ctx)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return nil, nil
	}
	return modelToSpec(m), nil
}

// modelToSpec 把 model.ModelConfig 转成 llmreg.ModelSpec。
func modelToSpec(m *model.ModelConfig) *llmreg.ModelSpec {
	return &llmreg.ModelSpec{
		ID:           m.ID,
		Protocol:     string(m.Protocol),
		BaseURL:      m.BaseURL,
		APIKey:       m.APIKey,
		Model:        m.Model,
		APIVersion:   m.APIVersion,
		ContextLimit: m.ContextLimit,
	}
}

// buildFallbackClient 从 config.LLM 构建回退客户端。
//
// 保留 config.yaml 的 llm 块作为「DB 里没有任何模型配置」时的兜底，
// 让未做前端配置的部署照常运行（行为与改造前一致）。
// provider 取值沿用 config 的旧口径 gemini/openai/azure，其中 azure 映射到
// llmreg 的 gpt 协议。
func fallbackModelSpec(cfg *config.Config) llmreg.ModelSpec {
	return llmreg.ModelSpec{
		Protocol:   cfg.LLM.ModelProtocol(),
		BaseURL:    cfg.LLM.BaseURL,
		APIKey:     cfg.LLM.APIKey,
		Model:      cfg.LLM.Model,
		APIVersion: cfg.LLM.APIVersion,
	}
}

func buildFallbackClient(cfg *config.Config) llm.Client {
	// provider→protocol 映射统一走 config.LLMConfig.ModelProtocol()，
	// 与 server 侧「种子默认模型」共用同一份口径（azure → gpt）。
	return llmreg.BuildClient(fallbackModelSpec(cfg))
}
