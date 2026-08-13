package pipeline

import (
	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
)

// kbSearchToolSchema is the OpenAI schema for astr_kb_search (agentic KB mode).
func kbSearchToolSchema() map[string]interface{} {
	return map[string]interface{}{
		"type": "function",
		"function": map[string]interface{}{
			"name": "astr_kb_search",
			"description": "Search the knowledge bases configured for this session. " +
				"Returns relevant document chunks to answer the query from the knowledge base.",
			"parameters": map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"query": map[string]interface{}{
						"type":        "string",
						"description": "Required. The query to search in the knowledge bases.",
					},
				},
				"required": []interface{}{"query"},
			},
		},
	}
}

// executeKBSearch implements astr_kb_search using the pipeline's KB retriever
// (lifecycle-injected, honors session kb_config / global kb_names).
func (s *ProcessStage) executeKBSearch(event *core.Event, args map[string]interface{}) string {
	if s.kbRetriever == nil {
		return "Error: 知识库不可用"
	}
	query := argString(args, "query")
	if query == "" {
		return "Error: astr_kb_search requires a query."
	}
	contextText, err := s.kbRetriever(event.UnifiedMsgOrigin(), query)
	if err != nil {
		return "Error: 知识库检索失败: " + err.Error()
	}
	if contextText == "" {
		return "知识库中未找到相关内容。"
	}
	return contextText
}
