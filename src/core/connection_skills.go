package core

import "xiaozhi-server-go/src/core/types"

// withSkillCatalog augments only this request, keeping stored role prompts and
// conversation history unchanged. Recomposition also handles role switches.
func withSkillCatalog(messages []types.Message, catalog string) []types.Message {
	if catalog == "" {
		return messages
	}
	result := append([]types.Message(nil), messages...)
	if len(result) > 0 && result[0].Role == "system" {
		result[0].Content += catalog
		return result
	}
	return append([]types.Message{{Role: "system", Content: catalog}}, result...)
}
