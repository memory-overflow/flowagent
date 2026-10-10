// Package skill — 内置 Skill 清单加载
//
// 从项目 skills/builtins/*.json 加载预置的 Skill 清单，供「一键导入」使用。
// 清单只是模板，导入时会为每条记录生成新的 UUID 并写入 skill 表。
package skill

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/memory-overflow/flowagent/model"
)

// BuiltinManifest 一个内置 Skill 的清单
type BuiltinManifest struct {
	Key         string            `json:"key"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	// NameEn/DescriptionEn 英文展示名与描述（仅前端英文界面用；给 LLM 的
	// skill 注入始终用中文 Name/Description）。i-have-adhd / shell 等名字
	// 本身是技术标识，没有 name_en，展示时回落原文。
	NameEn        string          `json:"name_en,omitempty"`
	DescriptionEn string          `json:"description_en,omitempty"`
	Type          model.SkillType `json:"type"` // 1=prompt 2=command
	Content       string          `json:"content"`
	Command       string          `json:"command,omitempty"`
	Args          []string        `json:"args,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
}

// LoadBuiltins 读取指定目录下的所有 *.json 内置清单。
// 目录不存在返回空切片；单个文件解析失败会跳过并在日志中提示。
func LoadBuiltins(dir string) ([]*BuiltinManifest, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("skill: read builtins dir %s: %w", dir, err)
	}

	var list []*BuiltinManifest
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("skill: read %s: %w", p, err)
		}
		var m BuiltinManifest
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("skill: parse %s: %w", p, err)
		}
		if m.Key == "" {
			m.Key = strings.TrimSuffix(e.Name(), ".json")
		}
		if m.Type == 0 {
			m.Type = model.SkillTypePrompt
		}
		list = append(list, &m)
	}

	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
	return list, nil
}

// ExpandEnv 展开 args / env / content 中的 ${VAR} 占位符（用 os.ExpandEnv）。
// 返回新副本，不修改入参。
func (m *BuiltinManifest) ExpandEnv() *BuiltinManifest {
	out := *m
	if len(m.Args) > 0 {
		out.Args = make([]string, len(m.Args))
		for i, a := range m.Args {
			out.Args[i] = os.ExpandEnv(a)
		}
	}
	if len(m.Env) > 0 {
		out.Env = make(map[string]string, len(m.Env))
		for k, v := range m.Env {
			out.Env[k] = os.ExpandEnv(v)
		}
	}
	out.Content = os.ExpandEnv(m.Content)
	out.Command = os.ExpandEnv(m.Command)
	return &out
}
