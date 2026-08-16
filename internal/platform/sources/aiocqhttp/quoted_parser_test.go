package aiocqhttp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

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

// TestParseReplySegment: a OneBot reply segment becomes a Reply component
// whose id is used later by enrichForwardAndQuoted.
func TestParseReplySegment(t *testing.T) {
	a := testAdapter()
	segments := []interface{}{
		map[string]interface{}{"type": "text", "data": map[string]interface{}{"text": "hi"}},
		map[string]interface{}{"type": "reply", "data": map[string]interface{}{"id": "r123"}},
	}
	chain, fids := a.parseOneBotSegments(segments, 0)
	if len(chain) != 2 {
		t.Fatalf("expected 2 components, got %d", len(chain))
	}
	if len(fids) != 0 {
		t.Errorf("reply must not produce forward ids, got %v", fids)
	}
	reply, ok := chain[1].(*message.Reply)
	if !ok {
		t.Fatalf("component[1] must be Reply, got %T", chain[1])
	}
	if reply.MessageID != "r123" {
		t.Errorf("Reply.MessageID mismatch: %q", reply.MessageID)
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

// startTestAdapter starts a real adapter HTTP server and connects a reverse-WS
// peer that answers OneBot actions via the given responder. The adapter's own
// read loop dispatches echo responses, so CallAction round-trips like in
// production.
func startTestAdapter(t *testing.T, respond func(action string, params map[string]interface{}, echo string) map[string]interface{}) *Adapter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	a := New(map[string]interface{}{"id": "default"}, map[string]interface{}{}, nil)
	a.quotedParser = defaultQuotedParserSettings()
	a.Host = "127.0.0.1"
	a.Port = port
	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = a.Stop() })

	wsURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", port)
	var conn *websocket.Conn
	for i := 0; i < 50; i++ {
		conn, _, err = websocket.DefaultDialer.Dial(wsURL, nil)
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if conn == nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	// Dial 返回成功 ≠ 服务端已完成 addConn：HTTP 101 升级响应发出后，服务端
	// handleWebSocket 还要在锁内把连接注册进 a.conns 才可用。立即发起 CallAction
	// 会命中注册前的窗口，遍历 conns 为空 → "no active WebSocket connection"
	// 快速失败（TestResolveForwardPlaceholdersMultiple 偶发 flaky 根因）。
	// 轮询等待连接真正注册完成后再返回。
	registered := false
	for i := 0; i < 100; i++ {
		a.mu.Lock()
		n := len(a.conns)
		a.mu.Unlock()
		if n > 0 {
			registered = true
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !registered {
		t.Fatalf("连接注册进适配器超时（服务端未完成 addConn）")
	}

	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req map[string]interface{}
			if json.Unmarshal(data, &req) != nil {
				continue
			}
			echo, _ := req["echo"].(string)
			action, _ := req["action"].(string)
			params, _ := req["params"].(map[string]interface{})
			resp := respond(action, params, echo)
			if resp == nil {
				continue
			}
			resp["echo"] = echo
			b, _ := json.Marshal(resp)
			_ = conn.WriteMessage(websocket.TextMessage, b)
		}
	}()
	return a
}

// TestParseActionResult: non-ok statuses must surface an error even when the
// response carries no msg (L-24).
func TestParseActionResult(t *testing.T) {
	data, err := parseActionResult(map[string]interface{}{
		"status": "ok",
		"data":   map[string]interface{}{"x": 1},
	})
	if err != nil {
		t.Fatalf("ok status must not error, got %v", err)
	}
	if data["x"] != 1 {
		t.Errorf("data mismatch: %#v", data)
	}

	_, err = parseActionResult(map[string]interface{}{
		"status": "failed",
		"msg":    "bad token",
	})
	if err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Errorf("expected msg error, got %v", err)
	}

	_, err = parseActionResult(map[string]interface{}{"status": "failed"})
	if err == nil || !strings.Contains(err.Error(), "status=failed") {
		t.Errorf("expected status error, got %v", err)
	}
}

// TestConvertFromCQFormatCQString: a CQ string message must not panic and
// yields an empty chain (with a warning log).
func TestConvertFromCQFormatCQString(t *testing.T) {
	a := testAdapter()
	raw := map[string]interface{}{"message": "[CQ:image,file=x] hello"}
	chain := a.convertFromCQFormat(raw)
	if chain == nil || len(chain.Chain) != 0 {
		t.Errorf("CQ string message must produce an empty chain, got %#v", chain)
	}
}

// TestFileSegmentFileID: the file segment carries the NapCat file id used by
// get_group_file_url / get_private_file_url.
func TestFileSegmentFileID(t *testing.T) {
	a := testAdapter()
	segments := []interface{}{
		map[string]interface{}{"type": "file", "data": map[string]interface{}{"file": "file-abc", "name": "doc.pdf"}},
	}
	chain, _ := a.parseOneBotSegments(segments, 0)
	if len(chain) != 1 {
		t.Fatalf("expected 1 component, got %d", len(chain))
	}
	f, ok := chain[0].(*message.File)
	if !ok {
		t.Fatalf("component = %T, want File", chain[0])
	}
	if f.FileID != "file-abc" || f.Name != "doc.pdf" {
		t.Errorf("file mismatch: %#v", f)
	}
}

// TestConvertFromCQFormatFileURL: a file segment without a URL is completed
// via get_group_file_url using the event's group context (L-24).
func TestConvertFromCQFormatFileURL(t *testing.T) {
	a := startTestAdapter(t, func(action string, params map[string]interface{}, echo string) map[string]interface{} {
		if action != "get_group_file_url" {
			return nil
		}
		return map[string]interface{}{
			"status": "ok",
			"data":   map[string]interface{}{"url": "https://example.com/file"},
		}
	})

	raw := map[string]interface{}{
		"message_type": "group",
		"group_id":     "12345",
		"message": []interface{}{
			map[string]interface{}{"type": "file", "data": map[string]interface{}{"file": "file-abc", "name": "doc.pdf"}},
		},
	}
	chain := a.convertFromCQFormat(raw)
	if len(chain.Chain) != 1 {
		t.Fatalf("expected 1 component, got %d", len(chain.Chain))
	}
	f, ok := chain.Chain[0].(*message.File)
	if !ok {
		t.Fatalf("component = %T, want File", chain.Chain[0])
	}
	if f.URL != "https://example.com/file" {
		t.Errorf("file URL not completed: %#v", f)
	}
}

// TestCollectNodeForwardIDs: nested ForwardIDs inside node content are queued
// for fetching (L-24).
func TestCollectNodeForwardIDs(t *testing.T) {
	pending := []string{}
	seen := map[string]bool{}
	node := &message.Node{Content: []message.Component{
		&message.Nodes{ForwardIDs: []string{"nested1", "  nested2  "}},
		&message.Reply{Chain: []message.Component{
			&message.Nodes{ForwardIDs: []string{"nested3"}},
		}},
		&message.Nodes{Nodes: []*message.Node{
			{Content: []message.Component{&message.Nodes{ForwardIDs: []string{"deep"}}}},
		}},
	}}
	collectNodeForwardIDs(node, &pending, seen)
	want := map[string]bool{"nested1": true, "nested2": true, "nested3": true, "deep": true}
	if len(pending) != len(want) {
		t.Fatalf("expected %d queued ids, got %v", len(want), pending)
	}
	for _, id := range pending {
		if !want[id] {
			t.Errorf("unexpected queued id %q (pending=%v)", id, pending)
		}
	}
}

// TestResolveForwardPlaceholdersMultiple: every forward placeholder — including
// ones inside quoted reply content — is replaced with its resolved nodes, not
// just the first one (L-24).
func TestResolveForwardPlaceholdersMultiple(t *testing.T) {
	a := startTestAdapter(t, func(action string, params map[string]interface{}, echo string) map[string]interface{} {
		if action == "get_msg" {
			return map[string]interface{}{
				"status": "ok",
				"data": map[string]interface{}{
					"message": []interface{}{
						map[string]interface{}{"type": "forward", "data": map[string]interface{}{"id": "fwd3"}},
					},
				},
			}
		}
		if action != "get_forward_msg" {
			return nil
		}
		return map[string]interface{}{
			"status": "ok",
			"data": map[string]interface{}{
				"messages": []interface{}{
					map[string]interface{}{
						"type": "node",
						"data": map[string]interface{}{
							"uin": "u1", "name": "user1",
							"content": []interface{}{map[string]interface{}{"type": "text", "data": map[string]interface{}{"text": "hi"}}},
						},
					},
				},
			},
		}
	})

	chain := &message.MessageChain{Chain: []message.Component{
		&message.Nodes{ForwardIDs: []string{"fwd1"}},
		&message.Plain{Text: "text"},
		&message.Nodes{ForwardIDs: []string{"fwd2"}},
		&message.Reply{MessageID: "r1"},
	}}
	a.enrichForwardAndQuoted(chain)

	if len(chain.Chain) != 4 {
		t.Fatalf("chain length = %d", len(chain.Chain))
	}
	for _, i := range []int{0, 2} {
		n, ok := chain.Chain[i].(*message.Nodes)
		if !ok {
			t.Fatalf("component[%d] = %T, want Nodes", i, chain.Chain[i])
		}
		if len(n.Nodes) != 1 || n.Nodes[0].UIN != "u1" {
			t.Errorf("component[%d] not resolved: %#v", i, n)
		}
	}
	reply, ok := chain.Chain[3].(*message.Reply)
	if !ok {
		t.Fatalf("component[3] = %T, want Reply", chain.Chain[3])
	}
	if len(reply.Chain) != 1 {
		t.Fatalf("reply chain length = %d", len(reply.Chain))
	}
	rn, ok := reply.Chain[0].(*message.Nodes)
	if !ok || len(rn.Nodes) != 1 || rn.Nodes[0].UIN != "u1" {
		t.Errorf("reply placeholder not resolved: %#v", reply.Chain[0])
	}
}

// TestStopClosesConns: Stop closes registered reverse-WS connections so the
// read-loop goroutines exit (L-24).
func TestStopClosesConns(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	a := testAdapter()
	if err := a.addConn(conn); err != nil {
		t.Fatalf("addConn: %v", err)
	}

	if err := a.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Error("connection must be closed by Stop")
	}
}

// TestConvertToCQFormatPlainText: array-format text segments carry the raw
// text without CQ escaping. OneBot implementations (NapCat/Lagrange/go-cqhttp)
// do not unescape array-form text, so escaping would render &#91;/&#44; literally.
func TestConvertToCQFormatPlainText(t *testing.T) {
	a := testAdapter()
	raw := "列表: [项目1, 项目2] [CQ:image,file=x] & <tag>"
	chain := &message.MessageChain{Chain: []message.Component{&message.Plain{Text: raw}}}
	segments := a.convertToCQFormat(chain)
	if len(segments) != 1 {
		t.Fatalf("expected 1 segment, got %d", len(segments))
	}
	seg := segments[0]
	if seg["type"] != "text" {
		t.Fatalf("segment type = %v", seg["type"])
	}
	data, ok := seg["data"].(map[string]interface{})
	if !ok {
		t.Fatalf("segment data = %T", seg["data"])
	}
	text, ok := data["text"].(string)
	if !ok {
		t.Fatalf("segment text = %T", data["text"])
	}
	if text != raw {
		t.Errorf("text must be passed through unescaped, got %q", text)
	}
	if strings.Contains(text, "&#91;") || strings.Contains(text, "&#44;") || strings.Contains(text, "&#93;") {
		t.Error("text must not contain CQ entities")
	}
}
