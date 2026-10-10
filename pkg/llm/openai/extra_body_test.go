package openai

import (
	"encoding/json"
	"testing"

	"github.com/memory-overflow/flowagent/pkg/llm"
)

// TestMergeExtraBody_NoExtra 未指定额外字段时请求体原样返回。
//
// 这是默认值：私有字段一律按需注入，绝大多数调用不该有任何额外字段。
func TestMergeExtraBody_NoExtra(t *testing.T) {
	in := []byte(`{"model":"hy4-preview","messages":[],"stream":false}`)
	out, err := mergeExtraBody(in, nil)
	if err != nil {
		t.Fatalf("mergeExtraBody failed: %v", err)
	}
	if string(out) != string(in) {
		t.Fatalf("expected unchanged payload, got %s", out)
	}
}

// TestMergeExtraBody_Injects 额外字段被合并进请求体，且原有字段不丢。
func TestMergeExtraBody_Injects(t *testing.T) {
	in := []byte(`{"model":"hy4-preview","messages":[],"temperature":0.2}`)
	out, err := mergeExtraBody(in, map[string]interface{}{
		"chat_template_kwargs": map[string]interface{}{"reasoning_effort": "no_think"},
	})
	if err != nil {
		t.Fatalf("mergeExtraBody failed: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal merged payload: %v", err)
	}
	for _, k := range []string{"model", "messages", "temperature"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("original field %q lost after merge: %s", k, out)
		}
	}
	raw, ok := got["chat_template_kwargs"]
	if !ok {
		t.Fatalf("extra field not injected: %s", out)
	}
	var kwargs map[string]string
	if err := json.Unmarshal(raw, &kwargs); err != nil {
		t.Fatalf("unmarshal chat_template_kwargs: %v", err)
	}
	if kwargs["reasoning_effort"] != "no_think" {
		t.Fatalf("reasoning_effort = %q, want no_think", kwargs["reasoning_effort"])
	}
}

// TestMergeExtraBody_Overrides 同名字段以 ExtraBody 为准：
// 调用方显式指定的意图强于结构体默认值。
func TestMergeExtraBody_Overrides(t *testing.T) {
	in := []byte(`{"model":"hy4-preview","max_tokens":100}`)
	out, err := mergeExtraBody(in, map[string]interface{}{"max_tokens": 4096})
	if err != nil {
		t.Fatalf("mergeExtraBody failed: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal merged payload: %v", err)
	}
	if got["max_tokens"] != float64(4096) {
		t.Fatalf("max_tokens = %v, want 4096", got["max_tokens"])
	}
}

// TestOptions_ExtraBodyDefaultZero llm.Options 默认不带额外字段 ——
// 保证「不传私有参数」是默认行为，只有显式设置的调用才会注入。
func TestOptions_ExtraBodyDefaultZero(t *testing.T) {
	if opts := llm.DefaultOptions(); opts.ExtraBody != nil {
		t.Fatalf("DefaultOptions.ExtraBody = %v, want nil", opts.ExtraBody)
	}
}
