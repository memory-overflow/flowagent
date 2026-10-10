package model

import (
	"time"

	"gorm.io/gorm"
)

// ============================================================
// ModelProtocol — 模型接入协议枚举
// ============================================================

// ModelProtocol 模型接入协议。四种取值：
//
//   - openai：OpenAI 兼容协议（/v1/chat/completions，如腾讯混元 tokenhub）
//   - gemini：Gemini 原生协议（走内部网关 :streamGenerateContent）
//   - gpt    ：Azure OpenAI 协议（/deployments/<model>/chat/completions?api-version=，
//     承载 gpt-5.6 系列 sol/terra/luna 等）
//   - claude ：Anthropic Messages API（/v1/messages，x-api-key 鉴权，
//     如腾讯云 Token Plan 的 /plan/anthropic 入口）
type ModelProtocol string

const (
	ModelProtocolOpenAI ModelProtocol = "openai"
	ModelProtocolGemini ModelProtocol = "gemini"
	ModelProtocolGPT    ModelProtocol = "gpt"
	ModelProtocolClaude ModelProtocol = "claude"
)

// Valid 判断协议取值是否合法。
func (p ModelProtocol) Valid() bool {
	switch p {
	case ModelProtocolOpenAI, ModelProtocolGemini, ModelProtocolGPT, ModelProtocolClaude:
		return true
	}
	return false
}

// ============================================================
// ModelConfig — 全局模型配置表
// ============================================================

// ModelConfig 一条可被对话选用的模型配置。
//
// 由前端在「模型」页增删改，替代原先写死在 config.yaml llm 块的单例配置。
// 各字段复用 config.LLMConfig 的语义（base_url 层级约定、api_version 仅 gpt 使用等）。
type ModelConfig struct {
	ID         string        `gorm:"column:id;size:100;primaryKey" json:"id"`
	Name       string        `gorm:"column:name;size:255;not null" json:"name"` // 展示名，如 "GPT-5.6 Sol"
	Protocol   ModelProtocol `gorm:"column:protocol;size:20;not null" json:"protocol"`
	BaseURL    string        `gorm:"column:base_url;size:1024" json:"base_url"`
	APIKey     string        `gorm:"column:api_key;size:512" json:"api_key"`
	Model      string        `gorm:"column:model;size:255" json:"model"` // 模型名，如 gpt-5.6-sol / gemini-3.1-pro-preview / hy3
	APIVersion string        `gorm:"column:api_version;size:64" json:"api_version,omitempty"`
	// IsDefault 默认模型。对话未显式指定 model_id 时回落到它。
	// 同一时刻至多一条为 true，由 handler 在 set-default 时互斥维护。
	IsDefault bool `gorm:"column:is_default;not null;default:0" json:"is_default"`
	// Enabled 是否启用。禁用后不出现在选择器，也不会被解析使用。
	Enabled bool `gorm:"column:enabled;not null;default:1" json:"enabled"`
	// SupportsImage 模型是否支持图像理解（多模态输入）。
	// 由「测试模型」的带图探测自动回填；供对话层据此决定是否内置图像分析能力。
	SupportsImage bool `gorm:"column:supports_image;not null;default:0" json:"supports_image"`
	// ContextLimit 模型的最大输入 token 数（上下文上限）。
	//
	// 口径刻意定为「最大输入」而非「总窗口」：上下文管理只对输入侧做水位检查，
	// 而多数厂商的总窗口 = 输入上限 + 输出上限（如 gpt-5.6-sol 总 1.05M
	// = 输入 922K + 输出 128K）。若误存总窗口，会高估可用空间、压缩来不及触发。
	//
	// 新增模型时必须按官方文档填写。为 0 时视为未配置，由
	// contextmgr.ResolveContextLimit 回落到保守默认值（见该函数注释），
	// 保证漏配不会直接导致上下文管理失效。
	ContextLimit int            `gorm:"column:context_limit;not null;default:0" json:"context_limit"`
	CreatedAt    time.Time      `gorm:"column:created_time;not null" json:"created_time"`
	UpdatedAt    time.Time      `gorm:"column:updated_time;not null;autoUpdateTime" json:"updated_time"`
	DeletedAt    gorm.DeletedAt `gorm:"column:delete_time;index" json:"-"`
}

func (ModelConfig) TableName() string { return "model_config" }
