package service

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestIsOpenAIEncryptedContentReplayError(t *testing.T) {
	body := []byte(`{"error":{"type":"invalid_request_error","message":"The encrypted content 0c9f...ff-0 could not be verified. Reason: Encrypted content could not be decrypted or parsed."}}`)
	assert.True(t, isOpenAIEncryptedContentReplayError(http.StatusBadRequest, body))

	// Only 400 carries the replay-failure semantics; a wrapped 5xx must not
	// trigger the strip-retry.
	assert.False(t, isOpenAIEncryptedContentReplayError(http.StatusInternalServerError, body))
	assert.False(t, isOpenAIEncryptedContentReplayError(http.StatusBadRequest, []byte(`{"error":{"message":"context_length_exceeded"}}`)))
	assert.False(t, isOpenAIEncryptedContentReplayError(http.StatusBadRequest, nil))
	assert.False(t, isOpenAIEncryptedContentReplayError(http.StatusBadRequest, []byte(`not json`)))
}

func TestDropOpenAIReasoningEncryptedContentItems(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-luna","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"ok"}],"encrypted_content":"gAAAAdeadbeef"},` +
		`{"type":"compaction","encrypted_content":"gAAAAc0mpact"},` +
		`{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"plain"}]},` +
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}` +
		`]}`)

	out, changed := dropOpenAIReasoningEncryptedContentItems(body)
	require.True(t, changed)

	input := gjson.GetBytes(out, "input").Array()
	require.Len(t, input, 4)
	assert.Equal(t, "message", input[0].Get("type").String())
	// Reasoning items WITHOUT encrypted_content stay.
	assert.Equal(t, "rs_2", input[1].Get("id").String())
	assert.Equal(t, "reasoning", input[1].Get("type").String())
	assert.Equal(t, "function_call", input[2].Get("type").String())
	assert.Equal(t, "done", input[3].Get("content.0.text").String())
	// The dropped items are gone entirely (reasoning + compaction with blobs).
	for _, item := range input {
		assert.NotEqual(t, "rs_1", item.Get("id").String())
		assert.NotEqual(t, "compaction", item.Get("type").String())
	}
	// Top-level fields survive the raw splice.
	assert.Equal(t, "gpt-5.6-luna", gjson.GetBytes(out, "model").String())
}

func TestDropOpenAIReasoningEncryptedContentItemsNoop(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[]}]}`)
	out, changed := dropOpenAIReasoningEncryptedContentItems(body)
	assert.False(t, changed)
	assert.Equal(t, body, out)

	// String input form is untouched.
	strBody := []byte(`{"model":"m","input":"hello"}`)
	out, changed = dropOpenAIReasoningEncryptedContentItems(strBody)
	assert.False(t, changed)
	assert.Equal(t, strBody, out)
}
