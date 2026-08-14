package pipeline

import (
	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// quotedMessageParserSettings mirrors
// astrbot/core/utils/quoted_message/settings.py. Forward-message fetching
// (max_forward_fetch / warn_on_action_failure) is not applicable in Go
// (no get_forward_msg implementation); the depth/image limits apply.
type quotedMessageParserSettings struct {
	maxComponentChainDepth  int
	maxForwardNodeDepth     int
	maxQuotedFallbackImages int
}

// defaultQuotedMessageParserSettings mirrors the Python defaults.
func defaultQuotedMessageParserSettings() quotedMessageParserSettings {
	return quotedMessageParserSettings{
		maxComponentChainDepth:  4,
		maxForwardNodeDepth:     6,
		maxQuotedFallbackImages: 20,
	}
}

// resolveQuotedMessageParserSettings reads provider_settings.quoted_message_parser.
func resolveQuotedMessageParserSettings(cfg map[string]interface{}) quotedMessageParserSettings {
	s := defaultQuotedMessageParserSettings()
	ps, ok := cfg["provider_settings"].(map[string]interface{})
	if !ok {
		return s
	}
	raw, ok := ps["quoted_message_parser"].(map[string]interface{})
	if !ok {
		return s
	}
	if v, ok := raw["max_component_chain_depth"].(int); ok && v > 0 {
		s.maxComponentChainDepth = v
	}
	if v, ok := raw["max_component_chain_depth"].(float64); ok && v > 0 {
		s.maxComponentChainDepth = int(v)
	}
	if v, ok := raw["max_forward_node_depth"].(int); ok && v > 0 {
		s.maxForwardNodeDepth = v
	}
	if v, ok := raw["max_forward_node_depth"].(float64); ok && v > 0 {
		s.maxForwardNodeDepth = int(v)
	}
	if v, ok := raw["max_quoted_fallback_images"].(int); ok && v > 0 {
		s.maxQuotedFallbackImages = v
	}
	if v, ok := raw["max_quoted_fallback_images"].(float64); ok && v > 0 {
		s.maxQuotedFallbackImages = int(v)
	}
	return s
}

// sanitizeQuotedChain applies the quoted-message parser limits to every Reply
// component in a chain: the nested quote depth is capped at
// max_component_chain_depth and Node nesting at max_forward_node_depth, and
// images inside quoted chains are capped at max_quoted_fallback_images.
// Mirrors the depth guards in quoted_message/chain_parser.py.
func sanitizeQuotedChain(chain []message.Component, settings quotedMessageParserSettings) []message.Component {
	for i, comp := range chain {
		switch c := comp.(type) {
		case *message.Reply:
			chain[i] = capReplyDepth(c, settings, 0)
		case *message.Node:
			chain[i] = capNodeDepth(c, settings, 0)
		case *message.Nodes:
			nodes := make([]*message.Node, 0, len(c.Nodes))
			for _, n := range c.Nodes {
				nodes = append(nodes, capNodeDepth(n, settings, 0))
			}
			chain[i] = &message.Nodes{Nodes: nodes}
		}
	}
	// Cap images inside quoted/forward content.
	imageCount := 0
	out := make([]message.Component, 0, len(chain))
	for _, comp := range chain {
		if _, ok := comp.(*message.Image); ok {
			imageCount++
			if imageCount > settings.maxQuotedFallbackImages {
				continue
			}
		}
		out = append(out, comp)
	}
	return out
}

// capReplyDepth limits the nested Reply chain depth (max_component_chain_depth).
func capReplyDepth(reply *message.Reply, settings quotedMessageParserSettings, depth int) *message.Reply {
	if depth >= settings.maxComponentChainDepth {
		return &message.Reply{
			MessageID:  reply.MessageID,
			SenderID:   reply.SenderID,
			SenderNick: reply.SenderNick,
			MessageStr: reply.MessageStr,
		}
	}
	out := &message.Reply{
		MessageID:  reply.MessageID,
		SenderID:   reply.SenderID,
		SenderNick: reply.SenderNick,
		MessageStr: reply.MessageStr,
		CreatedAt:  reply.CreatedAt,
	}
	clipped := make([]message.Component, 0, len(reply.Chain))
	for _, comp := range reply.Chain {
		switch c := comp.(type) {
		case *message.Reply:
			clipped = append(clipped, capReplyDepth(c, settings, depth+1))
		case *message.Node:
			clipped = append(clipped, capNodeDepth(c, settings, depth+1))
		case *message.Nodes:
			nodes := make([]*message.Node, 0, len(c.Nodes))
			for _, n := range c.Nodes {
				nodes = append(nodes, capNodeDepth(n, settings, depth+1))
			}
			clipped = append(clipped, &message.Nodes{Nodes: nodes})
		default:
			clipped = append(clipped, comp)
		}
	}
	out.Chain = clipped
	return out
}

// capNodeDepth limits nested forward-node depth (max_forward_node_depth).
func capNodeDepth(node *message.Node, settings quotedMessageParserSettings, depth int) *message.Node {
	if depth >= settings.maxForwardNodeDepth {
		return &message.Node{UIN: node.UIN, Name: node.Name}
	}
	out := &message.Node{UIN: node.UIN, Name: node.Name}
	clipped := make([]message.Component, 0, len(node.Content))
	for _, comp := range node.Content {
		switch c := comp.(type) {
		case *message.Reply:
			clipped = append(clipped, capReplyDepth(c, settings, depth+1))
		case *message.Node:
			clipped = append(clipped, capNodeDepth(c, settings, depth+1))
		case *message.Nodes:
			nodes := make([]*message.Node, 0, len(c.Nodes))
			for _, n := range c.Nodes {
				nodes = append(nodes, capNodeDepth(n, settings, depth+1))
			}
			clipped = append(clipped, &message.Nodes{Nodes: nodes})
		default:
			clipped = append(clipped, comp)
		}
	}
	out.Content = clipped
	return out
}

// applyQuotedMessageParser sanitizes the incoming message chain according to
// the quoted_message_parser settings (used by PreProcessStage).
func applyQuotedMessageParser(cfg map[string]interface{}, event *core.Event) {
	if event.Message == nil || len(event.Message.Chain) == 0 {
		return
	}
	settings := resolveQuotedMessageParserSettings(cfg)
	hasNested := false
	for _, comp := range event.Message.Chain {
		switch comp.(type) {
		case *message.Reply, *message.Node, *message.Nodes:
			hasNested = true
		}
	}
	if !hasNested {
		return
	}
	event.Message.Chain = sanitizeQuotedChain(event.Message.Chain, settings)
}
