package aiocqhttp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// sendActionWS 建立一个反向 WS 服务端：收到 sendAction 帧后按 wantStatus
// 回一个带相同 echo 的响应帧。
func sendActionWS(t *testing.T, wantStatus string) (*Adapter, *httptest.Server) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
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
			resp := map[string]interface{}{"status": wantStatus, "retcode": 0, "echo": echo}
			if wantStatus != "ok" {
				resp["msg"] = "send failed"
				resp["retcode"] = 100
			}
			_ = conn.WriteJSON(resp)
		}
	}))

	// 连接到测试服务端的 /ws 地址。
	wsURL := "ws" + srv.URL[len("http"):] + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial ws: %v", err)
	}
	a := New(map[string]interface{}{"id": "test"}, nil, nil)
	if err := a.addConn(conn); err != nil {
		srv.Close()
		t.Fatalf("addConn: %v", err)
	}
	// 镜像生产读循环：把带 echo 的响应帧投递给 pending 等待者。
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var msg map[string]interface{}
			if json.Unmarshal(data, &msg) != nil {
				continue
			}
			if echo, ok := msg["echo"].(string); ok {
				a.pendingMu.Lock()
				if ch, ok := a.pending[echo]; ok {
					delete(a.pending, echo)
					select {
					case ch <- msg:
					default:
					}
					close(ch)
				}
				a.pendingMu.Unlock()
			}
		}
	}()
	t.Cleanup(func() {
		conn.Close()
		srv.Close()
	})
	return a, srv
}

// TestSendActionConsumesResponse verifies sendAction registers the echo and the
// observer consumes the API response instead of dropping it.
func TestSendActionConsumesResponse(t *testing.T) {
	a, _ := sendActionWS(t, "ok")
	if err := a.sendAction("send_msg", map[string]interface{}{"message": "hi"}); err != nil {
		t.Fatalf("sendAction 应返回 nil，实际 %v", err)
	}
	// 等异步 observer 消费完响应并清理 pending。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.pendingMu.Lock()
		left := len(a.pending)
		a.pendingMu.Unlock()
		if left == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.pendingMu.Lock()
	defer a.pendingMu.Unlock()
	t.Errorf("响应到达后 pending 应被消费清理，剩余 %d 项", len(a.pending))
}

// TestSendActionPendingCleanedOnTimeout verifies a missing response cleans up
// the pending registration after actionTimeout.
func TestSendActionPendingCleanedOnTimeout(t *testing.T) {
	old := actionTimeout
	actionTimeout = 80 * time.Millisecond
	defer func() { actionTimeout = old }()

	a := New(map[string]interface{}{"id": "test"}, nil, nil)
	// 无连接 → sendAction 应失败且不遗留 pending。
	if err := a.sendAction("send_msg", map[string]interface{}{"message": "hi"}); err == nil {
		t.Fatal("无连接时 sendAction 应返回错误")
	}
	a.pendingMu.Lock()
	left := len(a.pending)
	a.pendingMu.Unlock()
	if left != 0 {
		t.Errorf("无连接失败路径不应遗留 pending，实际 %d", left)
	}
}
