// Package gemini — 工具 schema 清洗
//
// Gemini 的 function_declarations.parameters 只接受 OpenAPI 3.0 Schema 的一个子集，
// 而 MCP server 普遍返回完整的 JSON Schema（draft 2020-12）。
// 直接透传会被 Gemini 以 400 拒绝，例如：
//
//	Unknown name "propertyNames" at 'tools[0].function_declarations[6].parameters...'
//
// 因此在发请求前把 schema 收敛到 Gemini 支持的字段。
//
// 用白名单而非黑名单：Gemini 对未知字段一律 400，新出现的 JSON Schema 关键字
// （$defs / $ref / patternProperties / unevaluatedProperties ...）用黑名单永远追不完，
// 白名单则默认安全。
//
// 这是 Gemini 特有的限制，不是 MCP 的问题，所以清洗放在 provider 层，
// 保持 pkg/mcp 返回原始 schema，其他 provider（如 OpenAI）仍可用完整 schema。
package gemini

import (
	"encoding/json"
	"strings"
)

// geminiSchemaFields Gemini Schema 接受的字段白名单。
// 参见 https://ai.google.dev/api/caching#Schema
var geminiSchemaFields = map[string]bool{
	"type":             true,
	"format":           true,
	"title":            true,
	"description":      true,
	"nullable":         true,
	"enum":             true,
	"items":            true,
	"properties":       true,
	"required":         true,
	"minimum":          true,
	"maximum":          true,
	"minLength":        true,
	"maxLength":        true,
	"minItems":         true,
	"maxItems":         true,
	"minProperties":    true,
	"maxProperties":    true,
	"pattern":          true,
	"example":          true,
	"default":          true,
	"anyOf":            true,
	"propertyOrdering": true,
}

// sanitizeToolSchema 把 JSON Schema 收敛为 Gemini 可接受的形态。
// 入参不会被修改（深拷贝语义），因为同一份 schema 可能被其他 provider 复用。
func sanitizeToolSchema(schema map[string]interface{}) map[string]interface{} {
	// 根节点必须是 object，不能走自由 map 降级
	cleaned, ok := sanitizeNode(schema, true).(map[string]interface{})
	if !ok || len(cleaned) == 0 {
		// 无参数工具：Gemini 要求 parameters 至少是个合法 object
		return map[string]interface{}{
			"type":       "object",
			"properties": map[string]interface{}{},
		}
	}
	cleaned["type"] = "object"
	if _, has := cleaned["properties"]; !has {
		cleaned["properties"] = map[string]interface{}{}
	}
	return cleaned
}

// sanitizeNode 递归清洗单个 schema 节点。
// isRoot 为 true 时不做自由 map 降级 —— 顶层 parameters 必须是 object。
func sanitizeNode(node interface{}, isRoot bool) interface{} {
	m, ok := node.(map[string]interface{})
	if !ok {
		return node
	}

	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		if !geminiSchemaFields[k] {
			continue
		}
		switch k {
		case "properties":
			// properties 的每个 value 本身是 schema，逐个递归
			props, isMap := v.(map[string]interface{})
			if !isMap {
				continue
			}
			cleanedProps := make(map[string]interface{}, len(props))
			for name, sub := range props {
				cleanedProps[name] = sanitizeNode(sub, false)
			}
			out[k] = cleanedProps

		case "items":
			out[k] = sanitizeNode(v, false)

		case "anyOf":
			arr, isArr := v.([]interface{})
			if !isArr {
				continue
			}
			cleaned := make([]interface{}, 0, len(arr))
			for _, sub := range arr {
				cleaned = append(cleaned, sanitizeNode(sub, false))
			}
			out[k] = cleaned

		default:
			out[k] = v
		}
	}

	// 形如 {"type":"object","additionalProperties":{"type":"string"}} 的自由 map：
	// additionalProperties 被剔除后就成了没有任何 properties 的 object，
	// Gemini 会当成「无字段对象」，LLM 无法往里填内容，工具等于不可用。
	// 降级成 string 让模型传 JSON 文本，工具至少还能被调用。
	if !isRoot && isObjectType(out["type"]) && !hasProperties(out) {
		freeform := map[string]interface{}{"type": "string"}
		freeform["description"] = strings.TrimSpace(
			describeOf(out) + "（请传入 JSON 对象字符串）")
		return freeform
	}

	return out
}

// isObjectType 判断 type 是否为 object（兼容大小写）
func isObjectType(t interface{}) bool {
	s, ok := t.(string)
	return ok && strings.EqualFold(s, "object")
}

// hasProperties 判断 object 是否声明了至少一个字段
func hasProperties(m map[string]interface{}) bool {
	props, ok := m["properties"].(map[string]interface{})
	return ok && len(props) > 0
}

// describeOf 取节点的描述文本
func describeOf(m map[string]interface{}) string {
	if d, ok := m["description"].(string); ok {
		return d
	}
	return ""
}

// schemaDebugString 用于日志排查，输出清洗后的 schema
func schemaDebugString(schema map[string]interface{}) string {
	b, err := json.Marshal(schema)
	if err != nil {
		return "<marshal error>"
	}
	return string(b)
}
