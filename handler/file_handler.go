package handler

// 文件上传 / 下载。
//
// 两个端点都绕过 Wrap：
//   - upload 是 multipart/form-data，Wrap 的 JSON 绑定不适用
//   - download 要写文件流，Wrap 会把返回值包成 JSON
//
// 安全边界（关键）：两个端点都只能访问 workspace/<chat_id>/ 之内的路径，
// 一律经 workspace.Manager.Resolve 做沙箱校验。download 的 path 来自
// 前端传参，若不校验就是标准的路径穿越漏洞（../../etc/passwd）。

import (
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	workspacepkg "github.com/memory-overflow/flowagent/pkg/workspace"
)

// maxUploadSize 单文件上传上限（1GB）。
// 早先为 50MB，实际使用中视频等大附件（>100MB）是真实需求，放宽到 1GB。
// gin 未设 MaxMultipartMemory，SaveUploadedFile 大文件走磁盘临时文件，不会撑爆内存。
const maxUploadSize = 1 << 30

// wsManager 工作区管理器。由 main.go 在启动时通过 SetWorkspaceRoot 注入，
// 与 SetInternalToken 同一套注入方式。
var wsManager *workspacepkg.Manager

// SetWorkspaceRoot 注入工作区根目录（进程启动时调用一次）。
func SetWorkspaceRoot(root string) {
	wsManager = workspacepkg.NewManager(root)
	log.Printf("[file-handler] workspace root: %s", wsManager.Root())
}

// ============================================================
// 上传
// ============================================================

// UploadFile 接收用户上传的附件，落盘到 workspace/<chat_id>/inputs/。
//
// 请求：multipart/form-data
//   - chat_id : 对话 ID（必填，决定落到哪个工作区）
//   - file    : 文件内容（必填）
//
// 响应：CommonResponse{ data: FileInfo }，前端拿到后原样放进
// Chat 请求的 input_files，无需再关心磁盘路径。
//
// 落盘文件名做了「保留原名 + 冲突时加短后缀」处理：
// 原名对模型理解文件用途很重要（"季度财报.xlsx" 比 "a1b2c3.xlsx" 有信息量），
// 所以不做 uuid 重命名，只在重名时追加短 uuid 片段避免互相覆盖。
func UploadFile(c *gin.Context) {
	reqID := ensureReqID(c)

	if wsManager == nil {
		writeError(c, http.StatusInternalServerError, "工作区未初始化", reqID)
		return
	}

	chatID := strings.TrimSpace(c.PostForm("chat_id"))
	if chatID == "" {
		writeError(c, http.StatusBadRequest, "缺少 chat_id", reqID)
		return
	}

	// 校验对话存在，避免任意 chat_id 在工作区根下创建垃圾目录
	convDAO := &dao.ConversationDAO{}
	conv, err := convDAO.GetConversationByID(c.Request.Context(), chatID)
	if err != nil {
		log.Printf("[file-handler][%s] get conversation failed: %v", reqID, err)
		writeError(c, http.StatusInternalServerError, "查询对话失败", reqID)
		return
	}
	if conv == nil {
		writeError(c, http.StatusNotFound, "对话不存在", reqID)
		return
	}

	fileHeader, err := c.FormFile("file")
	if err != nil {
		writeError(c, http.StatusBadRequest, "缺少 file 字段: "+err.Error(), reqID)
		return
	}
	if fileHeader.Size > maxUploadSize {
		writeError(c, http.StatusBadRequest,
			fmt.Sprintf("文件过大（%.1fMB），上限 %dMB",
				float64(fileHeader.Size)/(1<<20), maxUploadSize>>20), reqID)
		return
	}

	// 只取 basename 并清洗，杜绝上传方用 ../ 或绝对路径逃出 inputs/
	origName := sanitizeUploadName(fileHeader.Filename)

	wsDir := wsManager.Dir(chatID)
	inputsDir := filepath.Join(wsDir, workspacepkg.InputsDirName)
	if err := os.MkdirAll(inputsDir, 0o755); err != nil {
		log.Printf("[file-handler][%s] mkdir inputs failed: %v", reqID, err)
		writeError(c, http.StatusInternalServerError, "创建上传目录失败", reqID)
		return
	}

	// 重名时追加短 uuid 片段，保留原始扩展名
	finalName := uniqueFileName(inputsDir, origName)
	relPath := workspacepkg.InputsDirName + "/" + finalName

	// 经沙箱解析确认最终路径合法（双重保险：origName 已清洗过一遍）
	absPath, err := wsManager.Resolve(wsDir, relPath)
	if err != nil {
		log.Printf("[file-handler][%s] resolve upload path failed: %v", reqID, err)
		writeError(c, http.StatusBadRequest, "非法文件名", reqID)
		return
	}

	if err := c.SaveUploadedFile(fileHeader, absPath); err != nil {
		log.Printf("[file-handler][%s] save upload failed: %v", reqID, err)
		writeError(c, http.StatusInternalServerError, "保存文件失败", reqID)
		return
	}

	// MIME 优先用客户端声明的，缺失/无意义时按扩展名推断
	mimeType := fileHeader.Header.Get("Content-Type")
	if mimeType == "" || mimeType == "application/octet-stream" {
		if detected := workspacepkg.DetectMimeType(finalName); detected != "" {
			mimeType = detected
		}
	}

	info := model.FileInfo{
		Source:   "local",
		Name:     finalName,
		Path:     relPath,
		URL:      buildDownloadURL(chatID, relPath),
		Size:     fileHeader.Size,
		MimeType: mimeType,
		FileType: workspacepkg.DetectFileType(finalName),
	}

	log.Printf("[file-handler][%s] uploaded chat=%s path=%s size=%d type=%d",
		reqID, chatID, relPath, info.Size, info.FileType)

	c.JSON(http.StatusOK, CommonResponse{
		Code:  0,
		Msg:   "ok",
		ReqID: reqID,
		Data:  info,
	})
}

// ============================================================
// 下载
// ============================================================

// DownloadFile 下载工作区内的文件（任务产物或用户附件）。
//
// 请求：GET /api/v1/files/download?chat_id=<id>&path=<相对工作区路径>
//   - inline=1 时用 Content-Disposition: inline（图片/PDF 可在浏览器内预览）
//
// path 必须是相对工作区的路径，经 Manager.Resolve 校验；任何绝对路径、
// .. 越界写法都会被拒绝。
func DownloadFile(c *gin.Context) {
	reqID := ensureReqID(c)

	if wsManager == nil {
		writeError(c, http.StatusInternalServerError, "工作区未初始化", reqID)
		return
	}

	chatID := strings.TrimSpace(c.Query("chat_id"))
	relPath := strings.TrimSpace(c.Query("path"))
	if chatID == "" || relPath == "" {
		writeError(c, http.StatusBadRequest, "缺少 chat_id 或 path", reqID)
		return
	}

	wsDir := wsManager.Dir(chatID)
	// 沙箱校验：这一步是安全边界，不能省
	absPath, err := wsManager.Resolve(wsDir, relPath)
	if err != nil {
		log.Printf("[file-handler][%s] reject path chat=%s path=%s: %v", reqID, chatID, relPath, err)
		writeError(c, http.StatusBadRequest, "非法路径: "+err.Error(), reqID)
		return
	}

	st, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(c, http.StatusNotFound, "文件不存在", reqID)
			return
		}
		log.Printf("[file-handler][%s] stat failed: %v", reqID, err)
		writeError(c, http.StatusInternalServerError, "读取文件失败", reqID)
		return
	}
	if st.IsDir() {
		writeError(c, http.StatusBadRequest, "path 指向目录，不是文件", reqID)
		return
	}

	name := filepath.Base(absPath)
	disposition := "attachment"
	if c.Query("inline") == "1" {
		disposition = "inline"
	}

	if mt := workspacepkg.DetectMimeType(name); mt != "" {
		c.Header("Content-Type", mt)
	}
	// filename* 用 RFC 5987 编码，保证中文文件名在各浏览器正确落地
	c.Header("Content-Disposition", fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`,
		disposition, sanitizeHeaderName(name), url.PathEscape(name)))

	c.File(absPath)
}

// ============================================================
// 本地打开
// ============================================================

// OpenFile 用系统默认应用在本地打开工作区内的文件。
//
// 本地 agent 场景下，后端进程就运行在用户自己的机器上，文件已经躺在
// 工作区里。与其让浏览器「下载」一份副本再手动打开，不如直接调用系统
// open 命令，用系统默认应用（预览 / Office / 播放器 / 编辑器等）原地打开。
//
// 请求：GET /api/v1/files/open?chat_id=<id>&path=<相对工作区路径>
// path 与 download 一样经 Manager.Resolve 做沙箱校验，安全边界一致。
func OpenFile(c *gin.Context) {
	reqID := ensureReqID(c)

	if wsManager == nil {
		writeError(c, http.StatusInternalServerError, "工作区未初始化", reqID)
		return
	}

	chatID := strings.TrimSpace(c.Query("chat_id"))
	relPath := strings.TrimSpace(c.Query("path"))
	if chatID == "" || relPath == "" {
		writeError(c, http.StatusBadRequest, "缺少 chat_id 或 path", reqID)
		return
	}

	wsDir := wsManager.Dir(chatID)
	absPath, err := wsManager.Resolve(wsDir, relPath)
	if err != nil {
		log.Printf("[file-handler][%s] reject open path chat=%s path=%s: %v", reqID, chatID, relPath, err)
		writeError(c, http.StatusBadRequest, "非法路径: "+err.Error(), reqID)
		return
	}

	st, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(c, http.StatusNotFound, "文件不存在", reqID)
			return
		}
		log.Printf("[file-handler][%s] stat failed: %v", reqID, err)
		writeError(c, http.StatusInternalServerError, "读取文件失败", reqID)
		return
	}
	if st.IsDir() {
		writeError(c, http.StatusBadRequest, "path 指向目录，不是文件", reqID)
		return
	}

	if err := openLocal(absPath); err != nil {
		log.Printf("[file-handler][%s] open failed: %v", reqID, err)
		writeError(c, http.StatusInternalServerError, "打开文件失败: "+err.Error(), reqID)
		return
	}

	log.Printf("[file-handler][%s] opened chat=%s path=%s", reqID, chatID, relPath)
	c.JSON(http.StatusOK, CommonResponse{Code: 0, Msg: "ok", ReqID: reqID})
}

// RevealFile 在文件管理器中定位工作区内的文件（打开所在目录并选中）。
//
// 请求：GET /api/v1/files/reveal?chat_id=<id>&path=<相对工作区路径>
// path 与 download / open 一样经 Manager.Resolve 做沙箱校验。
func RevealFile(c *gin.Context) {
	reqID := ensureReqID(c)

	if wsManager == nil {
		writeError(c, http.StatusInternalServerError, "工作区未初始化", reqID)
		return
	}

	chatID := strings.TrimSpace(c.Query("chat_id"))
	relPath := strings.TrimSpace(c.Query("path"))
	if chatID == "" || relPath == "" {
		writeError(c, http.StatusBadRequest, "缺少 chat_id 或 path", reqID)
		return
	}

	wsDir := wsManager.Dir(chatID)
	absPath, err := wsManager.Resolve(wsDir, relPath)
	if err != nil {
		log.Printf("[file-handler][%s] reject reveal path chat=%s path=%s: %v", reqID, chatID, relPath, err)
		writeError(c, http.StatusBadRequest, "非法路径: "+err.Error(), reqID)
		return
	}

	st, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			writeError(c, http.StatusNotFound, "文件不存在", reqID)
			return
		}
		log.Printf("[file-handler][%s] stat failed: %v", reqID, err)
		writeError(c, http.StatusInternalServerError, "读取文件失败", reqID)
		return
	}
	if st.IsDir() {
		writeError(c, http.StatusBadRequest, "path 指向目录，不是文件", reqID)
		return
	}

	if err := revealLocal(absPath); err != nil {
		log.Printf("[file-handler][%s] reveal failed: %v", reqID, err)
		writeError(c, http.StatusInternalServerError, "定位文件失败: "+err.Error(), reqID)
		return
	}

	log.Printf("[file-handler][%s] revealed chat=%s path=%s", reqID, chatID, relPath)
	c.JSON(http.StatusOK, CommonResponse{Code: 0, Msg: "ok", ReqID: reqID})
}

// OpenWorkspace 在文件管理器中打开当前对话的工作空间目录。
//
// 请求：GET /api/v1/files/open-workspace?chat_id=<id>
func OpenWorkspace(c *gin.Context) {
	reqID := ensureReqID(c)

	if wsManager == nil {
		writeError(c, http.StatusInternalServerError, "工作区未初始化", reqID)
		return
	}

	chatID := strings.TrimSpace(c.Query("chat_id"))
	if chatID == "" {
		writeError(c, http.StatusBadRequest, "缺少 chat_id", reqID)
		return
	}

	// 校验对话存在，避免打开不存在的目录
	convDAO := &dao.ConversationDAO{}
	conv, err := convDAO.GetConversationByID(c.Request.Context(), chatID)
	if err != nil {
		log.Printf("[file-handler][%s] get conversation failed: %v", reqID, err)
		writeError(c, http.StatusInternalServerError, "查询对话失败", reqID)
		return
	}
	if conv == nil {
		writeError(c, http.StatusNotFound, "对话不存在", reqID)
		return
	}

	wsDir := wsManager.Dir(chatID)
	if err := openDirLocal(wsDir); err != nil {
		log.Printf("[file-handler][%s] open workspace failed: %v", reqID, err)
		writeError(c, http.StatusInternalServerError, "打开工作空间失败: "+err.Error(), reqID)
		return
	}

	log.Printf("[file-handler][%s] opened workspace chat=%s dir=%s", reqID, chatID, wsDir)
	c.JSON(http.StatusOK, CommonResponse{Code: 0, Msg: "ok", ReqID: reqID})
}

// openLocal 用系统默认应用打开本地文件，按平台分发命令。
//
// 用 Start 而非 Run：open / xdg-open 把文件交给默认应用后立即返回，
// 无需等待应用关闭；也不接管其 stdout/stderr（避免阻塞 HTTP 响应）。
func openLocal(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}

// revealLocal 在文件管理器中定位文件（打开所在目录并选中文件）。
//
// macOS 用 open -R；Windows 用 explorer /select,；Linux 没有「选中文件」
// 的通用命令，退化为打开父目录。
func revealLocal(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", "-R", path)
	case "windows":
		cmd = exec.Command("explorer", "/select,"+path)
	default:
		cmd = exec.Command("xdg-open", filepath.Dir(path))
	}
	return cmd.Start()
}

// openDirLocal 在文件管理器中打开目录。
func openDirLocal(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("explorer", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	return cmd.Start()
}

// ============================================================
// 内部辅助
// ============================================================

// ensureReqID 取出或生成请求 ID（Wrap 之外的裸 handler 复用同一套 req_id 语义）。
func ensureReqID(c *gin.Context) string {
	reqID := c.GetHeader("X-Request-ID")
	if reqID == "" {
		reqID = c.GetString("req_id")
	}
	if reqID == "" {
		reqID = uuid.New().String()
	}
	c.Set("req_id", reqID)
	return reqID
}

// buildDownloadURL 拼出该文件的下载地址（相对路径，由前端补 origin）。
func buildDownloadURL(chatID, relPath string) string {
	q := url.Values{}
	q.Set("chat_id", chatID)
	q.Set("path", relPath)
	return "/api/v1/files/download?" + q.Encode()
}

// sanitizeUploadName 把上传文件名清洗成安全的 basename。
//
// 处理三类风险：
//   - 目录穿越：只取 basename（同时兼容 Windows 的 \ 分隔）
//   - 隐藏文件 / 空名：回落到 upload 前缀
//   - 路径分隔符残留：统一替换为下划线
func sanitizeUploadName(raw string) string {
	name := strings.TrimSpace(raw)
	// Windows 客户端可能传 C:\dir\file.txt，filepath.Base 在 unix 上不识别 \
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, string(filepath.Separator), "_")

	// 去掉开头的点，避免生成隐藏文件（产物扫描会跳过隐藏文件）
	name = strings.TrimLeft(name, ".")
	if name == "" {
		return "upload-" + uuid.New().String()[:8]
	}
	// 控制长度，避免超出文件系统上限
	if len([]rune(name)) > 120 {
		ext := filepath.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		r := []rune(stem)
		if len(r) > 100 {
			stem = string(r[:100])
		}
		name = stem + ext
	}
	return name
}

// uniqueFileName 在 dir 下为 name 找一个不冲突的文件名。
// 冲突时在扩展名前插入短 uuid 片段，保留原名与扩展名便于模型理解用途。
func uniqueFileName(dir, name string) string {
	if _, err := os.Stat(filepath.Join(dir, name)); os.IsNotExist(err) {
		return name
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 5; i++ {
		candidate := fmt.Sprintf("%s-%s%s", stem, uuid.New().String()[:6], ext)
		if _, err := os.Stat(filepath.Join(dir, candidate)); os.IsNotExist(err) {
			return candidate
		}
	}
	// 极端情况下用时间戳兜底
	return fmt.Sprintf("%s-%d%s", stem, time.Now().UnixNano(), ext)
}

// sanitizeHeaderName 去掉文件名里会破坏 HTTP header 的字符（引号、换行）。
func sanitizeHeaderName(name string) string {
	r := strings.NewReplacer(`"`, "", "\r", "", "\n", "")
	return r.Replace(name)
}
