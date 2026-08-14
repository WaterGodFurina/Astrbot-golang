package aiocqhttp

import (
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

func testAdapter() *Adapter {
	a := New(map[string]interface{}{"id": "default"}, map[string]interface{}{}, nil)
	a.quotedParser = defaultQuotedParserSettings()
	return a
}

// TestParseForwardInline: inline forward nodes become Nodes components.
func TestParseForwardInline(t *testing.T) {
	a := testAdapter()
	segments := []interface{}{
		map[string]interface{}{"type": "text", "data": map[string]interface{}{"text": "hi"}},
		map[string]interface{}{
			"type": "forward",
			"data": map[string]interface{}{
				"content": []interface{}{
					map[string]interface{}{
						"type": "node",
						"data": map[string]interface{}{
							"uin": "u1", "name": "user1",
							"content": []interface{}{
								map[string]interface{}{"type": "text", "data": map[string]interface{}{"text": "转发内容"}},
							},
						},
					},
				},
			},
		},
	}
	chain, fids := a.parseOneBotSegments(segments, 0)
	if len(chain) != 2 {
		t.Fatalf("expected 2 components, got %d", len(chain))
	}
	if len(fids) != 0 {
		t.Errorf("inline forward must not produce fetch ids, got %v", fids)
	}
	nodes, ok := chain[1].(*message.Nodes)
	if !ok {
		t.Fatalf("component[1] must be Nodes, got %T", chain[1])
	}
	if len(nodes.Nodes) != 1 || nodes.Nodes[0].Name != "user1" {
		t.Errorf("node parse mismatch: %+v", nodes.Nodes)
	}
}

// TestParseForwardRemoteId: remote forward ids are collected for fetching.
func TestParseForwardRemoteId(t *testing.T) {
	a := testAdapter()
	segments := []interface{}{
		map[string]interface{}{
			"type": "forward",
			"data": map[string]interface{}{"id": "fwd123"},
		},
	}
	chain, fids := a.parseOneBotSegments(segments, 0)
	if len(fids) != 1 || fids[0] != "fwd123" {
		t.Errorf("forward ids: %v", fids)
	}
	nodes, ok := chain[0].(*message.Nodes)
	if !ok || nodes.IDs()[0] != "fwd123" {
		t.Errorf("Nodes must carry the forward id: %#v", chain[0])
	}
}

// TestForwardDepthLimit: deeply nested inline forwards are capped.
func TestForwardDepthLimit(t *testing.T) {
	a := testAdapter()
	a.quotedParser.maxForwardNodeDepth = 2

	// Two-level nesting fits within depth 2.
	inner := []interface{}{
		map[string]interface{}{"type": "text", "data": map[string]interface{}{"text": "deep"}},
	}
	level2 := []interface{}{
		map[string]interface{}{
			"type": "node",
			"data": map[string]interface{}{"uin": "u", "name": "n", "content": inner},
		},
	}
	level1 := []interface{}{
		map[string]interface{}{
			"type": "node",
			"data": map[string]interface{}{"uin": "u", "name": "n", "content": level2},
		},
	}
	segments := []interface{}{
		map[string]interface{}{"type": "forward", "data": map[string]interface{}{"content": level1}},
	}
	chain, _ := a.parseOneBotSegments(segments, 0)
	if len(chain) != 1 {
		t.Fatalf("expected 1 component for depth-2 nesting, got %d", len(chain))
	}
	top, ok := chain[0].(*message.Nodes)
	if !ok || len(top.Nodes) != 1 {
		t.Fatalf("top must be Nodes with 1 node, got %#v", chain[0])
	}
	mid, ok := top.Nodes[0].Content[0].(*message.Nodes)
	if !ok || len(mid.Nodes) != 1 {
		t.Fatalf("level-2 node must be preserved, got %#v", top.Nodes[0].Content)
	}
	if len(mid.Nodes[0].Content) != 1 {
		t.Fatalf("level-3 content must survive: %#v", mid.Nodes[0].Content)
	}

	// Four-level nesting exceeds depth 2: over-deep subtrees are dropped.
	// A node that carries only a dropped subtree yields nothing.
	level4 := level2
	for i := 0; i < 2; i++ {
		level4 = []interface{}{
			map[string]interface{}{
				"type": "node",
				"data": map[string]interface{}{"uin": "u", "name": "n", "content": level4},
			},
		}
	}
	segments4 := []interface{}{
		map[string]interface{}{"type": "forward", "data": map[string]interface{}{"content": level4}},
	}
	chain4, _ := a.parseOneBotSegments(segments4, 0)
	if len(chain4) != 0 {
		t.Errorf("over-deep nested forwards must be dropped entirely, got %d components", len(chain4))
	}

	// A node with its own text keeps the text while over-deep children drop.
	selfTextLevel := []interface{}{
		map[string]interface{}{
			"type": "node",
			"data": map[string]interface{}{
				"uin": "u", "name": "n",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "data": map[string]interface{}{"text": "own text"}},
					map[string]interface{}{
						"type": "node",
						"data": map[string]interface{}{"uin": "u2", "name": "n2", "content": level4},
					},
				},
			},
		},
	}
	segments5 := []interface{}{
		map[string]interface{}{"type": "forward", "data": map[string]interface{}{"content": selfTextLevel}},
	}
	chain5, _ := a.parseOneBotSegments(segments5, 0)
	if len(chain5) != 1 {
		t.Fatalf("expected 1 component, got %d", len(chain5))
	}
	n5, _ := chain5[0].(*message.Nodes)
	if n5 == nil || len(n5.Nodes) != 1 {
		t.Fatalf("node with own text must be preserved: %#v", chain5[0])
	}
	if len(n5.Nodes[0].Content) != 1 {
		t.Errorf("over-deep child must be dropped, kept %d components", len(n5.Nodes[0].Content))
	}
	if plain, ok := n5.Nodes[0].Content[0].(*message.Plain); !ok || plain.Text != "own text" {
		t.Errorf("own text must be preserved: %#v", n5.Nodes[0].Content)
	}
}

// TestResolveQuotedParserSettings: settings are read from provider_settings.
func TestResolveQuotedParserSettings(t *testing.T) {
	settings := map[string]interface{}{
		"provider_settings": map[string]interface{}{
			"quoted_message_parser": map[string]interface{}{
				"max_forward_fetch":      5,
				"max_forward_node_depth": 3,
				"warn_on_action_failure": true,
			},
		},
	}
	s := resolveQuotedParserSettings(settings)
	if s.maxForwardFetch != 5 || s.maxForwardNodeDepth != 3 || !s.warnOnActionFailure {
		t.Errorf("settings mismatch: %+v", s)
	}
}
