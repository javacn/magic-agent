package agent

// jsonextract.go - llm 引擎的结构化输出后处理。
//
// 背景：上游（magic-video）注入 JSON Schema 进 system prompt，要求模型按
// schema 一次性输出 JSON 对象。llm CLI 走 OpenAI-compatible chat/completions
// 后返回的 `<response>` 是字符串；模型常把 JSON 嵌套进 `<think>...</think>`
// 之外的纯文本里，有时还裹了 markdown 围栏（```json ... ```）。直接拿原始
// 文本会让上游 `json.Unmarshal` 失败。
//
// `extractJSONObjectStrict` 用 schema 提供的「顶层必填字段」作为硬判定：
//   1. 必须能从文本里找到一个 JSON 对象（容忍 ```json 围栏与前后废话）；
//   2. 解出的对象必须是 map[string]any，且所有 schema.Required 都出现在
//      顶层键里（缺一个就算失败，不兜底）；
//   3. type=object 才执行；其他 schema 类型当前项目不出现。
//
// 失败时不改写原文本，让上游解析器（base/score 等已有 json 围栏剥离逻辑）
// 走兜底。
//
// 这条路径只对 llm 引擎启用。其他引擎（codebuddy/trae/claude）通过
// `--json-schema` flag / 各引擎自带的结构化输出支持处理；它们不进来。

import (
	"encoding/json"
	"strings"
)

// extractJSONObjectStrict 尝试从 text 里抽出一个严格匹配 schema 顶层
// Required 字段集的 JSON 对象，并返回紧凑 re-marshal 后的字符串。
//
// 失败时返回 (text, false) —— 调用方应原样用 text，不阻断。
func extractJSONObjectStrict(text string, schema *JSONSchema) (string, bool) {
	if schema == nil || !strings.EqualFold(schema.Type, "object") {
		return text, false
	}
	if len(schema.Required) == 0 {
		// 没有必填字段 → 不做"严格"判定，避免误判任何对象。
		// 上游有自带的宽松解析（容忍围栏），走兜底即可。
		return text, false
	}

	raw := strings.TrimSpace(text)
	raw = stripJSONFence(raw)

	obj, ok := firstJSONObject(raw)
	if !ok {
		return text, false
	}

	// 严格匹配：schema 顶层 Required 的每个字段都必须出现在 obj 的顶层键里。
	// 注意：只用 m[k] 直接判定；若 model 把字段放在嵌套对象里（如
	// {"scores": {...}}）也不通过 —— 上游要的就是扁平字段。
	missing := make([]string, 0, len(schema.Required))
	for _, k := range schema.Required {
		if _, ok := obj[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return text, false
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return text, false
	}
	return string(out), true
}

// stripJSONFence 去掉 markdown 的 ```json ... ``` 围栏（首尾）。
func stripJSONFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if idx := strings.Index(s, "\n"); idx > 0 {
		s = s[idx+1:]
	}
	if end := strings.LastIndex(s, "```"); end > 0 {
		s = s[:end]
	}
	return strings.TrimSpace(s)
}

// firstJSONObject 在 s 中找首个「完整」的顶层 JSON 对象，并解为 map。
// 括号计数法（支持嵌套）。找到返回 (map, true)；未找到 (nil, false)。
func firstJSONObject(s string) (map[string]any, bool) {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return nil, false
	}
	depth, inStr, escape := 0, false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch c {
			case '\\':
				escape = !escape
				continue
			case '"':
				if !escape {
					inStr = false
				}
				escape = false
				continue
			default:
				escape = false
				continue
			}
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				candidate := s[start : i+1]
				var obj map[string]any
				if err := json.Unmarshal([]byte(candidate), &obj); err != nil {
					return nil, false
				}
				return obj, true
			}
		}
	}
	return nil, false
}
