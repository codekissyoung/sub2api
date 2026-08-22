package service

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// openAIStrictJSONSchemaFormatPaths 列出携带 JSON schema 结构化输出的请求体
// 位置：/responses 的 text.format 与 chat completions 的 response_format。
var openAIStrictJSONSchemaFormatPaths = []struct {
	formatPath string // format 对象（含 type/strict）
	schemaPath string // 其中的 schema 子对象
	strictPath string // strict 标志位置
}{
	{"text.format", "text.format.schema", "text.format.strict"},
	{"response_format", "response_format.json_schema.schema", "response_format.json_schema.strict"},
}

// RepairOpenAIStrictJSONSchemaInBody 修补请求体中 OpenAI 严格模式结构化输出
// schema：带 properties 的 object 节点必须有列出全部属性键的 required 数组，
// 且需要 additionalProperties:false（仅在缺失时补，不覆盖显式设置）。
// 显式 strict:false 的格式块不动。任何解析异常都原样返回——修复绝不能打挂
// 请求。返回修复后的 body、修复的 schema 节点数。
func RepairOpenAIStrictJSONSchemaInBody(body []byte) ([]byte, int) {
	if len(body) == 0 {
		return body, 0
	}
	out := body
	totalRepaired := 0
	for _, path := range openAIStrictJSONSchemaFormatPaths {
		format := gjson.GetBytes(out, path.formatPath)
		if !format.Exists() || !format.IsObject() {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(format.Get("type").String()), "json_schema") {
			continue
		}
		if strict := format.Get("strict"); strict.Exists() && strict.Type == gjson.False {
			continue
		}
		schemaRaw := gjson.GetBytes(out, path.schemaPath)
		if !schemaRaw.Exists() || !schemaRaw.IsObject() {
			continue
		}
		var schema any
		if err := json.Unmarshal([]byte(schemaRaw.Raw), &schema); err != nil {
			continue
		}
		repaired := repairOpenAIStrictSchemaNode(schema)
		if repaired == 0 {
			continue
		}
		repairedRaw, err := json.Marshal(schema)
		if err != nil {
			continue
		}
		next, err := sjson.SetRawBytes(out, path.schemaPath, repairedRaw)
		if err != nil {
			continue
		}
		out = next
		totalRepaired += repaired
	}
	return out, totalRepaired
}

// repairOpenAIStrictSchemaNode 递归修补 schema 节点，返回修补的节点数。
func repairOpenAIStrictSchemaNode(node any) int {
	switch typed := node.(type) {
	case map[string]any:
		repaired := 0
		if props, ok := typed["properties"].(map[string]any); ok && len(props) > 0 {
			if repairOpenAIStrictObjectRequired(typed, props) {
				repaired++
			}
			if _, exists := typed["additionalProperties"]; !exists {
				typed["additionalProperties"] = false
				repaired++
			}
		}
		for _, key := range []string{"properties", "patternProperties", "$defs", "definitions"} {
			subs, exists := typed[key].(map[string]any)
			if !exists {
				continue
			}
			for _, sub := range subs {
				repaired += repairOpenAIStrictSchemaNode(sub)
			}
		}
		for _, key := range []string{
			"items", "prefixItems", "additionalProperties", "contains",
			"not", "if", "then", "else",
			"anyOf", "allOf", "oneOf",
		} {
			child, exists := typed[key]
			if !exists {
				continue
			}
			repaired += repairOpenAIStrictSchemaNode(child)
		}
		return repaired
	case []any:
		repaired := 0
		for _, item := range typed {
			repaired += repairOpenAIStrictSchemaNode(item)
		}
		return repaired
	default:
		return 0
	}
}

// repairOpenAIStrictObjectRequired 把 object 节点的 required 补齐为 properties
// 全部键（已有合法字符串数组成员保留，排序去重）。required 存在但不是字符串
// 数组时不做猜测性改写，交给上游报错。
func repairOpenAIStrictObjectRequired(node, props map[string]any) bool {
	keys := make([]string, 0, len(props))
	for key := range props {
		keys = append(keys, key)
	}
	merged := make(map[string]struct{}, len(keys)+4)
	for _, key := range keys {
		merged[key] = struct{}{}
	}
	if existing, exists := node["required"]; exists {
		existingList, ok := existing.([]any)
		if !ok {
			return false
		}
		for _, item := range existingList {
			str, ok := item.(string)
			if !ok {
				return false
			}
			merged[str] = struct{}{}
		}
		if len(merged) == len(existingList) {
			return false
		}
	}
	out := make([]string, 0, len(merged))
	for key := range merged {
		out = append(out, key)
	}
	sort.Strings(out)
	required := make([]any, len(out))
	for i, key := range out {
		required[i] = key
	}
	node["required"] = required
	return true
}
