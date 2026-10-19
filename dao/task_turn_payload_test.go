package dao

import (
	"encoding/json"
	"strings"
	"testing"
)

// marshalTurnPayload 必须关闭 HTML 转义：json.Marshal 默认把 & 写成 \u0026，
// run_command 参数里的 && 串联符落库后显示的不是模型真实发送的命令。
func TestMarshalTurnPayloadKeepsAmpersand(t *testing.T) {
	args := map[string]interface{}{"cmd": "echo %CD% && dir /a"}
	s, err := marshalTurnPayload(args)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(s, `\u0026`) {
		t.Errorf("不应出现 HTML 转义: %s", s)
	}
	if !strings.Contains(s, "&&") {
		t.Errorf("&& 应保持原样: %s", s)
	}
	// 输出仍是合法 JSON 且能无损还原
	var back map[string]interface{}
	if err := json.Unmarshal([]byte(s), &back); err != nil {
		t.Fatalf("输出应为合法 JSON: %v (%s)", err, s)
	}
	if back["cmd"] != "echo %CD% && dir /a" {
		t.Errorf("round-trip 不一致: %v", back["cmd"])
	}
}
