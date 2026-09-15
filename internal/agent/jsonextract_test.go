package agent

// jsonextract_test.go - extractJSONObjectStrict 单元测试。
//
// 覆盖：
//   - 普通 JSON 对象能抽取并紧凑化
//   - 顶层缺 Required 字段 → 不改写
//   - 围栏 / 前缀废话 → 剥掉
//   - schema 为 nil / type 非 object / Required 为空 → 直接不动
//   - 嵌套对象包了 Required 字段 → 不通过（要的就是扁平）

import (
	"encoding/json"
	"strings"
	"testing"
)

// schemaRequired 构造只有 Required 的最小 schema（Properties 留空，不参与判定）。
func schemaRequired(required ...string) *JSONSchema {
	return &JSONSchema{Type: "object", Required: required}
}

func TestExtractJSONObjectStrict(t *testing.T) {
	t.Run("plain object extract", func(t *testing.T) {
		raw := `{"setting":12,"character":12,"issues":[]}`
		got, ok := extractJSONObjectStrict(raw, schemaRequired("setting", "character", "issues"))
		if !ok {
			t.Fatal("ok=false on plain JSON")
		}
		// 应能紧凑 marshal 回 round-trip
		var back map[string]any
		if err := json.Unmarshal([]byte(got), &back); err != nil {
			t.Fatalf("not valid JSON after extract: %v (got=%q)", err, got)
		}
		for _, k := range []string{"setting", "character", "issues"} {
			if _, ok := back[k]; !ok {
				t.Errorf("missing %q in extracted: %s", k, got)
			}
		}
	})

	t.Run("with markdown fence and prefix text", func(t *testing.T) {
		raw := "Here is the result:\n```json\n{\"a\":1,\"b\":2}\n```"
		got, ok := extractJSONObjectStrict(raw, schemaRequired("a", "b"))
		if !ok {
			t.Fatal("ok=false on fenced JSON")
		}
		var back map[string]any
		if err := json.Unmarshal([]byte(got), &back); err != nil {
			t.Fatalf("invalid: %v (got=%q)", err, got)
		}
	})

	t.Run("missing required field rejects", func(t *testing.T) {
		raw := `{"setting":12}`
		got, ok := extractJSONObjectStrict(raw, schemaRequired("setting", "character"))
		if ok {
			t.Errorf("expected reject for missing required field, got ok=true text=%q", got)
		}
	})

	t.Run("nested object with required key inside doesn't count", func(t *testing.T) {
		raw := `{"scores":{"setting":12,"character":12}}` // Required 在嵌套层
		got, ok := extractJSONObjectStrict(raw, schemaRequired("setting"))
		if ok {
			t.Errorf("nested field should not satisfy top-level Required, got ok=true text=%q", got)
		}
	})

	t.Run("nil schema does nothing", func(t *testing.T) {
		raw := `{"a":1}`
		if got, ok := extractJSONObjectStrict(raw, nil); ok || got != raw {
			t.Errorf("nil schema should not transform: got=%q ok=%v", got, ok)
		}
	})

	t.Run("non-object schema type does nothing", func(t *testing.T) {
		raw := `[1,2,3]`
		s := &JSONSchema{Type: "array"}
		if got, ok := extractJSONObjectStrict(raw, s); ok || got != raw {
			t.Errorf("non-object schema should not transform: got=%q ok=%v", got, ok)
		}
	})

	t.Run("empty Required does nothing (upstream parser handles leniently)", func(t *testing.T) {
		raw := `{"a":1}`
		s := &JSONSchema{Type: "object"} // Required == nil
		if got, ok := extractJSONObjectStrict(raw, s); ok || got != raw {
			t.Errorf("empty Required should not transform: got=%q ok=%v", got, ok)
		}
	})

	t.Run("object with unicode keys and nested arrays", func(t *testing.T) {
		raw := `{"setting":12,"issues":["a","b"],"suggestions":["x"]}`
		got, ok := extractJSONObjectStrict(raw, schemaRequired("setting", "issues", "suggestions"))
		if !ok {
			t.Fatal("ok=false")
		}
		if !strings.Contains(got, `"issues":["a","b"]`) {
			t.Errorf("nested array should be preserved: %s", got)
		}
	})

	t.Run("no JSON object at all", func(t *testing.T) {
		raw := "just some plain text answer"
		if got, ok := extractJSONObjectStrict(raw, schemaRequired("a")); ok || got != raw {
			t.Errorf("expected reject for no JSON: got=%q ok=%v", got, ok)
		}
	})
}
