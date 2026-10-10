package handler

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"github.com/memory-overflow/flowagent/pkg/llm"
	"github.com/memory-overflow/flowagent/pkg/llmreg"
	"gorm.io/gorm"
)

// ============================================================
// 全局模型配置 —— 把原先写死在 config.yaml llm 块的模型开放到前端增删改
//
// 协议四选一：openai（OpenAI 兼容 /v1）、gemini（Gemini 网关）、
// gpt（Azure OpenAI，/deployments/<model>）、claude（Anthropic Messages API）。
// 对话通过 conversation.model_id 引用这里的模型；未显式指定时回落到
// is_default=1 的默认模型。
// ============================================================

// --- 请求 / 响应结构体 ---

type ModelConfigItem struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	BaseURL    string `json:"base_url"`
	APIKey     string `json:"api_key"`
	Model      string `json:"model"`
	APIVersion string `json:"api_version,omitempty"`
	IsDefault  bool   `json:"is_default"`
	Enabled    bool   `json:"enabled"`
	// SupportsImage 是否支持图像理解（多模态），由带图探测回填。
	SupportsImage bool `json:"supports_image"`
	// ContextLimit 最大输入 token 数（上下文上限）。0 表示未配置，
	// 运行时回落到保守默认值。
	ContextLimit int   `json:"context_limit"`
	CreateTs     int64 `json:"create_ts"`
	UpdateTs     int64 `json:"update_ts"`
}

type ListModelConfigsResponse struct {
	List []*ModelConfigItem `json:"list"`
}

// ModelStatusResponse 「是否有可用模型」的轻量状态。
//
// 刻意不带任何模型明细：前端在应用入口就要问这个状态（早于用户进入模型页），
// 返回明细等于把 api_key 无条件透给所有会话，属于没必要的信息泄漏面。
type ModelStatusResponse struct {
	// HasAvailable 是否存在可用模型（enabled = true 且未删除）。
	HasAvailable bool `json:"has_available"`
	// Total 模型配置总数（含已停用的）。
	Total int64 `json:"total"`
	// AvailableCount 可用（已启用）模型数。
	AvailableCount int64 `json:"available_count"`
}

type CreateModelConfigRequest struct {
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	BaseURL    string `json:"base_url"`
	APIKey     string `json:"api_key"`
	Model      string `json:"model"`
	APIVersion string `json:"api_version,omitempty"`
	// SupportsImage 是否支持图像理解。由前端「测试」探测后回传（指针区分未传/显式 false）。
	SupportsImage *bool `json:"supports_image,omitempty"`
	// ContextLimit 最大输入 token 数。未传或为 0 时运行时回落到保守默认值。
	ContextLimit int `json:"context_limit,omitempty"`
	// Enabled 用指针区分「未传」（默认 true）与「传 false」（禁用）
	Enabled *bool `json:"enabled,omitempty"`
}

type UpdateModelConfigRequest struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Protocol   string `json:"protocol,omitempty"`
	BaseURL    string `json:"base_url,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
	Model      string `json:"model,omitempty"`
	APIVersion string `json:"api_version,omitempty"`
	// SupportsImage 是否支持图像理解（指针区分未传/显式 false）。
	SupportsImage *bool `json:"supports_image,omitempty"`
	// ContextLimit 最大输入 token 数（指针区分未传/显式清零）。
	ContextLimit *int `json:"context_limit,omitempty"`
	// Enabled 用指针区分「未传」与「传 false」
	Enabled *bool `json:"enabled,omitempty"`
}

type DeleteModelConfigRequest struct {
	ID string `json:"id"`
}

type SetDefaultModelRequest struct {
	ID string `json:"id"`
}

type TestModelRequest struct {
	// ID 指定则测数据库里已保存的那条；否则用下方字段直连
	ID string `json:"id,omitempty"`
	// 直连字段（ID 为空时使用）
	Protocol   string `json:"protocol"`
	BaseURL    string `json:"base_url"`
	APIKey     string `json:"api_key"`
	Model      string `json:"model"`
	APIVersion string `json:"api_version,omitempty"`
}

type TestModelResponse struct {
	Success  bool   `json:"success"`
	ErrorMsg string `json:"error_msg,omitempty"`
	// Reply 连通性测试的模型回显（如 "pong" / 模型返回的第一句话）
	Reply string `json:"reply,omitempty"`
	// SupportsImage 带图探测结果：模型是否支持图像理解（多模态）。
	// 连通性失败时该字段无意义。
	SupportsImage bool `json:"supports_image"`
	// VisionErrorMsg 带图探测失败时的错误信息（供用户判断是否真不支持、还是网络抖动）。
	VisionErrorMsg string `json:"vision_error_msg,omitempty"`
}

// ============================================================
// Handler 函数
// ============================================================

// ListModelConfigs 列出全部模型配置。
func ListModelConfigs(ctx *gin.Context, req interface{}) (interface{}, error) {
	modelDAO := &dao.ModelConfigDAO{}
	models, err := modelDAO.ListModels(ctx)
	if err != nil {
		return nil, err
	}

	list := make([]*ModelConfigItem, 0, len(models))
	for _, m := range models {
		list = append(list, buildModelConfigItem(m))
	}
	return &ListModelConfigsResponse{List: list}, nil
}

func ListModelConfigsBinder() interface{} { return &struct{}{} }

// ModelStatus 查询当前是否存在可用模型。
//
// 供前端在应用入口判断要不要弹「必须先配置模型」的引导。判定口径与
// ensureAvailableModel 保持一致（enabled = true 即算可用），避免
// 「引导说没配、执行说有」这类前后端口径打架。
func ModelStatus(ctx *gin.Context, req interface{}) (interface{}, error) {
	modelDAO := &dao.ModelConfigDAO{}

	total, err := modelDAO.Count(ctx)
	if err != nil {
		return nil, err
	}
	available, err := modelDAO.CountAvailable(ctx)
	if err != nil {
		return nil, err
	}

	return &ModelStatusResponse{
		HasAvailable:   available > 0,
		Total:          total,
		AvailableCount: available,
	}, nil
}

func ModelStatusBinder() interface{} { return &struct{}{} }

// ensureAvailableModel 校验当前存在可用模型，否则返回带动作指引的错误。
//
// 前端的入口引导只是软拦截 —— 绕过前端直接调接口（脚本、旧页面）仍会
// 建出任务，一直到执行期解析模型才失败，报的是「解析模型失败」这类内部
// 口径，用户看不懂也不知道该干什么。这里在入口前置拦截，直接告诉用户
// 下一步该做什么。
func ensureAvailableModel(ctx *gin.Context) error {
	n, err := (&dao.ModelConfigDAO{}).CountAvailable(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	return &APIError{
		Code: http.StatusBadRequest,
		Msg:  "尚未配置可用模型，请先在「模型配置」中添加并启用一个模型",
	}
}

// CreateModelConfig 新增模型配置。
func CreateModelConfig(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*CreateModelConfigRequest)

	if err := validateModelFields(r.Name, r.Protocol, r.BaseURL, r.Model); err != nil {
		return nil, err
	}

	modelDAO := &dao.ModelConfigDAO{}

	// 首个模型自动设为默认，保证「删光后重建」也能有默认模型兜底
	isDefault := false
	if n, err := modelDAO.Count(ctx); err == nil && n == 0 {
		isDefault = true
	}

	enabled := true
	if r.Enabled != nil {
		enabled = *r.Enabled
	}
	supportsImage := false
	if r.SupportsImage != nil {
		supportsImage = *r.SupportsImage
	}

	now := time.Now()
	m := &model.ModelConfig{
		ID:            uuid.New().String(),
		Name:          r.Name,
		Protocol:      model.ModelProtocol(r.Protocol),
		BaseURL:       r.BaseURL,
		APIKey:        r.APIKey,
		Model:         r.Model,
		APIVersion:    r.APIVersion,
		IsDefault:     isDefault,
		Enabled:       enabled,
		SupportsImage: supportsImage,
		ContextLimit:  r.ContextLimit,
		CreatedAt:     now,
		UpdatedAt:     now,
	}

	if err := modelDAO.CreateModel(ctx, m); err != nil {
		return nil, err
	}
	return buildModelConfigItem(m), nil
}

func CreateModelConfigBinder() interface{} { return &CreateModelConfigRequest{} }

// UpdateModelConfig 更新模型配置（只改请求里非空的字段）。
func UpdateModelConfig(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*UpdateModelConfigRequest)

	modelDAO := &dao.ModelConfigDAO{}
	existing, err := modelDAO.GetByID(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "模型配置不存在"}
	}

	updates := map[string]interface{}{}
	if r.Name != "" {
		updates["name"] = r.Name
	}
	if r.Protocol != "" {
		if !model.ModelProtocol(r.Protocol).Valid() {
			return nil, &APIError{Code: http.StatusBadRequest, Msg: "protocol 必须为 openai / gemini / gpt / claude"}
		}
		updates["protocol"] = r.Protocol
	}
	if r.BaseURL != "" {
		updates["base_url"] = r.BaseURL
	}
	if r.APIKey != "" {
		updates["api_key"] = r.APIKey
	}
	if r.Model != "" {
		updates["model"] = r.Model
	}
	if r.APIVersion != "" {
		updates["api_version"] = r.APIVersion
	}
	if r.Enabled != nil && !*r.Enabled && existing.IsDefault {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "默认模型不可停用"}
	}
	if r.Enabled != nil {
		updates["enabled"] = *r.Enabled
	}
	if r.SupportsImage != nil {
		updates["supports_image"] = *r.SupportsImage
	}
	if r.ContextLimit != nil {
		if *r.ContextLimit < 0 {
			return nil, &APIError{Code: http.StatusBadRequest, Msg: "context_limit 不能为负数"}
		}
		updates["context_limit"] = *r.ContextLimit
	}
	updates["updated_time"] = time.Now()

	if err := modelDAO.UpdateModel(ctx, r.ID, updates); err != nil {
		return nil, err
	}

	updated, _ := modelDAO.GetByID(ctx, r.ID)
	if updated != nil {
		return buildModelConfigItem(updated), nil
	}
	return nil, nil
}

func UpdateModelConfigBinder() interface{} { return &UpdateModelConfigRequest{} }

// DeleteModelConfig 删除模型配置（软删除）。
func DeleteModelConfig(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*DeleteModelConfigRequest)

	modelDAO := &dao.ModelConfigDAO{}
	if err := modelDAO.DeleteModel(ctx, r.ID); err != nil {
		return nil, err
	}
	return nil, nil
}

func DeleteModelConfigBinder() interface{} { return &DeleteModelConfigRequest{} }

// SetDefaultModel 设为默认模型（互斥：清掉其它 is_default，同时强制启用）。
func SetDefaultModel(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*SetDefaultModelRequest)

	modelDAO := &dao.ModelConfigDAO{}
	existing, err := modelDAO.GetByID(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "模型配置不存在"}
	}

	// 清默认 + 设默认 + 启用，放在一个事务里保证「同一时刻至多一个默认」。
	if err := dao.Transaction(func(tx *gorm.DB) error {
		txDAO := &dao.ModelConfigDAO{Tx: tx}
		if err := txDAO.ClearDefault(ctx, r.ID); err != nil {
			return err
		}
		return txDAO.UpdateModel(ctx, r.ID, map[string]interface{}{
			"is_default":   true,
			"enabled":      true,
			"updated_time": time.Now(),
		})
	}); err != nil {
		return nil, err
	}

	updated, _ := modelDAO.GetByID(ctx, r.ID)
	if updated != nil {
		return buildModelConfigItem(updated), nil
	}
	return nil, nil
}

func SetDefaultModelBinder() interface{} { return &SetDefaultModelRequest{} }

// TestModel 连通性测试：用短超时调一次 Chat，确认网关可达、凭据有效；
// 连通性通过后再用一张极小 PNG 探测模型是否支持图像理解（多模态）。
func TestModel(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*TestModelRequest)

	spec, err := resolveTestModelSpec(ctx, r)
	if err != nil {
		return &TestModelResponse{Success: false, ErrorMsg: err.Error()}, nil
	}

	// 探测超时：推理类模型（如混元 hy4-preview 带 reasoning_content 思考链）
	// 在复杂 prompt 下首包/完整回复可能超过十几秒，15s 单次超时容易误判为超时。
	// 取 60s 单次、150s 总预算（连通性 + 多模态探测两次调用），兼顾「不可达网关
	// 不长时间挂起」与「推理模型慢响应」。
	spec.Timeout = 60 * time.Second
	client := llmreg.BuildClient(*spec)

	// 连通性 + 多模态探测共用 150s 总预算（单次调用 60s 超时）
	testCtx, cancel := context.WithTimeout(ctx, 150*time.Second)
	defer cancel()

	// 1. 连通性测试
	reply, err := client.Chat(testCtx, []llm.Message{
		{Role: llm.RoleUser, Content: "只回复一个词：pong"},
	}, llm.DefaultOptions())
	if err != nil {
		return &TestModelResponse{Success: false, ErrorMsg: err.Error()}, nil
	}

	// 2. 多模态探测
	supportsImage, visionErr := probeVision(testCtx, client)

	// 3. 按 ID 测试时，把探测结果回填到 DB，供对话层据此决定是否内置图像分析能力。
	// 回填失败不阻断结果返回（测试本身已成功）。
	if r.ID != "" {
		if uErr := (&dao.ModelConfigDAO{}).UpdateModel(ctx, r.ID, map[string]interface{}{
			"supports_image": supportsImage,
			"updated_time":   time.Now(),
		}); uErr != nil {
			log.Printf("[test-model] 回填 supports_image 失败 (id=%s): %v", r.ID, uErr)
		}
	}

	resp := &TestModelResponse{Success: true, Reply: reply, SupportsImage: supportsImage}
	if visionErr != "" {
		resp.VisionErrorMsg = visionErr
	}
	return resp, nil
}

func TestModelBinder() interface{} { return &TestModelRequest{} }

// resolveTestModelSpec 优先按 ID 查 DB，否则用请求里的直连字段。
func resolveTestModelSpec(ctx context.Context, r *TestModelRequest) (*llmreg.ModelSpec, error) {
	if r.ID != "" {
		m, err := (&dao.ModelConfigDAO{}).GetByID(ctx, r.ID)
		if err != nil {
			return nil, fmt.Errorf("查询模型配置失败: %v", err)
		}
		if m == nil {
			return nil, fmt.Errorf("模型配置不存在 (id=%s)", r.ID)
		}
		return modelToModelSpec(m), nil
	}

	if err := validateModelFields("test", r.Protocol, r.BaseURL, r.Model); err != nil {
		return nil, err
	}
	return &llmreg.ModelSpec{
		Protocol:   r.Protocol,
		BaseURL:    r.BaseURL,
		APIKey:     r.APIKey,
		Model:      r.Model,
		APIVersion: r.APIVersion,
	}, nil
}

// probeImageBase64 一张 64x64 白底黑字「7」的 PNG，用于探测模型是否支持图像理解。
//
// 为什么用「数字识别」而非旧版的「1x1 红图 + 请回复"可以"」：
//   - 旧 prompt 把期望答案（"可以"）直接写进了指令，纯文本模型（看不到图）也会
//     严格照做回"可以"，请求又返回 200，于是被误判为「支持多模态」。
//   - 混元 hy3 实测：网关（lkeap plan/v3）会把 image_url 静默丢弃，模型明确表示
//     「没有收到任何图片」，但只要 prompt 让它回"可以"它照样回，导致 supports_image 误置 1。
//   - 改成 OCR 数字题后，答案不在 prompt 里：真多模态模型能稳定读出「7」，
//     而看不到图的模型只能瞎猜或坦白「未提供图片」，判定结果可靠。
const probeImageBase64 = "iVBORw0KGgoAAAANSUhEUgAAAEAAAABACAIAAAAlC+aJAAAAXUlEQVR42u3PwQ0AIAzEsO6/NEwADwTVITkTxDXWVUZjWwEAAAAAAAQEAAAAAACQDzi+BAAAAAAAaAO8uAQAAAAAALgLaL4EAAAAAAAAAAAAAAAACAwAAAAAAODrJnehXbnxQwHnAAAAAElFTkSuQmCC"

// probeImageAnswer 探测图里画的数字，模型回复需包含它才判定支持图像理解。
const probeImageAnswer = "7"

// probeVision 用一张白底黑字「7」的 PNG 探测模型是否支持图像理解（多模态）。
//
// 返回 (是否支持, 错误信息)。判定标准：请求成功 **且** 模型回复里正确识别出
// 「7」才判支持；请求报错、或回复不含「7」（图片被网关丢弃 / 模型无视觉能力），
// 都判不支持，并把模型回复/错误原样返回供展示，让用户能区分
// 「模型确实不支持」与「网关对图片有其他限制」。
func probeVision(ctx context.Context, client llm.Client) (bool, string) {
	msg := llm.Message{
		Role:    llm.RoleUser,
		Content: "图片里画的是哪个数字？只回答这个数字本身，不要解释",
		Images:  []llm.ImagePart{{MimeType: "image/png", Data: probeImageBase64}},
	}
	reply, err := client.Chat(ctx, []llm.Message{msg}, llm.DefaultOptions())
	if err != nil {
		return false, err.Error()
	}
	if strings.Contains(reply, probeImageAnswer) {
		return true, ""
	}
	return false, fmt.Sprintf("模型未正确识别图片内容（回复: %q），判定为不支持图像理解", truncateRunes(reply, 200))
}

// truncateRunes 按 rune 截断字符串，避免超长回复撑爆错误信息展示。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// ============================================================
// 辅助函数
// ============================================================

func buildModelConfigItem(m *model.ModelConfig) *ModelConfigItem {
	return &ModelConfigItem{
		ID:            m.ID,
		Name:          m.Name,
		Protocol:      string(m.Protocol),
		BaseURL:       m.BaseURL,
		APIKey:        m.APIKey,
		Model:         m.Model,
		APIVersion:    m.APIVersion,
		IsDefault:     m.IsDefault,
		Enabled:       m.Enabled,
		SupportsImage: m.SupportsImage,
		ContextLimit:  m.ContextLimit,
		CreateTs:      m.CreatedAt.UnixMilli(),
		UpdateTs:      m.UpdatedAt.UnixMilli(),
	}
}

// modelToModelSpec 把 model.ModelConfig 转成 llmreg.ModelSpec。
func modelToModelSpec(m *model.ModelConfig) *llmreg.ModelSpec {
	return &llmreg.ModelSpec{
		ID:         m.ID,
		Protocol:   string(m.Protocol),
		BaseURL:    m.BaseURL,
		APIKey:     m.APIKey,
		Model:      m.Model,
		APIVersion: m.APIVersion,
	}
}

// validateModelFields 校验创建/直连测试的必填字段与协议取值。
func validateModelFields(name, protocol, baseURL, modelName string) error {
	if name == "" {
		return &APIError{Code: http.StatusBadRequest, Msg: "name 不能为空"}
	}
	if !model.ModelProtocol(protocol).Valid() {
		return &APIError{Code: http.StatusBadRequest, Msg: "protocol 必须为 openai / gemini / gpt / claude"}
	}
	if baseURL == "" {
		return &APIError{Code: http.StatusBadRequest, Msg: "base_url 不能为空"}
	}
	if modelName == "" {
		return &APIError{Code: http.StatusBadRequest, Msg: "model 不能为空"}
	}
	return nil
}
