package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// codexMissingCallIDRepair records only aggregate compatibility changes. Tool
// identities are deliberately excluded so request logs never gain user data.
type codexMissingCallIDRepair struct {
	callsAssigned   int
	outputsAssigned int
}

func (r codexMissingCallIDRepair) changed() bool {
	return r.callsAssigned > 0 || r.outputsAssigned > 0
}

type pendingCodexCall struct {
	item               map[string]any
	expectedOutputType string
	callID             string
	inputIndex         int
	consumed           bool
}

// normalizeCodexMissingCallIDs adapts CLIProxyAPI's pending-call FIFO repair
// semantics to native Responses input. It only repairs a missing call_id when
// an earlier compatible call in the same request proves the association:
//
//   - an output without call_id inherits the first pending compatible call;
//   - a call without call_id inherits an explicit ID from its paired output;
//   - when both sides are missing, one deterministic collision-free ID is
//     assigned to both.
//
// Truly orphaned outputs remain unchanged and are rejected by the existing
// validation. In particular, an output item id (for example fco_*) is never
// treated as a call_id because those identifiers have different semantics.
func normalizeCodexMissingCallIDs(body []byte) ([]byte, codexMissingCallIDRepair) {
	repair := codexMissingCallIDRepair{}
	if len(body) == 0 || !hasCodexCallIDRepairCandidate(body) || !hasUniqueJSONMembers(body) {
		return body, repair
	}

	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var request map[string]any
	if err := decoder.Decode(&request); err != nil {
		return body, repair
	}
	input, ok := request["input"].([]any)
	if !ok || len(input) == 0 {
		return body, repair
	}

	usedCallIDs := make(map[string]struct{})
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if callID := responseItemCallID(item); callID != "" {
			usedCallIDs[callID] = struct{}{}
		}
	}

	pending := make([]*pendingCodexCall, 0)
	for inputIndex, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		itemType := stringField(item, "type")
		if outputType := codexOutputTypeForCall(itemType); outputType != "" {
			pending = append(pending, &pendingCodexCall{
				item:               item,
				expectedOutputType: outputType,
				callID:             responseItemCallID(item),
				inputIndex:         inputIndex,
			})
			continue
		}
		if !isCodexRepairableOutputType(itemType) {
			continue
		}

		outputCallID := responseItemCallID(item)
		if outputCallID != "" {
			if matching := findPendingCodexCall(pending, itemType, outputCallID, false); matching != nil {
				matching.consumed = true
				continue
			}
			// If that explicit ID belongs to any preceding call, a second output
			// or a mismatched output type is ambiguous and must not be attached to
			// a different ID-less call.
			if hasPendingCodexCallID(pending, outputCallID) {
				continue
			}
			matching := findFirstIDLessPendingCodexCall(pending, itemType)
			if matching == nil {
				continue
			}
			matching.item["call_id"] = outputCallID
			matching.callID = outputCallID
			matching.consumed = true
			repair.callsAssigned++
			continue
		}

		matching := findFirstPendingCodexCall(pending, itemType)
		if matching == nil {
			// Same behavior boundary as the existing Sub2API validation: no
			// prior call means there is no trustworthy identity to recover.
			continue
		}
		if matching.callID == "" {
			matching.callID = nextMissingCodexCallID(usedCallIDs, matching.inputIndex, inputIndex)
			matching.item["call_id"] = matching.callID
			repair.callsAssigned++
		}
		item["call_id"] = matching.callID
		matching.consumed = true
		repair.outputsAssigned++
	}

	if !repair.changed() {
		return body, repair
	}
	normalized, err := json.Marshal(request)
	if err != nil {
		return body, codexMissingCallIDRepair{}
	}
	return normalized, repair
}

// hasCodexCallIDRepairCandidate keeps the compatibility path off normal tool
// continuation traffic. Full JSON decoding is reserved for requests that have
// a tool output and at least one relevant call or output without an ID.
func hasCodexCallIDRepairCandidate(body []byte) bool {
	input := gjson.GetBytes(body, "input")
	if !input.IsArray() {
		return false
	}
	hasOutput := false
	missing := false
	input.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() {
			return true
		}
		itemType := item.Get("type").String()
		isCall := codexOutputTypeForCall(itemType) != ""
		isOutput := isCodexRepairableOutputType(itemType)
		if !isCall && !isOutput {
			return true
		}
		if isOutput {
			hasOutput = true
		}
		if strings.TrimSpace(item.Get("call_id").String()) == "" {
			missing = true
		}
		return true
	})
	return hasOutput && missing
}

func responseItemCallID(item map[string]any) string {
	value, _ := item["call_id"].(string)
	return strings.TrimSpace(value)
}

func codexOutputTypeForCall(itemType string) string {
	switch itemType {
	case "function_call":
		return "function_call_output"
	case "custom_tool_call":
		return "custom_tool_call_output"
	default:
		return ""
	}
}

func isCodexRepairableOutputType(itemType string) bool {
	return itemType == "function_call_output" || itemType == "custom_tool_call_output"
}

func findPendingCodexCall(pending []*pendingCodexCall, outputType, callID string, includeConsumed bool) *pendingCodexCall {
	for _, candidate := range pending {
		if candidate.expectedOutputType != outputType || candidate.callID != callID || (!includeConsumed && candidate.consumed) {
			continue
		}
		return candidate
	}
	return nil
}

func hasPendingCodexCallID(pending []*pendingCodexCall, callID string) bool {
	for _, candidate := range pending {
		if candidate.callID == callID {
			return true
		}
	}
	return false
}

func findFirstIDLessPendingCodexCall(pending []*pendingCodexCall, outputType string) *pendingCodexCall {
	for _, candidate := range pending {
		if !candidate.consumed && candidate.expectedOutputType == outputType && candidate.callID == "" {
			return candidate
		}
	}
	return nil
}

func findFirstPendingCodexCall(pending []*pendingCodexCall, outputType string) *pendingCodexCall {
	for _, candidate := range pending {
		if !candidate.consumed && candidate.expectedOutputType == outputType {
			return candidate
		}
	}
	return nil
}

func nextMissingCodexCallID(used map[string]struct{}, callIndex, outputIndex int) string {
	base := fmt.Sprintf("call_missing_%d_%d", callIndex, outputIndex)
	candidate := base
	for suffix := 1; ; suffix++ {
		if _, exists := used[candidate]; !exists {
			used[candidate] = struct{}{}
			return candidate
		}
		candidate = fmt.Sprintf("%s_%d", base, suffix)
	}
}
