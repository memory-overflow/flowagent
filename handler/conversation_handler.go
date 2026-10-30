package handler

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	"gorm.io/gorm"
)

// ============================================================
// 请求 / 响应结构体（JSON 对应 proto 定义）
// ============================================================

// --- 创建对话 ---
type CreateConversationRequest struct {
	Title        string   `json:"title"`
	MCPServerIDs []string `json:"mcp_server_ids,omitempty"`
	SkillIDs     []string `json:"skill_ids,omitempty"`
	// ModelID 创建时可选指定使用的模型；为空则回落到默认模型
	ModelID string `json:"model_id,omitempty"`
}

type CreateConversationResponse struct {
	ChatID     string `json:"chat_id"`
	Title      string `json:"title"`
	CreateUser string `json:"create_user"`
	CreateTs   int64  `json:"create_ts"`
}

// --- 发送消息 ---
type ChatRequest struct {
	ChatID     string           `json:"chat_id"`
	InputText  string           `json:"input_text,omitempty"`
	InputFiles []model.FileInfo `json:"input_files,omitempty"`
	// ModelID 发送时可选切换对话使用的模型；非空则持久化到 conversation.model_id。
	// 为空表示沿用对话当前已选模型（不改动）。
	ModelID string `json:"model_id,omitempty"`
	// ForceNewWorkflow 用户显式要求「新开一个工作流」完成本轮任务。
	//
	// 一个对话可以绑定多个工作流，规划期默认由模型在候选清单里选
	// 「续接已有工作流」还是「新开工作流」。这个开关是给用户兜底的显式入口：
	// 用户最清楚本轮任务和之前那堆 DAG 有没有关系，所以用户判断优先于模型判断
	// ——置 true 时规划期无条件新开，不再把已有工作流放进候选。
	ForceNewWorkflow bool `json:"force_new_workflow,omitempty"`
}

type ChatResponse struct {
	TaskID string `json:"task_id"`
}

// --- 对话列表 ---
type ListConversationsRequest struct {
	LastTs   int64  `json:"last_ts,omitempty"`
	Keyword  string `json:"keyword,omitempty"`
	PageSize int    `json:"page_size,omitempty"`
}

type ConversationInfo struct {
	ChatID     string                      `json:"chat_id"`
	Title      string                      `json:"title"`
	Status     int                         `json:"status"`
	CreateUser string                      `json:"create_user"`
	CreateTs   int64                       `json:"create_ts"`
	UpdateTs   int64                       `json:"update_ts"`
	TaskCount  int                         `json:"task_count"`
	Preview    string                      `json:"preview,omitempty"`
	MCPServers []ConversationMCPServerInfo `json:"mcp_servers,omitempty"`
	// WorkflowTaskID 本对话「当前激活」的工作流任务 ID，一个工作流都没有时为空。
	//
	// v1.4 起一个对话可以有多个工作流，本字段只表示前端默认展示的那一条，
	// 全集请以 conversations/detail 的 workflow_list 为准。
	WorkflowTaskID string `json:"workflow_task_id,omitempty"`
	// HasRunningTask 是否有正在执行中的任务或工作流（排队中 / 进行中）
	HasRunningTask bool `json:"has_running_task"`
	// HasFailedTask 是否有失败的任务或工作流（失败 / 部分失败）
	HasFailedTask bool `json:"has_failed_task"`
}

type ConversationMCPServerInfo struct {
	ServerID string `json:"server_id"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
}

type ListConversationsResponse struct {
	List    []*ConversationInfo `json:"list"`
	Total   int64               `json:"total"`
	HasMore bool                `json:"has_more"`
}

// --- 对话详情 ---
type GetConversationDetailRequest struct {
	ChatID string `json:"chat_id"`
	// Brief 轮询模式：只返回 chat_info（含工作流摘要），跳过对话任务详情渲染。
	// 供工作流页/对话页的工作流状态轮询使用 —— 轮询只需工作流状态与节点进度，
	// 对话任务的 think 重建与 token 统计是高频重活，跳过能显著降载。
	Brief bool `json:"brief,omitempty"`
}

// --- 删除对话 ---
type DeleteConversationRequest struct {
	ChatID string `json:"chat_id"`
}

// --- 对话 MCP 管理 ---
type ConversationMCPRequest struct {
	ChatID    string   `json:"chat_id"`
	ServerIDs []string `json:"server_ids,omitempty"`
}

type ConversationMCPItem struct {
	ServerID string `json:"server_id"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
}

type ConversationMCPListResponse struct {
	Items []*ConversationMCPItem `json:"items"`
}

// --- 对话 Skill 管理 ---
type ConversationSkillRequest struct {
	ChatID   string   `json:"chat_id"`
	SkillIDs []string `json:"skill_ids,omitempty"`
}

type ConversationSkillItem struct {
	SkillID string `json:"skill_id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type ConversationSkillListResponse struct {
	Items []*ConversationSkillItem `json:"items"`
}

// ============================================================
// Handler 函数
// ============================================================

// CreateConversation 创建对话
func CreateConversation(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*CreateConversationRequest)

	// 无可用模型时不给开新对话：否则用户先看到一个「已建好」的对话，
	// 发第一条消息才撞上执行期失败。理由见 ensureAvailableModel。
	if err := ensureAvailableModel(ctx); err != nil {
		return nil, err
	}

	chatID := uuid.New().String()
	now := time.Now()

	// 标题为空（前端新建对话时不传）时按界面语言补一个占位标题。
	// 占位文案由后端单点定义，首条输入才知道该不该覆盖（见 isDefaultConversationTitle）。
	title := strings.TrimSpace(r.Title)
	if title == "" {
		title = defaultConversationTitle(LangFromContext(ctx))
	}

	conv := &model.Conversation{
		ChatID:     chatID,
		Title:      title,
		Status:     model.ConversationStatusActive,
		CreateUser: "default", // TODO: 从 JWT 解析
		ModelID:    r.ModelID,
		CreatedAt:  now,
		UpdatedAt:  now,
	}

	convDAO := &dao.ConversationDAO{}
	if err := convDAO.CreateConversation(ctx, conv); err != nil {
		return nil, err
	}

	// 创建对话-MCP 关联
	if len(r.MCPServerIDs) > 0 {
		mcpDAO := &dao.ConversationMCPDAO{}
		if err := mcpDAO.AddConversationMCP(ctx, chatID, r.MCPServerIDs); err != nil {
			return nil, err
		}
	}

	// 创建对话-Skill 关联
	if len(r.SkillIDs) > 0 {
		skillDAO := &dao.ConversationSkillDAO{}
		if err := skillDAO.AddConversationSkill(ctx, chatID, r.SkillIDs); err != nil {
			return nil, err
		}
	}

	return &CreateConversationResponse{
		ChatID:     chatID,
		Title:      title,
		CreateUser: conv.CreateUser,
		CreateTs:   now.UnixMilli(),
	}, nil
}

// CreateConversationBinder 绑定 CreateConversation 请求参数
func CreateConversationBinder() interface{} {
	return &CreateConversationRequest{}
}

// Chat 发送消息（创建异步任务）
func Chat(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*ChatRequest)

	// 0. 校验存在可用模型（理由见 ensureAvailableModel）
	if err := ensureAvailableModel(ctx); err != nil {
		return nil, err
	}

	// 1. 校验对话是否存在
	convDAO := &dao.ConversationDAO{}
	conv, err := convDAO.GetConversationByID(ctx, r.ChatID)
	if err != nil {
		return nil, err
	}
	if conv == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "对话不存在"}
	}

	taskDAO := &dao.TaskDAO{}

	// 1.4 发送时可选切换模型：非空则持久化到 conversation.model_id，
	// 本次及后续任务都会按它解析 LLM 客户端。为空沿用对话当前已选模型。
	if r.ModelID != "" {
		if err := convDAO.UpdateConversationModel(ctx, r.ChatID, r.ModelID); err != nil {
			return nil, err
		}
	}

	// 1.5 首次输入自动命名：该对话尚无任何用户消息时，用输入前若干字符替换默认标题。
	//
	// 判断「首次输入」用 ListTasksByChatID —— 它只返回 TaskTypeConversation
	// （即用户消息），工作流任务 / 子任务不计入。首次发消息时这里必然为空。
	if title := deriveConversationTitle(r.InputText); title != "" {
		existing, err := taskDAO.ListTasksByChatID(ctx, r.ChatID)
		if err != nil {
			return nil, err
		}
		if len(existing) == 0 && isDefaultConversationTitle(conv.Title) {
			if err := convDAO.UpdateConversationTitle(ctx, r.ChatID, title); err != nil {
				return nil, err
			}
		}
	}

	// 2. 构建任务输入
	input := &model.TaskInputData{
		InputText:  r.InputText,
		InputFiles: r.InputFiles,
		// 用户显式要求新开工作流时透传给规划期，优先级高于模型自己的判断。
		// 注意这里改的是「本轮任务」的输入，不落库到 conversation ——
		// 激活态要等规划期真正选中目标工作流后才写入。
		ForceNewWorkflow: r.ForceNewWorkflow,
	}

	// 3. 创建任务（双写 task + task_temp）
	taskID := uuid.New().String()
	now := time.Now()
	task := &model.Task{
		TaskID: taskID,
		ChatID: r.ChatID,
		// 用户发的每条消息都是一条对话任务。
		// 若规划判定为 workflow，节点会并入对话绑定的工作流任务
		// （另一条 task_type=workflow 的记录），本任务本身仍是对话任务。
		TaskType:     model.TaskTypeConversation,
		TaskInput:    model.JsonField[*model.TaskInputData]{Data: input},
		Stage:        int(model.StageFunctionalCallWait), // 初始阶段：等待 functional_call 调度器认领
		Phase:        int(model.PhasePlanning),           // 初始语义阶段：规划期（可调工具，最终 submit_plan）
		Status:       model.TaskStatusQueued,
		Progress:     0,
		AttemptTimes: 0,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	if err := dao.Transaction(func(tx *gorm.DB) error {
		taskDAO.Tx = tx
		return taskDAO.CreateTask(ctx, task)
	}); err != nil {
		return nil, err
	}

	return &ChatResponse{TaskID: taskID}, nil
}

// ChatBinder 绑定 Chat 请求参数
func ChatBinder() interface{} {
	return &ChatRequest{}
}

// defaultConversationTitles 各界面语言下新建对话的占位标题。
//
// 前端创建对话时不传 title，由后端按 X-Lang 选一个落库 —— 占位文案由后端
// 单点定义，前端只负责展示，不会出现「前端传的文案后端认不出」的漂移
// （历史上前端硬编码过「新对话」，i18n 后又按语言传「新建对话」/「New chat」，
// 后端只认「新对话」，首条输入便不再改写标题，列表里挂着一串占位标题）。
var defaultConversationTitles = map[string]string{
	"zh": "新对话",
	"en": "New Chat",
}

// defaultConversationTitle 按界面语言返回占位标题。
func defaultConversationTitle(lang string) string {
	if v, ok := defaultConversationTitles[lang]; ok {
		return v
	}
	return defaultConversationTitles["zh"]
}

// placeholderConversationTitles 占位标题的全部形态：当前语言版本 + 历史版本写入过的。
// 只用于判定「标题是否仍是占位」，存量数据里的旧形态也要能被首条输入覆盖。
var placeholderConversationTitles = []string{"新对话", "新建对话", "New Chat", "New chat", "未命名对话", "Untitled"}

// isDefaultConversationTitle 判断标题是否仍是占位形态（空也算占位）。
// 只有占位标题才允许被首次输入覆盖，避免覆盖用户主动重命名过的标题。
func isDefaultConversationTitle(title string) bool {
	key := strings.ToLower(strings.TrimSpace(title))
	if key == "" {
		return true
	}
	for _, v := range placeholderConversationTitles {
		if strings.ToLower(v) == key {
			return true
		}
	}
	return false
}

// deriveConversationTitle 从用户输入提炼对话标题：压缩空白、取前 20 个字符。
// 输入为空或纯空白时返回空串，调用方据此跳过命名。
func deriveConversationTitle(input string) string {
	s := strings.Join(strings.Fields(input), " ")
	if s == "" {
		return ""
	}
	runes := []rune(s)
	const maxRunes = 20
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

// ListConversations 拉取对话列表
func ListConversations(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*ListConversationsRequest)

	pageSize := r.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}

	convDAO := &dao.ConversationDAO{}
	convs, total, err := convDAO.ListConversations(ctx, "default", r.LastTs, r.Keyword, pageSize)
	if err != nil {
		return nil, err
	}

	// 批量统计每个对话的运行中 / 失败任务，供列表渲染执行中 / 失败图标。
	// 顶层任务（对话任务 + 工作流任务）都挂在 chat_id 下，单条 GROUP BY 即可，
	// 无需逐对话查询。
	chatIDs := make([]string, 0, len(convs))
	for _, c := range convs {
		chatIDs = append(chatIDs, c.ChatID)
	}
	taskDAO := &dao.TaskDAO{}
	statusByChat, err := taskDAO.CountTopLevelTasksByStatusForChats(ctx, chatIDs)
	if err != nil {
		// 状态统计失败不阻断列表返回，仅降级为不显示图标
		log.Printf("[ListConversations] count task status failed: %v", err)
		statusByChat = map[string]map[model.TaskStatus]int64{}
	}

	// 构建响应
	list := make([]*ConversationInfo, 0, len(convs))
	for _, c := range convs {
		statusCount := statusByChat[c.ChatID]
		info := &ConversationInfo{
			ChatID:         c.ChatID,
			Title:          c.Title,
			Status:         int(c.Status),
			CreateUser:     c.CreateUser,
			CreateTs:       c.CreatedAt.UnixMilli(),
			UpdateTs:       c.UpdatedAt.UnixMilli(),
			WorkflowTaskID: c.WorkflowTaskID,
			HasRunningTask: statusCount[model.TaskStatusQueued] > 0 ||
				statusCount[model.TaskStatusRunning] > 0,
			HasFailedTask: statusCount[model.TaskStatusFailed] > 0 ||
				statusCount[model.TaskStatusPartialFailed] > 0,
		}
		list = append(list, info)
	}

	return &ListConversationsResponse{
		List:    list,
		Total:   total,
		HasMore: int64(len(convs)) >= int64(pageSize),
	}, nil
}

// ListConversationsBinder 绑定 ListConversations 请求参数
func ListConversationsBinder() interface{} {
	return &ListConversationsRequest{}
}

// GetConversationDetail 获取对话详情（含任务列表）
func GetConversationDetail(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*GetConversationDetailRequest)

	convDAO := &dao.ConversationDAO{}
	conv, err := convDAO.GetConversationByID(ctx, r.ChatID)
	if err != nil {
		return nil, err
	}
	if conv == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "对话不存在"}
	}

	// 工作流全集：brief 与完整模式都需要（workflow_list / workflow 面板数据源）。
	taskDAO := &dao.TaskDAO{}
	wfs, err := taskDAO.ListWorkflowTasksByChatID(ctx, r.ChatID)
	if err != nil {
		return nil, err
	}

	// 对话任务详情只在完整模式渲染。brief（轮询）模式跳过：轮询只需工作流
	// 状态与节点进度，对话任务的 think 重建（enrichThinkProcess 查 turn 表）是
	// 高频重活，跳过能显著降载。
	//
	// token 索引则两种模式都构建 —— 它是一次 GROUP BY（轻量），工作流节点的
	// token 消耗靠它展示，跳过会让 DAG 面板 token 在轮询期间闪成 0。
	tokenIdx := dao.BuildChatTokenIndex(ctx, r.ChatID)

	var tasks []*model.Task
	taskDetails := make([]map[string]interface{}, 0)
	if !r.Brief {
		tasks, err = taskDAO.ListTasksByChatID(ctx, r.ChatID)
		if err != nil {
			return nil, err
		}

		// 提前加载工作流全集：给对话任务注入其绑定工作流的执行状态与结果
		// （见 enrichBoundWorkflow）。放在任务详情循环之前，避免逐任务二次查询。
		wfByID := make(map[string]*boundWorkflowSnapshot, len(wfs))
		for _, wf := range wfs {
			wfByID[wf.TaskID] = &boundWorkflowSnapshot{
				dag:    decodeWorkflowOutput(wf),
				status: wf.Status,
			}
		}

		turnDAO := &dao.TaskTurnDAO{}
		taskDetails = make([]map[string]interface{}, 0, len(tasks))
		for _, t := range tasks {
			d := buildTaskDetailMap(t)
			attachTokenStats(d, tokenIdx, t.TaskID)
			enrichThinkProcess(ctx, turnDAO, t, d)
			enrichBoundWorkflow(d, wfByID)
			taskDetails = append(taskDetails, d)
		}
	}

	// 获取对话关联的 MCP 服务器
	mcpDAO := &dao.ConversationMCPDAO{}
	mcpItems, _ := mcpDAO.ListConversationMCP(ctx, r.ChatID)

	// 获取对话关联的 Skill
	convSkillDAO := &dao.ConversationSkillDAO{}
	convSkillItems, _ := convSkillDAO.ListConversationSkill(ctx, r.ChatID)
	skillIDs := make([]string, len(convSkillItems))
	for i, s := range convSkillItems {
		skillIDs[i] = s.SkillID
	}
	skillDAO := &dao.SkillDAO{}
	skills, _ := skillDAO.GetSkillsByIDs(ctx, skillIDs)
	skillNameMap := make(map[string]string, len(skills))
	for _, s := range skills {
		skillNameMap[s.ID] = s.Name
	}

	mcpServers := make([]ConversationMCPServerInfo, 0)
	for _, m := range mcpItems {
		mcpServers = append(mcpServers, ConversationMCPServerInfo{
			ServerID: m.ServerID,
			Enabled:  m.Enabled,
		})
	}

	convSkills := make([]ConversationSkillItem, 0)
	for _, s := range convSkillItems {
		convSkills = append(convSkills, ConversationSkillItem{
			SkillID: s.SkillID,
			Name:    skillNameMap[s.SkillID],
			Enabled: s.Enabled,
		})
	}

	chatInfo := map[string]interface{}{
		"chat_id":     conv.ChatID,
		"title":       conv.Title,
		"status":      int(conv.Status),
		"create_user": conv.CreateUser,
		"create_ts":   conv.CreatedAt.UnixMilli(),
		"update_ts":   conv.UpdatedAt.UnixMilli(),
		"task_count":  len(tasks),
		"mcp_servers": mcpServers,
		"skills":      convSkills,
		// 对话当前选用的模型配置 ID，为空表示用默认模型。前端模型选择器据此高亮。
		"model_id": conv.ModelID,
	}
	// 工作流（v1.4：一个对话可有多条）
	//
	// workflow_list 是全集（切换器数据源），workflow 是当前激活那条完整 DAG
	// （DAG 面板数据源）。一条都没有时整个块不出现，前端据此不渲染面板。
	if len(wfs) > 0 {
		activeID := resolveActiveWorkflowID(wfs, conv.WorkflowTaskID)
		chatInfo["workflow_list"] = buildWorkflowList(wfs, activeID)
		chatInfo["active_workflow_task_id"] = activeID
		// 兼容旧字段：老前端只读 workflow_task_id，语义收敛为「当前激活」
		chatInfo["workflow_task_id"] = activeID
		if wf := loadBoundWorkflow(ctx, taskDAO, activeID, tokenIdx); wf != nil {
			chatInfo["workflow"] = wf
		}
	}

	return map[string]interface{}{
		"chat_info": chatInfo,
		"tasks":     taskDetails,
	}, nil
}

// loadBoundWorkflow 读取对话绑定的工作流任务，拼成前端要的 DAG 视图。
//
// 任何异常（未绑定 / 任务被清理 / DAG 解析失败）都返回 nil，
// 前端只是少一块工作流面板，对话本身照常可用。
func loadBoundWorkflow(
	ctx *gin.Context, taskDAO *dao.TaskDAO, workflowTaskID string, idx *dao.ChatTokenIndex,
) map[string]interface{} {
	if workflowTaskID == "" {
		return nil
	}
	task, err := taskDAO.GetTaskByID(ctx, workflowTaskID)
	if err != nil {
		log.Printf("[handler] get bound workflow task %s failed: %v", workflowTaskID, err)
		return nil
	}
	if task == nil {
		log.Printf("[handler] bound workflow task %s not found", workflowTaskID)
		return nil
	}

	var dag model.WorkflowOutputData
	if parsed := decodeWorkflowOutput(task); parsed != nil {
		dag = *parsed
	}

	// 标题优先取 DAG 内的（模型生成时写进去的那份），没有再退回 task.title。
	// 一个对话有多条工作流时，前端横幅/切换器靠它区分「看的是哪一条」。
	title := dag.Title
	if title == "" {
		title = task.Title
	}

	// DAG 面板只展示节点摘要（name/description/status），完整 output（tool_calls /
	// think 明细）由 GetWorkflowNodeDetail 点开节点时单独拉。这里与 buildTaskDetailMap
	// 一致，先摘要化节点 output，避免每次轮询 DAG 都带上 MB 级明细字段。
	for i := range dag.Nodes {
		if dag.Nodes[i].Output != nil {
			summarizeNodeOutputForDetail(dag.Nodes[i].Output)
		}
	}

	// 节点带上各自的 token 消耗与执行时长，同时算出整条工作流的合计
	durations := loadWorkflowDurations(ctx, taskDAO, task.TaskID, dag.Nodes)
	nodeViews, usage := buildWorkflowNodeViews(dag.Nodes, idx, task.TaskID, durations)

	return map[string]interface{}{
		"task_id":      task.TaskID,
		"title":        title,
		"task_status":  int(task.Status),
		"status_name":  taskStatusName(task.Status),
		"stage":        task.Stage,
		"stage_name":   model.TaskStage(task.Stage).String(),
		"progress":     task.Progress,
		"plan_summary": dag.PlanSummary,
		// 恒为数组，避免前端对 null 做额外判空
		"nodes":       nodeViews,
		"token_usage": usage,
		"duration":    taskDuration(task, time.Now()),
		// 工作流级产物清单（各子任务 output_files 的并集），
		// 供右侧结果面板直接展示交付文件，无需前端自己遍历节点收集
		"output_files":  nonNilFiles(dag.OutputFiles),
		"create_ts":     task.CreatedAt.UnixMilli(),
		"update_ts":     task.UpdatedAt.UnixMilli(),
		"failed_reason": task.FailedReason.Data,
	}
}

// nonNilNodes 保证节点列表序列化成 [] 而不是 null
func nonNilNodes(nodes []model.WorkflowNodeData) []model.WorkflowNodeData {
	if nodes == nil {
		return []model.WorkflowNodeData{}
	}
	return nodes
}

// nonNilFiles 保证文件列表序列化成 [] 而不是 null
func nonNilFiles(files []model.FileInfo) []model.FileInfo {
	if files == nil {
		return []model.FileInfo{}
	}
	return files
}

// ---------------------------------------------------------------------------
// 工作流 token 归集
// ---------------------------------------------------------------------------

// WorkflowTokenUsage 工作流 / 工作流节点的 token 消耗。
// 三列都含子任务（见 buildWorkflowNodesWithTokens）。
type WorkflowTokenUsage struct {
	TotalTokens      int64 `json:"total_tokens"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// WorkflowDuration 一段执行的时间信息（工作流整体 / 单个节点通用）。
type WorkflowDuration struct {
	// DurationMs 耗时毫秒。运行中的任务以当前时间为终点，前端每次轮询拿到
	// 的都是更大的值，配合本地秒跳就是持续增长的「执行中 1m 23s」。
	DurationMs int64 `json:"duration_ms"`
	StartTs    int64 `json:"start_ts"`
	// EndTs 结束时间毫秒；0 表示尚未结束（仍在排队/执行）。
	EndTs int64 `json:"end_ts"`
	// Running 是否仍在进行中。前端据此决定要不要本地秒跳。
	Running bool `json:"running"`
}

// taskSpan 一个任务（或其子树）覆盖的时间区间。
type taskSpan struct {
	start   time.Time
	end     time.Time
	running bool
}

// loadWorkflowDurations 读取工作流各节点子任务及其子任务（孙层）的时间快照，
// 归集成「节点 task_id → 该节点子树的执行区间」。
//
// 分两次批量查（节点层 / 孙层）而不是对每个节点递归查：详情接口是 5s 轮询的
// 高频路径，逐节点递归会退化成 N+1 次查询。孙层已足够覆盖 deep_search
// （节点 → deep_search 任务 → 搜索子任务），再深没有实际产生数据的路径。
//
// 查询失败只记日志、返回空 map：时长是展示性信息，不该把整个工作流面板拖挂。
func loadWorkflowDurations(
	ctx *gin.Context, taskDAO *dao.TaskDAO, workflowTaskID string, nodes []model.WorkflowNodeData,
) map[string]WorkflowDuration {
	nodeIDs := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n.TaskID != "" {
			nodeIDs = append(nodeIDs, n.TaskID)
		}
	}
	if len(nodeIDs) == 0 {
		return nil
	}

	children, err := taskDAO.ListTaskTimeRanges(ctx, []string{workflowTaskID})
	if err != nil {
		log.Printf("[handler] list workflow node time ranges for %s failed: %v", workflowTaskID, err)
		return nil
	}
	grandchildren, err := taskDAO.ListTaskTimeRanges(ctx, nodeIDs)
	if err != nil {
		log.Printf("[handler] list workflow descendant time ranges for %s failed: %v", workflowTaskID, err)
	}
	return buildWorkflowDurations(children, grandchildren, time.Now())
}

// buildWorkflowDurations 把子任务与孙任务的时间快照归集成每个节点的执行区间。
//
// 取 min(start) / max(end) 而不是只看节点子任务自身：deep_search 派生的搜索
// 任务挂在孙层，节点时长理应覆盖它们 —— 与 token 归集同口径，都是「这棵子树」。
func buildWorkflowDurations(
	children, descendants []dao.TaskTimeRange, now time.Time,
) map[string]WorkflowDuration {
	spans := make(map[string]*taskSpan, len(children))
	for i := range children {
		spans[children[i].TaskID] = newTaskSpan(children[i], now)
	}
	for i := range descendants {
		d := descendants[i]
		parent, ok := spans[d.ParentTaskID]
		if !ok {
			continue
		}
		ds := newTaskSpan(d, now)
		if ds.start.Before(parent.start) {
			parent.start = ds.start
		}
		if ds.end.After(parent.end) {
			parent.end = ds.end
		}
		parent.running = parent.running || ds.running
	}

	out := make(map[string]WorkflowDuration, len(spans))
	for id, s := range spans {
		out[id] = newWorkflowDuration(*s)
	}
	return out
}

// newTaskSpan 把一个任务的时间快照解析成区间。
//
// 起点优先 start_time（任务真正开始执行），退回 created_time —— 还没被调度到时，
// 排队时间同样是用户感知到的等待。
// 终点优先 end_time；未终态的任务取 now（表现为耗时持续增长）；
// 终态却没有 end_time 只可能是历史脏数据，退回 updated_time，避免时间无限上涨。
func newTaskSpan(t dao.TaskTimeRange, now time.Time) *taskSpan {
	s := taskSpan{start: t.CreatedAt, end: now}
	if t.StartTime != nil && !t.StartTime.IsZero() && t.StartTime.Before(s.end) {
		s.start = *t.StartTime
	}
	running := !dao.IsTerminalStatus(t.Status)
	if t.EndTime != nil && !t.EndTime.IsZero() {
		s.end = *t.EndTime
		running = false
	} else if !running {
		s.end = t.UpdatedAt
	}
	s.running = running
	// 数据异常（结束早于开始）时截断到 0，别让前端显示负数
	if s.end.Before(s.start) {
		s.end = s.start
	}
	return &s
}

func newWorkflowDuration(s taskSpan) WorkflowDuration {
	d := WorkflowDuration{
		DurationMs: s.end.Sub(s.start).Milliseconds(),
		StartTs:    s.start.UnixMilli(),
		Running:    s.running,
	}
	if !s.running {
		d.EndTs = s.end.UnixMilli()
	}
	return d
}

// taskDuration 工作流任务自身的时长（规划期 + 各节点执行，取墙钟）。
func taskDuration(t *model.Task, now time.Time) WorkflowDuration {
	return newWorkflowDuration(*newTaskSpan(dao.TaskTimeRange{
		Status:    t.Status,
		StartTime: t.StartTime,
		EndTime:   t.EndTime,
		CreatedAt: t.CreatedAt,
		UpdatedAt: t.UpdatedAt,
	}, now))
}

// buildWorkflowNodeViews 把 DAG 节点转成前端视图，给每个节点挂上
// 「这棵子树」的 token 消耗与执行时长，并返回整条工作流的 token 合计。
//
// 为什么要按子树算而不是只看节点子任务自身：节点内部调用 deep_search 时，
// 真正的消耗（多轮「搜索 → 抓正文」，每轮都把整页正文塞进上下文）记在
// deep_search 派生的孙任务名下，只看节点自身会严重低估 —— 与对话级归集
// 同理，详见 dao/token_stats.go。因此节点消耗取 idx.Total（自身 + 子任务）。
//
// 工作流合计 = 工作流任务自身在规划期的调用 + 各节点子树 + 差值补齐。
// 差值补齐是兜底：工作流名下可能存在没登记进 DAG 的直接子任务
// （现网理论上为 0，但历史数据/异常路径可能有），用 idx.Sub 与「节点自身
// 消耗之和」的差把它们算进来，避免这部分游离消耗被漏掉。
//
// idx 为 nil（token 索引构建失败）时退化成只返回节点视图、消耗全 0 ——
// token 是展示性信息，不该把整个工作流面板拖挂。
func buildWorkflowNodeViews(
	nodes []model.WorkflowNodeData,
	idx *dao.ChatTokenIndex,
	workflowTaskID string,
	durations map[string]WorkflowDuration,
) ([]map[string]interface{}, WorkflowTokenUsage) {
	views := make([]map[string]interface{}, 0, len(nodes))
	// nodeTotal 各节点子树之和；nodeOwn 各节点自身之和（用于算差值补齐）
	var nodeTotal, nodeOwn dao.TokenSums

	for _, n := range nodes {
		view := workflowNodeView(n)
		if n.TaskID != "" {
			if d, ok := durations[n.TaskID]; ok {
				view["duration"] = d
			}
		}
		if idx != nil && n.TaskID != "" {
			t := idx.Total(n.TaskID)
			view["token_usage"] = WorkflowTokenUsage{
				TotalTokens:      t.Total,
				PromptTokens:     t.Prompt,
				CompletionTokens: t.Completion,
			}
			nodeTotal = addTokenSums(nodeTotal, t)
			nodeOwn = addTokenSums(nodeOwn, idx.Own(n.TaskID))
		}
		views = append(views, view)
	}

	var total dao.TokenSums
	if idx != nil {
		total = addTokenSums(idx.Own(workflowTaskID), nodeTotal)
		total = addTokenSums(total, diffTokenSums(idx.Sub(workflowTaskID), nodeOwn))
	}
	return views, WorkflowTokenUsage{
		TotalTokens:      total.Total,
		PromptTokens:     total.Prompt,
		CompletionTokens: total.Completion,
	}
}

// workflowNodeView 把节点结构转成 map，以便在 handler 层附加展示字段。
//
// 不直接给 model.WorkflowNodeData 加字段：DAG 是整块序列化回写 task_output 的
// 持久化结构，把「仅用于展示的 token 消耗」塞进去会污染存储，也会让
// 每次回写都带上冗余数据。
func workflowNodeView(n model.WorkflowNodeData) map[string]interface{} {
	fallback := func() map[string]interface{} {
		return map[string]interface{}{
			"node_id": n.NodeID,
			"name":    n.Name,
			"status":  n.Status,
		}
	}
	b, err := json.Marshal(n)
	if err != nil {
		return fallback()
	}
	var m map[string]interface{}
	if err := json.Unmarshal(b, &m); err != nil {
		return fallback()
	}
	return m
}

func addTokenSums(a, b dao.TokenSums) dao.TokenSums {
	return dao.TokenSums{
		Prompt:     a.Prompt + b.Prompt,
		Completion: a.Completion + b.Completion,
		Total:      a.Total + b.Total,
	}
}

// diffTokenSums 返回 a - b，逐列下限截断到 0。
func diffTokenSums(a, b dao.TokenSums) dao.TokenSums {
	return dao.TokenSums{
		Prompt:     maxInt64(a.Prompt-b.Prompt, 0),
		Completion: maxInt64(a.Completion-b.Completion, 0),
		Total:      maxInt64(a.Total-b.Total, 0),
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// GetConversationDetailBinder 绑定 GetConversationDetail 请求参数
func GetConversationDetailBinder() interface{} {
	return &GetConversationDetailRequest{}
}

// DeleteConversation 删除对话
func DeleteConversation(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*DeleteConversationRequest)

	convDAO := &dao.ConversationDAO{}
	conv, err := convDAO.GetConversationByID(ctx, r.ChatID)
	if err != nil {
		return nil, err
	}
	if conv == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "对话不存在"}
	}

	if err := convDAO.DeleteConversation(ctx, r.ChatID); err != nil {
		return nil, err
	}
	return nil, nil
}

// DeleteConversationBinder 绑定 DeleteConversation 请求参数
func DeleteConversationBinder() interface{} {
	return &DeleteConversationRequest{}
}

// --- 切换对话模型 ---
type UpdateConversationModelRequest struct {
	ChatID string `json:"chat_id"`
	// ModelID 目标模型配置 ID；传空串表示恢复默认模型
	ModelID string `json:"model_id"`
}

// UpdateConversationModel 切换对话使用的模型（选择器切换后立即持久化）。
func UpdateConversationModel(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*UpdateConversationModelRequest)

	convDAO := &dao.ConversationDAO{}
	conv, err := convDAO.GetConversationByID(ctx, r.ChatID)
	if err != nil {
		return nil, err
	}
	if conv == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "对话不存在"}
	}
	if err := convDAO.UpdateConversationModel(ctx, r.ChatID, r.ModelID); err != nil {
		return nil, err
	}
	return nil, nil
}

// UpdateConversationModelBinder 绑定 UpdateConversationModel 请求参数
func UpdateConversationModelBinder() interface{} {
	return &UpdateConversationModelRequest{}
}

// --- 修改对话标题 ---
type UpdateConversationTitleRequest struct {
	ChatID string `json:"chat_id"`
	Title  string `json:"title"`
}

// UpdateConversationTitle 修改对话标题（用户在侧边栏/头部重命名）。
func UpdateConversationTitle(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*UpdateConversationTitleRequest)

	// 规范化：去首尾空白，限制最大长度（与 model.Conversation.Title 的 size:512 对齐，
	// 按 rune 截断避免截断多字节字符）。空标题视为非法，避免产生无标题对话。
	title := strings.TrimSpace(r.Title)
	if title == "" {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "标题不能为空"}
	}
	if len([]rune(title)) > 512 {
		title = string([]rune(title)[:512])
	}

	convDAO := &dao.ConversationDAO{}
	conv, err := convDAO.GetConversationByID(ctx, r.ChatID)
	if err != nil {
		return nil, err
	}
	if conv == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "对话不存在"}
	}
	if err := convDAO.UpdateConversationTitle(ctx, r.ChatID, title); err != nil {
		return nil, err
	}
	return nil, nil
}

// UpdateConversationTitleBinder 绑定 UpdateConversationTitle 请求参数
func UpdateConversationTitleBinder() interface{} {
	return &UpdateConversationTitleRequest{}
}

// ListConversationMCP 查看对话关联的 MCP 服务器
func ListConversationMCP(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*ConversationMCPRequest)

	mcpDAO := &dao.ConversationMCPDAO{}
	items, err := mcpDAO.ListConversationMCP(ctx, r.ChatID)
	if err != nil {
		return nil, err
	}

	// 获取服务器名称
	serverIDs := make([]string, len(items))
	for i, item := range items {
		serverIDs[i] = item.ServerID
	}

	serverDAO := &dao.MCPServerDAO{}
	servers, _ := serverDAO.GetServersByIDs(ctx, serverIDs)
	serverNameMap := make(map[string]string)
	for _, s := range servers {
		serverNameMap[s.ID] = s.Name
	}

	result := make([]*ConversationMCPItem, 0, len(items))
	for _, item := range items {
		result = append(result, &ConversationMCPItem{
			ServerID: item.ServerID,
			Name:     serverNameMap[item.ServerID],
			Enabled:  item.Enabled,
		})
	}

	return &ConversationMCPListResponse{Items: result}, nil
}

// ListConversationMCPBinder 绑定 ListConversationMCP 请求参数
func ListConversationMCPBinder() interface{} {
	return &ConversationMCPRequest{}
}

// AddConversationMCP 给对话添加 MCP 服务器
func AddConversationMCP(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*ConversationMCPRequest)

	mcpDAO := &dao.ConversationMCPDAO{}
	if err := mcpDAO.AddConversationMCP(ctx, r.ChatID, r.ServerIDs); err != nil {
		return nil, err
	}
	return nil, nil
}

// AddConversationMCPBinder 绑定 AddConversationMCP 请求参数
func AddConversationMCPBinder() interface{} {
	return &ConversationMCPRequest{}
}

// RemoveConversationMCP 从对话移除 MCP 服务器
func RemoveConversationMCP(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*ConversationMCPRequest)

	mcpDAO := &dao.ConversationMCPDAO{}
	if err := mcpDAO.RemoveConversationMCP(ctx, r.ChatID, r.ServerIDs); err != nil {
		return nil, err
	}
	return nil, nil
}

// RemoveConversationMCPBinder 绑定 RemoveConversationMCP 请求参数
func RemoveConversationMCPBinder() interface{} {
	return &ConversationMCPRequest{}
}

// ListConversationSkill 查看对话关联的 Skill
func ListConversationSkill(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*ConversationSkillRequest)

	convSkillDAO := &dao.ConversationSkillDAO{}
	items, err := convSkillDAO.ListConversationSkill(ctx, r.ChatID)
	if err != nil {
		return nil, err
	}

	// 批量回填 skill 名称
	skillIDs := make([]string, len(items))
	for i, item := range items {
		skillIDs[i] = item.SkillID
	}
	skillDAO := &dao.SkillDAO{}
	skills, _ := skillDAO.GetSkillsByIDs(ctx, skillIDs)
	nameMap := make(map[string]string, len(skills))
	for _, s := range skills {
		nameMap[s.ID] = s.Name
	}

	result := make([]*ConversationSkillItem, 0, len(items))
	for _, item := range items {
		result = append(result, &ConversationSkillItem{
			SkillID: item.SkillID,
			Name:    nameMap[item.SkillID],
			Enabled: item.Enabled,
		})
	}
	return &ConversationSkillListResponse{Items: result}, nil
}

// ListConversationSkillBinder 绑定 ListConversationSkill 请求参数
func ListConversationSkillBinder() interface{} {
	return &ConversationSkillRequest{}
}

// AddConversationSkill 给对话添加 Skill
func AddConversationSkill(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*ConversationSkillRequest)

	convSkillDAO := &dao.ConversationSkillDAO{}
	if err := convSkillDAO.AddConversationSkill(ctx, r.ChatID, r.SkillIDs); err != nil {
		return nil, err
	}
	return nil, nil
}

// AddConversationSkillBinder 绑定 AddConversationSkill 请求参数
func AddConversationSkillBinder() interface{} {
	return &ConversationSkillRequest{}
}

// RemoveConversationSkill 从对话移除 Skill
func RemoveConversationSkill(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*ConversationSkillRequest)

	convSkillDAO := &dao.ConversationSkillDAO{}
	if err := convSkillDAO.RemoveConversationSkill(ctx, r.ChatID, r.SkillIDs); err != nil {
		return nil, err
	}
	return nil, nil
}

// RemoveConversationSkillBinder 绑定 RemoveConversationSkill 请求参数
func RemoveConversationSkillBinder() interface{} {
	return &ConversationSkillRequest{}
}

// ============================================================
// 辅助函数
// ============================================================

// attachTokenStats 给任务详情挂上 token 消耗字段。
//
// total_tokens 是**含子任务**的总量。这一点很关键：deep_search 会派生
// 独立子任务去做多轮「搜索 → 抓正文」，消耗全记在子任务名下，
// 只看任务自身会严重低估 —— 实测一次带检索的对话，子任务能占到总量的 85%+。
//
// sub_task_tokens 单独给出，前端据此显示「其中检索占多少」；
// prompt / completion 拆开，因为上下文水位看的是输入侧。
func attachTokenStats(d map[string]interface{}, idx *dao.ChatTokenIndex, taskID string) {
	own, sub, total := idx.Own(taskID), idx.Sub(taskID), idx.Total(taskID)
	d["total_tokens"] = total.Total
	d["own_tokens"] = own.Total
	d["sub_task_tokens"] = sub.Total
	d["prompt_tokens"] = total.Prompt
	d["completion_tokens"] = total.Completion
}

func buildTaskDetailMap(t *model.Task) map[string]interface{} {
	result := map[string]interface{}{
		"task_id":        t.TaskID,
		"chat_id":        t.ChatID,
		"parent_task_id": t.ParentTaskID,
		"task_type":      int(t.TaskType),
		"task_status":    int(t.Status),
		"progress":       t.Progress,
		"failed_reason":  t.FailedReason.Data,
		"create_ts":      t.CreatedAt.UnixMilli(),
		"time_cost":      "",
	}

	if t.StartTime != nil {
		result["start_ts"] = t.StartTime.UnixMilli()
	}
	if t.EndTime != nil {
		result["end_ts"] = t.EndTime.UnixMilli()
	}

	// 总耗时从任务创建时刻算起，而不是 start_time。
	//
	// start_time 是任务被调度器首次捞起（进入 doing）才写入的，
	// 用它算耗时会漏掉：planning 之前的排队、以及每次 wait → doing 的
	// 调度等待。用户感知的是「我提交到拿到结果」的墙钟时间，
	// 所以基准必须是 create_ts。
	//
	// 未结束的任务算到当前时刻，让前端能显示实时增长的耗时。
	endAt := time.Now()
	if t.EndTime != nil {
		endAt = *t.EndTime
	}
	if cost := endAt.Sub(t.CreatedAt); cost > 0 {
		result["time_cost"] = formatDuration(cost)
		result["time_cost_ms"] = cost.Milliseconds()
	} else {
		result["time_cost_ms"] = int64(0)
	}

	// 解析输入
	if t.TaskInput.Data != nil {
		result["task_input"] = t.TaskInput.Data
	}

	// 解析输出（按 task_type 拆成 agent_output / workflow_output 两个字段）。
	//
	// 前端按 task_type 分别渲染单 agent 回复与工作流 DAG，
	// 因此这里不返回统一的 task_output，而是拆开给出，与前端契约保持一致。
	if t.TaskOutput != "" {
		switch t.TaskType {
		case model.TaskTypeConversation, model.TaskTypeWorkflowSubTask:
			var agentOut model.AgentOutputData
			if json.Unmarshal([]byte(t.TaskOutput), &agentOut) == nil {
				trimDetailToolCallOutputs(&agentOut)
				result["agent_output"] = agentOut
			}
		case model.TaskTypeWorkflow:
			var wfOut model.WorkflowOutputData
			if json.Unmarshal([]byte(t.TaskOutput), &wfOut) == nil {
				for i := range wfOut.Nodes {
					if wfOut.Nodes[i].Output != nil {
						trimDetailToolCallOutputs(wfOut.Nodes[i].Output)
						// 详情接口只给节点 reply 与产物摘要，tool_calls / think 明细
						// 由 GetWorkflowNodeDetail 点开节点时单独拉，避免 DAG 膨胀。
						summarizeNodeOutputForDetail(wfOut.Nodes[i].Output)
					}
				}
				result["workflow_output"] = wfOut
			}
		}
	}

	return result
}

// summarizeNodeOutputForDetail 摘要化工作流节点 output：详情接口只保留 reply 与
// 产物清单，去掉 tool_calls / think_process / think_layers 这些明细大字段。
// 节点详情页（WorkflowNodeDetail）点开时经 GetWorkflowNodeDetail 单独拉完整。
func summarizeNodeOutputForDetail(out *model.AgentOutputData) {
	if out == nil {
		return
	}
	out.ToolCalls = nil
	out.ThinkProcess = nil
	out.ThinkLayers = nil
}

// trimDetailToolCallOutputs 截断 agent_output 里工具调用记录的超大输出。
func trimDetailToolCallOutputs(out *model.AgentOutputData) {
	if out == nil {
		return
	}
	for i := range out.ToolCalls {
		out.ToolCalls[i].Output = model.TruncateToolCallOutput(out.ToolCalls[i].Output)
	}
}

// enrichThinkProcess 实时从 task_turn 补齐思考过程。
//
// 为什么不能只依赖 task_output：
//   - 执行中的任务还没写 task_output，但用户此时最想看到「进行到哪一步了」
//   - 失败的任务由容器补写，存在时序窗口；且 planning/工具阶段失败时
//     failed_reason 只有一句话，中间过程恰恰是排查关键
//
// 重建规则按任务状态分流：
//   - 终态任务：think_process 已由 actuator（成功）或容器（失败补写）写入，
//     是最完整的一版，已有的不覆盖。
//   - running 任务：think_process 可能是「失败重试残留的过期快照」——早期
//     失败收口时 ToFailedStatus 把 task_output 写成了旧快照，框架自动重试
//     恢复 running 后 task_output 不清空；若不重建，前端会一直卡在旧快照，
//     看不到后续实时进展。因此 running 任务一律实时重建。
func enrichThinkProcess(
	ctx *gin.Context, turnDAO *dao.TaskTurnDAO,
	t *model.Task, detail map[string]interface{},
) {
	// 对话任务与单 Agent 子任务都走 agent_output 契约（子任务产出 AgentOutputData）。
	// 工作流任务本身存的是 DAG，有自己的 workflow_output 展示，不在此补齐。
	if t.TaskType != model.TaskTypeConversation && t.TaskType != model.TaskTypeWorkflowSubTask {
		return
	}

	// 终态任务且已有非空 think_process：actuator/容器已写入最完整一版，不重建。
	// running 任务：实时重建，覆盖失败重试残留的过期快照。
	if !shouldRebuildThinkProcess(t, detail) {
		return
	}

	turns, err := turnDAO.ListTurnsByTaskIDForThink(ctx, t.TaskID)
	if err != nil || len(turns) == 0 {
		return
	}
	out, _ := detail["agent_output"].(model.AgentOutputData)
	// 执行中的 task_output 还没有最终汇总，按已落库 turn 重建工具调用链；
	// 终态若已有完整快照则保留 actuator/容器写入的版本。
	if !t.IsTerminal() || len(out.ToolCalls) == 0 {
		out.ToolCalls = toolCallRecordsFromTurns(turns)
	}
	out.FillThinkProcess(turns)
	detail["agent_output"] = out
}

// toolCallRecordsFromTurns 与 actuator 的完成态汇总保持同一字段口径，供任务详情
// 在执行期间根据 task_turn 实时生成工具调用链。
func toolCallRecordsFromTurns(turns []*model.TaskTurn) []model.ToolCallRecord {
	calls := make([]model.ToolCallRecord, 0)
	for _, turn := range turns {
		if turn == nil || turn.TurnType != model.TurnTypeToolCall || turn.Status == model.TurnStatusInterrupted {
			continue
		}
		rec := model.ToolCallRecord{
			ToolName:   turn.ToolName,
			ServerName: turn.ServerName,
			Status:     int(turn.Status),
			TimeCost:   fmt.Sprintf("%dms", turn.TimeCostMs),
		}
		if turn.Input != "" {
			var input model.TurnToolInput
			if json.Unmarshal([]byte(turn.Input), &input) == nil {
				if args, err := json.Marshal(input.Arguments); err == nil {
					rec.Input = string(args)
				}
				if rec.ToolName == "" {
					rec.ToolName = input.ToolName
				}
				if rec.ServerName == "" {
					rec.ServerName = input.ServerName
				}
				rec.Summary = strings.TrimSpace(input.Summary)
			}
		}
		if turn.Output != "" {
			var output model.TurnToolOutput
			if json.Unmarshal([]byte(turn.Output), &output) == nil {
				rec.Output = model.TruncateToolCallOutput(output.Content)
			}
		}
		if turn.ErrorMsg != "" && rec.Output == "" {
			rec.Output = turn.ErrorMsg
		}
		calls = append(calls, rec)
	}
	return calls
}

// shouldRebuildThinkProcess 判断是否要实时重建 think_process。
//
// 终态任务（completed / failed / stopped / partial_failed）的 think_process
// 已由 actuator（成功）或容器（失败补写）写入最完整一版，已有非空就不重建；
// 其余（running / queued）任务的 think_process 可能是「失败重试残留的过期
// 快照」，必须实时重建，否则前端会一直卡在旧快照、看不到后续实时进展。
func shouldRebuildThinkProcess(t *model.Task, detail map[string]interface{}) bool {
	if !t.IsTerminal() {
		return true
	}
	out, ok := detail["agent_output"].(model.AgentOutputData)
	return !ok || len(out.ThinkProcess) == 0
}

// boundWorkflowSnapshot 一条工作流的展示快照：DAG + 整体状态。
//
// 前端 5s 轮询对话详情，同一对话里的多个对话任务可能绑定同一条工作流，
// 把 DAG 预解析一次缓存起来，避免逐任务重复 json.Unmarshal 同一份 longtext。
type boundWorkflowSnapshot struct {
	dag    *model.WorkflowOutputData
	status model.TaskStatus
}

// enrichBoundWorkflow 把对话任务绑定工作流的执行状态与结果注入 detail。
//
// 对话任务规划判定 workflow 后当轮收口，其回复与思考过程停在「规划完成」。
// 真正干活的是它规划出的节点（WorkflowNodeData.SourceTaskID 指回该任务）。
// 这里实时从工作流 DAG 提取绑定节点的执行状态，动态补两段：
//   - 执行中：往 think_layers.execution 追加一条 running 的「工作流执行中」，
//     让用户在思考过程里看到进度；
//   - 终态：往 reply_text 追加落库的 WorkflowSummary（由 workflow_actuator
//     在工作流收口时调用 LLM 生成 —— 成功给结论、失败给原因，不做机械拼接）。
//
// think 状态条目动态合成（执行中每轮都变）；汇总文本则读落库字段（LLM 生成的
// 一次性结果）。每次请求都从落库的原始 reply_text / think_layers 重新拼，天然
// 幂等，不会累加重复条目。
func enrichBoundWorkflow(detail map[string]interface{}, wfByID map[string]*boundWorkflowSnapshot) {
	out, ok := detail["agent_output"].(model.AgentOutputData)
	if !ok || out.WorkflowTaskID == "" {
		return
	}
	snap, ok := wfByID[out.WorkflowTaskID]
	if !ok || snap == nil || snap.dag == nil {
		return
	}

	taskID, _ := detail["task_id"].(string)
	var nodes []model.WorkflowNodeData
	for _, n := range snap.dag.Nodes {
		if n.SourceTaskID == taskID {
			nodes = append(nodes, n)
		}
	}
	view := model.BuildBoundWorkflowView(nodes, snap.status)

	if view.ExecutionStep != nil {
		if out.ThinkLayers == nil {
			out.ThinkLayers = &model.ThinkLayers{
				Planning:  make([]model.ThinkStep, 0),
				Execution: make([]model.ThinkStep, 0),
			}
		}
		out.ThinkLayers.Execution = append(out.ThinkLayers.Execution, *view.ExecutionStep)
	}
	// 工作流收口后落库的 LLM 汇总：用分隔线 + 标题与规划回复隔开，
	// 否则两段文字连排，用户分不清哪段是规划、哪段是执行结果。
	if s := strings.TrimSpace(out.WorkflowSummary); s != "" {
		out.ReplyText = strings.TrimRight(out.ReplyText, "\n") +
			"\n\n---\n\n**工作流执行结果**\n\n" + s
		// 追加产物清单。链接必须用工作区相对路径（f.Path），不能用完整下载
		// URL：Markdown 渲染器把非协议链接统一当作工作区相对路径处理，点击时
		// 用当前 chat_id 拼出 download 地址弹预览；直接给 /api/v1/... 完整
		// URL 反而会被二次包装（path=整个URL），后端 404 → 预览「内容读取失败」。
		if files := collectBoundOutputFiles(nodes); len(files) > 0 {
			var b strings.Builder
			b.WriteString("\n\n**产物文件**\n")
			for _, f := range files {
				fmt.Fprintf(&b, "- [%s](%s)\n", f.Name, f.Path)
			}
			out.ReplyText += b.String()
		}
	}
	detail["agent_output"] = out
}

// collectBoundOutputFiles 收集绑定节点产出的文件清单（按 path 去重），
// 只保留带相对路径（Path 非空）的条目。用于在规划回复的「工作流执行结果」
// 里追加产物链接 —— 链接用相对路径，前端 Markdown 渲染器负责拼 download URL。
func collectBoundOutputFiles(nodes []model.WorkflowNodeData) []model.FileInfo {
	seen := make(map[string]bool)
	var files []model.FileInfo
	for _, n := range nodes {
		if n.Output == nil {
			continue
		}
		for _, f := range n.Output.OutputFiles {
			if f.Path == "" || seen[f.Path] {
				continue
			}
			seen[f.Path] = true
			files = append(files, f)
		}
	}
	return files
}

// formatDuration 把耗时格式化成便于展示的形式。
// Duration.String() 会输出 "1m3.482910293s" 这种精度过高的形式，不适合直接给用户看。
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		m := int(d / time.Minute)
		s := d - time.Duration(m)*time.Minute
		return fmt.Sprintf("%dm%.0fs", m, s.Seconds())
	}
}

// APIError API 层错误
type APIError struct {
	Code int
	Msg  string
}

func (e *APIError) Error() string {
	return e.Msg
}
