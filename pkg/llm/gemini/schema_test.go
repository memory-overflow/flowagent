package gemini

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustSchema(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	return m
}

// collectKeys 递归收集 schema 里出现的所有 key
func collectKeys(node interface{}, out map[string]bool) {
	switch v := node.(type) {
	case map[string]interface{}:
		for k, sub := range v {
			out[k] = true
			// properties 下的 key 是字段名而非 schema 关键字，只递归其 value
			if k == "properties" {
				if props, ok := sub.(map[string]interface{}); ok {
					for _, ps := range props {
						collectKeys(ps, out)
					}
					continue
				}
			}
			collectKeys(sub, out)
		}
	case []interface{}:
		for _, sub := range v {
			collectKeys(sub, out)
		}
	}
}

// TestSanitize_RemovesUnsupportedKeywords 复现线上 400 报错的真实 schema
// （playwright browser_drop），验证不兼容关键字被清除
func TestSanitize_RemovesUnsupportedKeywords(t *testing.T) {
	raw := `{
      "$schema": "https://json-schema.org/draft/2020-12/schema",
      "type": "object",
      "properties": {
        "element": {"description": "el desc", "type": "string"},
        "target": {"type": "string", "description": "target desc"},
        "paths": {"description": "paths", "type": "array", "items": {"type": "string"}},
        "data": {
          "description": "Data to drop",
          "type": "object",
          "propertyNames": {"type": "string"},
          "additionalProperties": {"type": "string"}
        }
      },
      "required": ["target"],
      "additionalProperties": false
    }`

	got := sanitizeToolSchema(mustSchema(t, raw))

	keys := map[string]bool{}
	collectKeys(got, keys)
	for _, bad := range []string{"$schema", "additionalProperties", "propertyNames"} {
		if keys[bad] {
			t.Errorf("unsupported keyword %q should be removed, got schema: %s",
				bad, schemaDebugString(got))
		}
	}

	// 有用的信息必须保留
	props, _ := got["properties"].(map[string]interface{})
	for _, want := range []string{"element", "target", "paths", "data"} {
		if _, ok := props[want]; !ok {
			t.Errorf("property %q lost after sanitize", want)
		}
	}
	if req, ok := got["required"].([]interface{}); !ok || len(req) != 1 {
		t.Errorf("required lost, got %v", got["required"])
	}
	// 数组元素类型要保留
	paths, _ := props["paths"].(map[string]interface{})
	if items, ok := paths["items"].(map[string]interface{}); !ok || items["type"] != "string" {
		t.Errorf("items type lost: %v", paths["items"])
	}
}

// TestSanitize_FreeformMapDegradesToString 自由 map 剔除 additionalProperties 后
// 会变成无字段 object（LLM 填不进内容），应降级为 string
func TestSanitize_FreeformMapDegradesToString(t *testing.T) {
	raw := `{
      "type": "object",
      "properties": {
        "data": {
          "description": "MIME map",
          "type": "object",
          "additionalProperties": {"type": "string"}
        }
      }
    }`
	got := sanitizeToolSchema(mustSchema(t, raw))
	props := got["properties"].(map[string]interface{})
	data := props["data"].(map[string]interface{})

	if data["type"] != "string" {
		t.Fatalf("freeform map should degrade to string, got type=%v", data["type"])
	}
	desc, _ := data["description"].(string)
	if !strings.Contains(desc, "MIME map") {
		t.Errorf("original description lost: %q", desc)
	}
	if !strings.Contains(desc, "JSON") {
		t.Errorf("description should hint JSON string, got %q", desc)
	}
}

// TestSanitize_RootNeverDegrades 根节点即使无 properties 也必须保持 object，
// 否则 Gemini 会拒绝 parameters
func TestSanitize_RootNeverDegrades(t *testing.T) {
	cases := []string{
		`{"type":"object","additionalProperties":false}`,
		`{"type":"object","properties":{}}`,
		`{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}`,
		`{}`,
	}
	for _, raw := range cases {
		got := sanitizeToolSchema(mustSchema(t, raw))
		if got["type"] != "object" {
			t.Errorf("root type should stay object for %s, got %v", raw, got["type"])
		}
		if _, ok := got["properties"].(map[string]interface{}); !ok {
			t.Errorf("root should have properties map for %s, got %v", raw, got["properties"])
		}
	}
}

// TestSanitize_NilSchema 无参数工具
func TestSanitize_NilSchema(t *testing.T) {
	got := sanitizeToolSchema(nil)
	if got["type"] != "object" {
		t.Errorf("nil schema should produce object, got %v", got["type"])
	}
	if _, ok := got["properties"].(map[string]interface{}); !ok {
		t.Errorf("nil schema should produce empty properties, got %v", got["properties"])
	}
}

// TestSanitize_PreservesSupportedFields 白名单内字段不能被误删
func TestSanitize_PreservesSupportedFields(t *testing.T) {
	raw := `{
      "type": "object",
      "properties": {
        "level": {"type": "string", "enum": ["a","b"], "default": "a"},
        "index": {"type": "number", "minimum": 1, "maximum": 10},
        "name":  {"type": "string", "minLength": 1, "pattern": "^x"},
        "tags":  {"type": "array", "items": {"type":"string"}, "minItems": 1}
      },
      "required": ["level"]
    }`
	got := sanitizeToolSchema(mustSchema(t, raw))
	props := got["properties"].(map[string]interface{})

	level := props["level"].(map[string]interface{})
	if _, ok := level["enum"]; !ok {
		t.Error("enum lost")
	}
	if level["default"] != "a" {
		t.Error("default lost")
	}
	idx := props["index"].(map[string]interface{})
	if idx["minimum"] == nil || idx["maximum"] == nil {
		t.Error("minimum/maximum lost")
	}
	name := props["name"].(map[string]interface{})
	if name["pattern"] == nil || name["minLength"] == nil {
		t.Error("pattern/minLength lost")
	}
}

// TestSanitize_DoesNotMutateInput 同一份 schema 可能被其他 provider 复用，
// 清洗不能改动原始对象
func TestSanitize_DoesNotMutateInput(t *testing.T) {
	raw := `{"$schema":"x","type":"object","properties":{"a":{"type":"string"}},"additionalProperties":false}`
	in := mustSchema(t, raw)
	before, _ := json.Marshal(in)

	_ = sanitizeToolSchema(in)

	after, _ := json.Marshal(in)
	if string(before) != string(after) {
		t.Errorf("input schema mutated:\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestSanitize_NestedAnyOf anyOf 分支内也要递归清洗
func TestSanitize_NestedAnyOf(t *testing.T) {
	raw := `{
      "type": "object",
      "properties": {
        "v": {"anyOf": [
          {"type":"string","$schema":"x"},
          {"type":"object","additionalProperties":true,"properties":{"k":{"type":"number","propertyNames":{}}}}
        ]}
      }
    }`
	got := sanitizeToolSchema(mustSchema(t, raw))
	keys := map[string]bool{}
	collectKeys(got, keys)
	for _, bad := range []string{"$schema", "additionalProperties", "propertyNames"} {
		if keys[bad] {
			t.Errorf("keyword %q survived inside anyOf: %s", bad, schemaDebugString(got))
		}
	}
}
