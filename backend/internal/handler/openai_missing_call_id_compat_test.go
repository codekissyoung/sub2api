package handler

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestNormalizeCodexMissingCallIDsPairsMissingIDs(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","input":[{"type":"function_call","id":"fc_1","name":"lookup","arguments":"{}"},{"type":"message","role":"assistant","content":"working"},{"type":"function_call_output","id":"fco_1","output":"ok"}]}`)

	got, repair := normalizeCodexMissingCallIDs(body)
	require.Equal(t, 1, repair.callsAssigned)
	require.Equal(t, 1, repair.outputsAssigned)
	callID := gjson.GetBytes(got, "input.0.call_id").String()
	require.Equal(t, "call_missing_0_2", callID)
	require.Equal(t, callID, gjson.GetBytes(got, "input.2.call_id").String())
	require.Equal(t, "fc_1", gjson.GetBytes(got, "input.0.id").String())
	require.Equal(t, "fco_1", gjson.GetBytes(got, "input.2.id").String())
	validation := service.ValidateFunctionCallOutputContextBytes(got)
	require.True(t, validation.HasToolCallContext)
	require.False(t, validation.HasFunctionCallOutputMissingCallID)

	again, secondRepair := normalizeCodexMissingCallIDs(got)
	require.False(t, secondRepair.changed())
	require.Equal(t, got, again)
}

func TestNormalizeCodexMissingCallIDsDoesNotCrossPairExplicitIDTypes(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","input":[{"type":"custom_tool_call","call_id":"call_shared","name":"patch","input":"x"},{"type":"function_call","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_shared","output":"ambiguous"}]}`)

	got, repair := normalizeCodexMissingCallIDs(body)
	require.False(t, repair.changed())
	require.Equal(t, body, got)
	require.False(t, gjson.GetBytes(got, "input.1.call_id").Exists())
}

func TestNormalizeCodexMissingCallIDsUsesExistingCallID(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","input":[{"type":"function_call","call_id":"call_existing","name":"lookup","arguments":"{}"},{"type":"function_call_output","id":"fco_1","output":"ok"}]}`)

	got, repair := normalizeCodexMissingCallIDs(body)
	require.Zero(t, repair.callsAssigned)
	require.Equal(t, 1, repair.outputsAssigned)
	require.Equal(t, "call_existing", gjson.GetBytes(got, "input.1.call_id").String())
}

func TestNormalizeCodexMissingCallIDsUsesExplicitOutputID(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","input":[{"type":"function_call","id":"fc_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_from_output","output":"ok"}]}`)

	got, repair := normalizeCodexMissingCallIDs(body)
	require.Equal(t, 1, repair.callsAssigned)
	require.Zero(t, repair.outputsAssigned)
	require.Equal(t, "call_from_output", gjson.GetBytes(got, "input.0.call_id").String())
	require.Equal(t, "call_from_output", gjson.GetBytes(got, "input.1.call_id").String())
}

func TestNormalizeCodexMissingCallIDsPairsFIFOByCallType(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","input":[{"type":"function_call","name":"first","arguments":"{}"},{"type":"custom_tool_call","name":"patch","input":"x"},{"type":"function_call","call_id":"call_third","name":"third","arguments":"{}"},{"type":"function_call_output","output":"one"},{"type":"custom_tool_call_output","output":"two"},{"type":"function_call_output","output":"three"}]}`)

	got, repair := normalizeCodexMissingCallIDs(body)
	require.Equal(t, 2, repair.callsAssigned)
	require.Equal(t, 3, repair.outputsAssigned)
	require.Equal(t, gjson.GetBytes(got, "input.0.call_id").String(), gjson.GetBytes(got, "input.3.call_id").String())
	require.Equal(t, gjson.GetBytes(got, "input.1.call_id").String(), gjson.GetBytes(got, "input.4.call_id").String())
	require.Equal(t, "call_third", gjson.GetBytes(got, "input.5.call_id").String())
}

func TestNormalizeCodexMissingCallIDsDoesNotGuessOrphanOutput(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","input":[{"type":"function_call_output","id":"fco_74ec40c883248ebb4885ec84","namespace":"codex_app","name":"unknown","output":"result"}]}`)

	got, repair := normalizeCodexMissingCallIDs(body)
	require.False(t, repair.changed())
	require.Equal(t, body, got)
	require.False(t, gjson.GetBytes(got, "input.0.call_id").Exists())
}

func TestNormalizeCodexMissingCallIDsDoesNotPairFutureCall(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","input":[{"type":"function_call_output","output":"too early"},{"type":"function_call","name":"lookup","arguments":"{}"}]}`)

	got, repair := normalizeCodexMissingCallIDs(body)
	require.False(t, repair.changed())
	require.Equal(t, body, got)
}

func TestNormalizeCodexMissingCallIDsAvoidsGeneratedIDCollision(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","input":[{"type":"function_call","name":"lookup","arguments":"{}"},{"type":"function_call","call_id":"call_missing_0_2","name":"other","arguments":"{}"},{"type":"function_call_output","output":"ok"}]}`)

	got, repair := normalizeCodexMissingCallIDs(body)
	require.True(t, repair.changed())
	require.Equal(t, "call_missing_0_2_1", gjson.GetBytes(got, "input.0.call_id").String())
	require.Equal(t, "call_missing_0_2_1", gjson.GetBytes(got, "input.2.call_id").String())
}

func TestNormalizeCodexMissingCallIDsPreservesValidAndDuplicateJSON(t *testing.T) {
	valid := []byte(`{"model":"gpt-5.6","input":[{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`)
	got, repair := normalizeCodexMissingCallIDs(valid)
	require.False(t, repair.changed())
	require.Equal(t, valid, got)

	duplicate := []byte(`{"model":"gpt-5.6","input":[],"input":[{"type":"function_call","name":"lookup"},{"type":"function_call_output","output":"ok"}]}`)
	got, repair = normalizeCodexMissingCallIDs(duplicate)
	require.False(t, repair.changed())
	require.Equal(t, duplicate, got)
}
