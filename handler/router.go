package handler

import "github.com/gin-gonic/gin"

// RegisterRoutes 注册所有 API 路由
func RegisterRoutes(router *gin.RouterGroup) {
	api := router.Group("/api/v1")

	// ========== 对话模块 ==========
	conv := api.Group("/conversations")
	{
		conv.POST("/create", Wrap(CreateConversationBinder, CreateConversation))
		conv.POST("/chat", Wrap(ChatBinder, Chat))
		conv.POST("/list", Wrap(ListConversationsBinder, ListConversations))
		conv.POST("/detail", Wrap(GetConversationDetailBinder, GetConversationDetail))
		conv.POST("/delete", Wrap(DeleteConversationBinder, DeleteConversation))
		// 切换对话使用的模型（前端模型选择器切换后立即持久化）
		conv.POST("/update-model", Wrap(UpdateConversationModelBinder, UpdateConversationModel))
		// 修改对话标题（侧边栏 / 头部重命名）
		conv.POST("/update-title", Wrap(UpdateConversationTitleBinder, UpdateConversationTitle))
		// 上下文用量：最近一次 LLM 调用的输入分块明细 + 模型上限/水位
		conv.POST("/context-usage", Wrap(GetContextUsageBinder, GetContextUsage))
	}

	// ========== 工作流模块（一个对话可绑定多条工作流） ==========
	workflows := api.Group("/workflows")
	{
		// 切换器数据源：列出某对话下的全部工作流 + 当前激活 ID
		workflows.POST("/list", Wrap(ListChatWorkflowsBinder, ListChatWorkflows))
		// 切换当前查看的工作流（只改激活指针，不动 DAG / 不改任务状态）
		workflows.POST("/set-active", Wrap(SetActiveWorkflowBinder, SetActiveWorkflow))
	}

	// ========== 全局模型配置模块 ==========
	models := api.Group("/models")
	{
		// 轻量状态查询：前端入口判断是否需要「先配置模型」引导
		models.POST("/status", Wrap(ModelStatusBinder, ModelStatus))
		models.POST("/list", Wrap(ListModelConfigsBinder, ListModelConfigs))
		models.POST("/create", Wrap(CreateModelConfigBinder, CreateModelConfig))
		models.POST("/update", Wrap(UpdateModelConfigBinder, UpdateModelConfig))
		models.POST("/delete", Wrap(DeleteModelConfigBinder, DeleteModelConfig))
		models.POST("/set-default", Wrap(SetDefaultModelBinder, SetDefaultModel))
		models.POST("/test", Wrap(TestModelBinder, TestModel))
	}

	// ========== 系统设置（数据库连接） ==========
	settings := api.Group("/settings")
	{
		settings.POST("/db/get", Wrap(GetDBConfigBinder, GetDBConfig))
		settings.POST("/db/update", Wrap(UpdateDBConfigBinder, UpdateDBConfig))
		settings.POST("/db/test", Wrap(TestDBConfigBinder, TestDBConfig))
	}

	// ========== 全局检索引擎配置模块 ==========
	search := api.Group("/search-config")
	{
		search.POST("/get", Wrap(GetSearchConfigBinder, GetSearchConfig))
		search.POST("/update", Wrap(UpdateSearchConfigBinder, UpdateSearchConfig))
		search.POST("/test", Wrap(TestSearchBinder, TestSearch))
	}

	// 对话 MCP 管理
	convMCP := api.Group("/conversations/mcp")
	{
		convMCP.POST("/list", Wrap(ListConversationMCPBinder, ListConversationMCP))
		convMCP.POST("/add", Wrap(AddConversationMCPBinder, AddConversationMCP))
		convMCP.POST("/remove", Wrap(RemoveConversationMCPBinder, RemoveConversationMCP))
	}

	// 对话 Skill 管理
	convSkill := api.Group("/conversations/skills")
	{
		convSkill.POST("/list", Wrap(ListConversationSkillBinder, ListConversationSkill))
		convSkill.POST("/add", Wrap(AddConversationSkillBinder, AddConversationSkill))
		convSkill.POST("/remove", Wrap(RemoveConversationSkillBinder, RemoveConversationSkill))
	}

	// ========== 任务模块 ==========
	task := api.Group("/tasks")
	{
		task.POST("/detail", Wrap(GetTaskDetailBinder, GetTaskDetail))
		task.POST("/stop", Wrap(StopTaskBinder, StopTask))
		task.POST("/retry", Wrap(RetryTaskBinder, RetryTask))
		task.POST("/turns", Wrap(ListTaskTurnsBinder, ListTaskTurns))
		// 调试页用：按 parent_task_id 拉子任务摘要列表（deep_search / workflow 节点子任务）
		task.POST("/sub-tasks", Wrap(ListSubTasksBinder, ListSubTasks))
		// SSE 走 GET（EventSource 只支持 GET），绕过 Wrap 的 JSON 包装
		task.GET("/stream", TaskStream)
	}

	// ========== MCP 管理模块 ==========
	mcp := api.Group("/mcp/servers")
	{
		mcp.POST("/list", Wrap(ListMCPServersBinder, ListMCPServers))
		mcp.POST("/detail", Wrap(GetMCPServerDetailBinder, GetMCPServerDetail))
		mcp.POST("/create", Wrap(CreateMCPServerBinder, CreateMCPServer))
		mcp.POST("/update", Wrap(UpdateMCPServerBinder, UpdateMCPServer))
		mcp.POST("/delete", Wrap(DeleteMCPServerBinder, DeleteMCPServer))
		mcp.POST("/test", Wrap(TestMCPServerBinder, TestMCPServer))
		mcp.POST("/list-builtins", Wrap(ListBuiltinMCPBinder, ListBuiltinMCP))
		mcp.POST("/import-builtins", Wrap(ImportBuiltinMCPBinder, ImportBuiltinMCP))
	}

	// ========== Skill 管理模块 ==========
	skill := api.Group("/skills")
	{
		skill.POST("/list", Wrap(ListSkillsBinder, ListSkills))
		skill.POST("/detail", Wrap(GetSkillDetailBinder, GetSkillDetail))
		skill.POST("/create", Wrap(CreateSkillBinder, CreateSkill))
		skill.POST("/update", Wrap(UpdateSkillBinder, UpdateSkill))
		skill.POST("/delete", Wrap(DeleteSkillBinder, DeleteSkill))
		skill.POST("/list-builtins", Wrap(ListBuiltinSkillBinder, ListBuiltinSkill))
		skill.POST("/import-builtins", Wrap(ImportBuiltinSkillBinder, ImportBuiltinSkill))
		// zip 整体导入是 multipart（Wrap 走 JSON 绑定），与 files/upload 同理绕过 Wrap
		skill.POST("/import-zip", ImportSkillZip)
	}

	// ========== 文件模块 ==========
	//
	// 两个端点都绕过 Wrap：upload 是 multipart（Wrap 走 JSON 绑定），
	// download 要写文件流（Wrap 会把返回值包成 JSON）。
	files := api.Group("/files")
	{
		files.POST("/upload", UploadFile)
		files.GET("/download", DownloadFile)
		// 本地打开：用系统默认应用打开工作区文件（替换浏览器「下载」）
		files.GET("/open", OpenFile)
	}

	// ========== 内部接口（scheduler → server 回调，不对外） ==========
	internal := router.Group("/internal")
	{
		internal.POST("/events", InternalEvent)
	}
}
