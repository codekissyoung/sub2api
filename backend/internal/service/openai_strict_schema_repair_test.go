package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestRepairOpenAIStrictJSONSchemaInBody_ResponsesTextFormat(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4-mini",
		"input":[{"type":"message","role":"user","content":"hi"}],
		"text":{"format":{"type":"json_schema","name":"ozon_sku_intro_v191","strict":true,"schema":{
			"type":"object",
			"properties":{
				"title":{"type":"string"},
				"based_on":{"type":"object","properties":{"sku":{"type":"string"}},"required":["sku"]}
			}
		}}
	}`)

	repaired, count := RepairOpenAIStrictJSONSchemaInBody(body)
	require.Equal(t, 3, count, "root required + root additionalProperties + nested additionalProperties")

	schema := gjson.GetBytes(repaired, "text.format.schema")
	require.Equal(t, []string{"based_on", "title"}, stringsFromJSON(schema.Get("required")))
	require.False(t, schema.Get("additionalProperties").Bool())
	require.Equal(t, []string{"sku"}, stringsFromJSON(schema.Get("properties.based_on.required")))
	require.False(t, schema.Get("properties.based_on.additionalProperties").Bool())
	// 未触碰的部分保持原样
	require.Equal(t, "ozon_sku_intro_v191", gjson.GetBytes(repaired, "text.format.name").String())
	require.True(t, gjson.GetBytes(repaired, "text.format.strict").Bool())
}

func TestRepairOpenAIStrictJSONSchemaInBody_ChatResponseFormat(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4-mini",
		"messages":[{"role":"user","content":"hi"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"out","schema":{
			"type":"object","properties":{"a":{"type":"string"}},"required":["a"]
		}}}
	}`)

	repaired, count := RepairOpenAIStrictJSONSchemaInBody(body)
	require.Equal(t, 1, count, "only additionalProperties missing")
	require.False(t, gjson.GetBytes(repaired, "response_format.json_schema.schema.additionalProperties").Bool())
	require.Equal(t, []string{"a"}, stringsFromJSON(gjson.GetBytes(repaired, "response_format.json_schema.schema.required")))
}

func TestRepairOpenAIStrictJSONSchemaInBody_SkipsExplicitNonStrict(t *testing.T) {
	body := []byte(`{"model":"m","input":[],"text":{"format":{"type":"json_schema","strict":false,"schema":{"type":"object","properties":{"a":{"type":"string"}}}}}}`)
	repaired, count := RepairOpenAIStrictJSONSchemaInBody(body)
	require.Equal(t, 0, count)
	require.Equal(t, body, repaired)
}

func TestRepairOpenAIStrictJSONSchemaInBody_IgnoresNonSchemaFormats(t *testing.T) {
	body := []byte(`{"model":"m","input":[],"text":{"format":{"type":"json_object"}}}`)
	repaired, count := RepairOpenAIStrictJSONSchemaInBody(body)
	require.Equal(t, 0, count)
	require.Equal(t, body, repaired)
}

func TestRepairOpenAIStrictJSONSchemaInBody_RecursesContainers(t *testing.T) {
	body := []byte(`{"model":"m","input":[],"text":{"format":{"type":"json_schema","schema":{
		"type":"object",
		"properties":{"list":{"type":"array","items":{"type":"object","properties":{"x":{"type":"string"}},"additionalProperties":false}}},
		"additionalProperties":false,
		"$defs":{"ref":{"type":"object","properties":{"y":{"type":"string"}},"required":[],"additionalProperties":false}}
	}}}}`)

	repaired, count := RepairOpenAIStrictJSONSchemaInBody(body)
	require.Equal(t, 3, count, "root required + items required + $defs required")
	schema := gjson.GetBytes(repaired, "text.format.schema")
	require.Equal(t, []string{"list"}, stringsFromJSON(schema.Get("required")))
	require.Equal(t, []string{"x"}, stringsFromJSON(schema.Get("properties.list.items.required")))
	require.Equal(t, []string{"y"}, stringsFromJSON(schema.Get(`$defs.ref.required`)))
}

func TestRepairOpenAIStrictJSONSchemaInBody_MalformedSchemaLeftUntouched(t *testing.T) {
	body := []byte(`{"model":"m","input":[],"text":{"format":{"type":"json_schema","schema":"not-an-object"}}}`)
	repaired, count := RepairOpenAIStrictJSONSchemaInBody(body)
	require.Equal(t, 0, count)
	require.Equal(t, body, repaired)
}

func TestRepairOpenAIStrictJSONSchemaInBody_MalformedRequiredLeftUntouched(t *testing.T) {
	body := []byte(`{"model":"m","input":[],"text":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"a":{"type":"string"}},"required":"a"}}}}`)
	repaired, count := RepairOpenAIStrictJSONSchemaInBody(body)
	require.Equal(t, 1, count, "only additionalProperties is added; malformed required is left for the upstream error")
	require.Equal(t, "a", gjson.GetBytes(repaired, "text.format.schema.required").String())
}

func stringsFromJSON(result gjson.Result) []string {
	var out []string
	for _, item := range result.Array() {
		out = append(out, item.String())
	}
	return out
}
