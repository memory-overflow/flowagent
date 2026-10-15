package handler

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/memory-overflow/flowagent/dao"
	"github.com/memory-overflow/flowagent/model"
	pkgskill "github.com/memory-overflow/flowagent/pkg/skill"
)

// skillsBuiltinsDir 返回内置 Skill 清单目录。优先读 env SKILLS_BUILTINS_DIR，
// 否则回落到相对工作目录的 ./skills/builtins。
func skillsBuiltinsDir() string {
	if v := os.Getenv("SKILLS_BUILTINS_DIR"); v != "" {
		return v
	}
	return "./skills/builtins"
}

// skillsRootDir 返回 skill 脚本落地根目录（zip 导入的脚本解压到这里）。
// 优先读 env SKILLS_DIR，否则回落到相对工作目录的 skills/user —— 用户自定义
// skill 目录，已被 .gitignore 忽略，上传的 skill 不进仓库。
func skillsRootDir() string {
	if v := os.Getenv("SKILLS_DIR"); v != "" {
		return v
	}
	return pkgskill.UserSkillsRelDir
}

// ============================================================
// 请求 / 响应结构体
// ============================================================

type ListSkillsRequest struct {
	Keyword string `json:"keyword"`
}

type SkillItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content,omitempty"`
	CreateTs    int64  `json:"create_ts"`
	UpdateTs    int64  `json:"update_ts"`
	// 展示与数据分离：name/description 永远是中文原文（编辑回填、英文界面回落值），
	// *_display 按请求语言折算。列表渲染用 *_display，编辑表单回填用原文。
	NameDisplay string `json:"name_display"`
	DescDisplay string `json:"description_display"`
}

type ListSkillsResponse struct {
	List []*SkillItem `json:"list"`
}

type SkillDetailRequest struct {
	SkillID string `json:"skill_id"`
}

type CreateSkillRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

type UpdateSkillRequest struct {
	SkillID     string `json:"skill_id"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Content     string `json:"content,omitempty"`
}

type DeleteSkillRequest struct {
	SkillID string `json:"skill_id"`
}

// ============================================================
// Handler 函数
// ============================================================

// ListSkills 获取 Skill 列表
func ListSkills(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*ListSkillsRequest)

	skillDAO := &dao.SkillDAO{}
	skills, err := skillDAO.ListSkills(ctx, r.Keyword)
	if err != nil {
		return nil, err
	}

	list := make([]*SkillItem, 0, len(skills))
	for _, s := range skills {
		list = append(list, buildSkillItemLocalized(ctx, s))
	}
	return &ListSkillsResponse{List: list}, nil
}

func ListSkillsBinder() interface{} { return &ListSkillsRequest{} }

// GetSkillDetail 获取 Skill 详情
func GetSkillDetail(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*SkillDetailRequest)

	skillDAO := &dao.SkillDAO{}
	skill, err := skillDAO.GetSkillByID(ctx, r.SkillID)
	if err != nil {
		return nil, err
	}
	if skill == nil {
		return nil, &APIError{Code: http.StatusNotFound, Msg: "Skill 不存在"}
	}
	return buildSkillItemLocalized(ctx, skill), nil
}

func GetSkillDetailBinder() interface{} { return &SkillDetailRequest{} }

// CreateSkill 创建 Skill
func CreateSkill(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*CreateSkillRequest)

	if r.Name == "" {
		return nil, &APIError{Code: http.StatusBadRequest, Msg: "name 不能为空"}
	}

	now := time.Now()
	skill := &model.Skill{
		ID:          uuid.New().String(),
		Name:        r.Name,
		Description: r.Description,
		Content:     r.Content,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	skillDAO := &dao.SkillDAO{}
	if err := skillDAO.CreateSkill(ctx, skill); err != nil {
		return nil, err
	}
	return buildSkillItemLocalized(ctx, skill), nil
}

func CreateSkillBinder() interface{} { return &CreateSkillRequest{} }

// UpdateSkill 更新 Skill
func UpdateSkill(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*UpdateSkillRequest)

	updates := map[string]interface{}{}
	if r.Name != "" {
		updates["name"] = r.Name
	}
	if r.Description != "" {
		updates["description"] = r.Description
	}
	if r.Content != "" {
		updates["content"] = r.Content
	}
	updates["updated_time"] = time.Now()

	skillDAO := &dao.SkillDAO{}
	if err := skillDAO.UpdateSkill(ctx, r.SkillID, updates); err != nil {
		return nil, err
	}

	skill, _ := skillDAO.GetSkillByID(ctx, r.SkillID)
	if skill != nil {
		return buildSkillItemLocalized(ctx, skill), nil
	}
	return nil, nil
}

func UpdateSkillBinder() interface{} { return &UpdateSkillRequest{} }

// DeleteSkill 删除 Skill
func DeleteSkill(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*DeleteSkillRequest)

	skillDAO := &dao.SkillDAO{}
	if err := skillDAO.DeleteSkill(ctx, r.SkillID); err != nil {
		return nil, err
	}
	return nil, nil
}

func DeleteSkillBinder() interface{} { return &DeleteSkillRequest{} }

// ============================================================
// 辅助函数
// ============================================================

func buildSkillItem(s *model.Skill) *SkillItem {
	return &SkillItem{
		ID:          s.ID,
		Name:        s.Name,
		Description: s.Description,
		Content:     s.Content,
		CreateTs:    s.CreatedAt.UnixMilli(),
		UpdateTs:    s.UpdatedAt.UnixMilli(),
	}
}

// buildSkillItemLocalized 在 buildSkillItem 基础上按界面语言填展示字段。
// lang=en 且有英文翻译时 display 取英文，否则回落中文原文。
func buildSkillItemLocalized(c *gin.Context, s *model.Skill) *SkillItem {
	item := buildSkillItem(s)
	lang := LangFromContext(c)
	item.NameDisplay = PickDisplay(lang, s.Name, s.NameEn)
	item.DescDisplay = PickDisplay(lang, s.Description, s.DescriptionEn)
	return item
}

// ============================================================
// 内置 Skill 清单：列表 & 一键导入
// ============================================================

// BuiltinSkillItem 前端展示用的内置 Skill 条目
type BuiltinSkillItem struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// NameDisplay/DescDisplay 按请求语言折算的展示值（en 有翻译用翻译，否则回落中文）
	NameDisplay string `json:"name_display"`
	DescDisplay string `json:"description_display"`
	Content     string `json:"content,omitempty"`
	// AlreadyImported 数据库里已有同名记录（按 name 判重）
	AlreadyImported bool `json:"already_imported"`
}

type ListBuiltinSkillRequest struct{}

type ListBuiltinSkillResponse struct {
	Dir  string              `json:"dir"`
	List []*BuiltinSkillItem `json:"list"`
}

// ListBuiltinSkill 列出项目 skills/builtins/ 目录下的所有内置 Skill 清单。
func ListBuiltinSkill(ctx *gin.Context, req interface{}) (interface{}, error) {
	dir := skillsBuiltinsDir()
	manifests, err := pkgskill.LoadBuiltins(dir)
	if err != nil {
		return nil, err
	}

	existing, _ := (&dao.SkillDAO{}).ListSkills(ctx, "")
	nameSet := make(map[string]bool, len(existing))
	for _, s := range existing {
		nameSet[s.Name] = true
	}

	list := make([]*BuiltinSkillItem, 0, len(manifests))
	lang := LangFromContext(ctx)
	for _, m := range manifests {
		e := m.ExpandEnv()
		list = append(list, &BuiltinSkillItem{
			Key:             m.Key,
			Name:            m.Name,
			Description:     m.Description,
			NameDisplay:     PickDisplay(lang, m.Name, m.NameEn),
			DescDisplay:     PickDisplay(lang, m.Description, m.DescriptionEn),
			Content:         e.Content,
			AlreadyImported: nameSet[m.Name],
		})
	}
	return &ListBuiltinSkillResponse{Dir: dir, List: list}, nil
}

func ListBuiltinSkillBinder() interface{} { return &ListBuiltinSkillRequest{} }

type ImportBuiltinSkillRequest struct {
	Keys      []string `json:"keys"`
	Overwrite bool     `json:"overwrite"`
}

type ImportBuiltinSkillResponse struct {
	Imported []string `json:"imported"`
	Updated  []string `json:"updated"`
	Skipped  []string `json:"skipped"`
}

// ImportBuiltinSkill 一键把内置清单导入 skill 表。
func ImportBuiltinSkill(ctx *gin.Context, req interface{}) (interface{}, error) {
	r := req.(*ImportBuiltinSkillRequest)

	manifests, err := pkgskill.LoadBuiltins(skillsBuiltinsDir())
	if err != nil {
		return nil, err
	}
	if len(manifests) == 0 {
		return &ImportBuiltinSkillResponse{
			Imported: []string{},
			Updated:  []string{},
			Skipped:  []string{},
		}, nil
	}

	keySet := map[string]bool{}
	for _, k := range r.Keys {
		keySet[k] = true
	}
	filter := func(m *pkgskill.BuiltinManifest) bool {
		if len(keySet) == 0 {
			return true
		}
		return keySet[m.Key]
	}

	skillDAO := &dao.SkillDAO{}
	existing, err := skillDAO.ListSkills(ctx, "")
	if err != nil {
		return nil, err
	}
	byName := make(map[string]*model.Skill, len(existing))
	for _, s := range existing {
		byName[s.Name] = s
	}

	resp := &ImportBuiltinSkillResponse{
		Imported: []string{},
		Updated:  []string{},
		Skipped:  []string{},
	}
	for _, m := range manifests {
		if !filter(m) {
			continue
		}
		expanded := m.ExpandEnv()

		if old, ok := byName[m.Name]; ok {
			if !r.Overwrite {
				resp.Skipped = append(resp.Skipped, m.Name)
				continue
			}
			updates := map[string]interface{}{
				"description":    m.Description,
				"description_en": m.DescriptionEn,
				"name_en":        m.NameEn,
				"content":        expanded.Content,
				"updated_time":   time.Now(),
			}
			if err := skillDAO.UpdateSkill(ctx, old.ID, updates); err != nil {
				return nil, fmt.Errorf("update %s: %w", m.Name, err)
			}
			resp.Updated = append(resp.Updated, m.Name)
			continue
		}

		now := time.Now()
		s := &model.Skill{
			ID:            uuid.New().String(),
			Name:          m.Name,
			Description:   m.Description,
			NameEn:        m.NameEn,
			DescriptionEn: m.DescriptionEn,
			Content:       expanded.Content,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if err := skillDAO.CreateSkill(ctx, s); err != nil {
			return nil, fmt.Errorf("create %s: %w", m.Name, err)
		}
		resp.Imported = append(resp.Imported, m.Name)
	}

	return resp, nil
}

func ImportBuiltinSkillBinder() interface{} { return &ImportBuiltinSkillRequest{} }

// ============================================================
// Skill 包 zip 整体导入
// ============================================================

// SkillZipImportItem zip 导入里单条 skill 的结果
type SkillZipImportItem struct {
	Skill      *SkillItem `json:"skill"`
	Key        string     `json:"key"`
	Action     string     `json:"action"` // created / updated / skipped
	FilesInZip int        `json:"files_in_zip"`
	ScriptsDir string     `json:"scripts_dir"`
}

// SkillZipImportResult zip 导入整体结果（集合包可能一次导入多条）
type SkillZipImportResult struct {
	Skills     []*SkillZipImportItem `json:"skills"`
	Total      int                   `json:"total"`
	TotalFiles int                   `json:"total_files"`
}

// ImportSkillZip 接收一个 skill 包 zip（multipart），解析并整体导入。
//
// 支持三种布局：本项目 skill.json 单包、社区 SKILL.md 单包、社区 SKILL.md 集合包
// （根 SKILL.md + 多个子目录各自 SKILL.md，见 pkg/skill.AnalyzeSkillZip）。
//
// 表单字段：file（zip 文件）、overwrite（"true"/"1" 时同名覆盖，否则跳过）。
// 安全：zip-slip 路径穿越拒收、50MB/500 文件/单文件 20MB 上限。
func ImportSkillZip(c *gin.Context) {
	reqID := ensureReqID(c)

	fileHeader, err := c.FormFile("file")
	if err != nil {
		writeError(c, http.StatusBadRequest, "缺少 file 字段: "+err.Error(), reqID)
		return
	}
	overwrite := c.PostForm("overwrite") == "true" || c.PostForm("overwrite") == "1"

	tmp, err := os.CreateTemp("", "skill-import-*.zip")
	if err != nil {
		writeError(c, http.StatusInternalServerError, "创建临时文件失败: "+err.Error(), reqID)
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	src, err := fileHeader.Open()
	if err != nil {
		tmp.Close()
		writeError(c, http.StatusBadRequest, "读取上传文件失败: "+err.Error(), reqID)
		return
	}
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		writeError(c, http.StatusInternalServerError, "保存上传文件失败: "+err.Error(), reqID)
		return
	}
	tmp.Close()
	src.Close()

	// 1. 解析（可能拆出多条：集合包）
	items, err := pkgskill.AnalyzeSkillZip(tmpPath)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error(), reqID)
		return
	}

	// 2. 已有 skill 按 name 建索引（集合包内也可能重名，落地一个后要更新索引）
	skillDAO := &dao.SkillDAO{}
	existing, err := skillDAO.ListSkills(c.Request.Context(), "")
	if err != nil {
		writeError(c, http.StatusInternalServerError, "查询已有 Skill 失败: "+err.Error(), reqID)
		return
	}
	byName := make(map[string]*model.Skill, len(existing))
	for _, s := range existing {
		byName[s.Name] = s
	}

	// 3. 逐个落地脚本 + 写表
	result := &SkillZipImportResult{Skills: []*SkillZipImportItem{}}
	for _, item := range items {
		old := byName[item.Name]
		if old != nil && !overwrite {
			result.Skills = append(result.Skills, &SkillZipImportItem{
				Key:    item.Key,
				Action: "skipped",
				Skill:  buildSkillItem(old),
			})
			continue
		}

		scriptsDir := filepath.Join(skillsRootDir(), item.Key)
		filesInZip, err := pkgskill.ExtractItemFiles(item, scriptsDir)
		if err != nil {
			writeError(c, http.StatusBadRequest, err.Error(), reqID)
			return
		}
		result.TotalFiles += filesInZip

		now := time.Now()

		action := "created"
		var saved *model.Skill
		if old != nil {
			updates := map[string]interface{}{
				"description":  item.Description,
				"content":      item.Content,
				"updated_time": now,
			}
			if err := skillDAO.UpdateSkill(c.Request.Context(), old.ID, updates); err != nil {
				writeError(c, http.StatusInternalServerError, "更新 Skill 失败: "+err.Error(), reqID)
				return
			}
			saved, _ = skillDAO.GetSkillByID(c.Request.Context(), old.ID)
			action = "updated"
		} else {
			saved = &model.Skill{
				ID:          uuid.New().String(),
				Name:        item.Name,
				Description: item.Description,
				Content:     item.Content,
				CreatedAt:   now,
				UpdatedAt:   now,
			}
			if err := skillDAO.CreateSkill(c.Request.Context(), saved); err != nil {
				writeError(c, http.StatusInternalServerError, "创建 Skill 失败: "+err.Error(), reqID)
				return
			}
		}
		byName[item.Name] = saved

		result.Skills = append(result.Skills, &SkillZipImportItem{
			Skill:      buildSkillItem(saved),
			Key:        item.Key,
			Action:     action,
			FilesInZip: filesInZip,
			ScriptsDir: scriptsDir,
		})
	}
	result.Total = len(result.Skills)

	log.Printf("[skill-handler][%s] zip imported: %d skill(s), %d files",
		reqID, result.Total, result.TotalFiles)

	c.JSON(http.StatusOK, CommonResponse{Code: 0, Msg: "ok", ReqID: reqID, Data: result})
}

// ImportSkillMD 接收单个 SKILL.md 文件（multipart），解析为 skill。
//
// 与 ImportSkillZip 的「社区 SKILL.md 单包」等价，省去打包 zip：直接上传一个
// .md（YAML frontmatter: name/description + 正文）即可导入。
//
// 表单字段：file（.md 文件）、overwrite（"true"/"1" 时同名覆盖）。
func ImportSkillMD(c *gin.Context) {
	reqID := ensureReqID(c)

	fileHeader, err := c.FormFile("file")
	if err != nil {
		writeError(c, http.StatusBadRequest, "缺少 file 字段: "+err.Error(), reqID)
		return
	}
	overwrite := c.PostForm("overwrite") == "true" || c.PostForm("overwrite") == "1"

	src, err := fileHeader.Open()
	if err != nil {
		writeError(c, http.StatusBadRequest, "读取上传文件失败: "+err.Error(), reqID)
		return
	}
	defer src.Close()
	data, err := io.ReadAll(io.LimitReader(src, 1<<20)) // 单文件 1MB 上限
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取文件内容失败: "+err.Error(), reqID)
		return
	}

	name, desc, body, key, err := pkgskill.ParseSkillMarkdown(string(data))
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error(), reqID)
		return
	}

	// 落地 SKILL.md 到 skills/user/<key>/（与 zip 导入同目录）
	dir := filepath.Join(skillsRootDir(), key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(c, http.StatusInternalServerError, "创建 skill 目录失败: "+err.Error(), reqID)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), data, 0o644); err != nil {
		writeError(c, http.StatusInternalServerError, "写入 SKILL.md 失败: "+err.Error(), reqID)
		return
	}
	// ${CODEBUDDY_SKILL_DIR} 替换（与 zip 导入一致，见 mergeSkillPack）
	content := strings.ReplaceAll(body, "${CODEBUDDY_SKILL_DIR}", pkgskill.UserSkillsRelDir+"/"+key)

	// 写 skill 表
	skillDAO := &dao.SkillDAO{}
	existing, err := skillDAO.ListSkills(c.Request.Context(), "")
	if err != nil {
		writeError(c, http.StatusInternalServerError, "查询已有 Skill 失败: "+err.Error(), reqID)
		return
	}
	var old *model.Skill
	for _, s := range existing {
		if s.Name == name {
			old = s
			break
		}
	}

	now := time.Now()
	action := "created"
	var saved *model.Skill
	if old != nil {
		if !overwrite {
			c.JSON(http.StatusOK, CommonResponse{Code: 0, Msg: "ok", ReqID: reqID, Data: &SkillZipImportResult{
				Skills: []*SkillZipImportItem{{Skill: buildSkillItem(old), Key: key, Action: "skipped"}},
				Total:  1,
			}})
			return
		}
		updates := map[string]interface{}{
			"description":  desc,
			"content":      content,
			"updated_time": now,
		}
		if err := skillDAO.UpdateSkill(c.Request.Context(), old.ID, updates); err != nil {
			writeError(c, http.StatusInternalServerError, "更新 Skill 失败: "+err.Error(), reqID)
			return
		}
		saved, _ = skillDAO.GetSkillByID(c.Request.Context(), old.ID)
		action = "updated"
	} else {
		saved = &model.Skill{
			ID:          uuid.New().String(),
			Name:        name,
			Description: desc,
			Content:     content,
			CreatedAt:   now,
			UpdatedAt:   now,
		}
		if err := skillDAO.CreateSkill(c.Request.Context(), saved); err != nil {
			writeError(c, http.StatusInternalServerError, "创建 Skill 失败: "+err.Error(), reqID)
			return
		}
	}

	log.Printf("[skill-handler][%s] md imported: name=%s key=%s action=%s", reqID, name, key, action)
	c.JSON(http.StatusOK, CommonResponse{Code: 0, Msg: "ok", ReqID: reqID, Data: &SkillZipImportResult{
		Skills: []*SkillZipImportItem{{Skill: buildSkillItem(saved), Key: key, Action: action, ScriptsDir: dir}},
		Total:  1,
	}})
}
