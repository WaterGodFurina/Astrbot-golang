package aiocqhttp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestMaxReverseWSConns verifies the /ws endpoint rejects connections beyond
// the configured limit (ws_reverse_max_conns) with 503 Service Unavailable,
// and accepts a new connection after an existing one closes (bug 6.4:
// removeConn must decrement the count correctly).
func TestMaxReverseWSConns(t *testing.T) {
	const limit = 3
	a := New(map[string]interface{}{"id": "test", "ws_reverse_max_conns": float64(limit)}, nil, nil)
	srv := httptest.NewServer(http.HandlerFunc(a.handleWebSocket))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	dial := func() (*websocket.Conn, int) {
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			if resp != nil {
				return nil, resp.StatusCode
			}
			return nil, 0
		}
		return conn, http.StatusSwitchingProtocols
	}

	// 前 N 个连接全部成功。
	var conns []*websocket.Conn
	for i := 0; i < limit; i++ {
		c, status := dial()
		if status != http.StatusSwitchingProtocols {
			if c != nil {
				c.Close()
			}
			t.Fatalf("第 %d 个连接应成功（101 Switching Protocols），实际状态码 %d", i+1, status)
		}
		conns = append(conns, c)
	}

	// 客户端收到 101 时服务端可能尚未执行该连接的 addConn（计数未及时
	// 上升）。若此时立即拨下一个连接，pre-check 可能看到计数不足而放行
	// 造成误判，故先轮询等服务端计数达到上限。
	deadline := time.Now().Add(2 * time.Second)
	for a.connCount() != limit && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if a.connCount() != limit {
		t.Fatalf("服务端应计数 %d 个连接，当前 %d", limit, a.connCount())
	}

	// 第 N+1 个连接被拒绝：503，且未建立 WS（Upgrade 之前即被拒绝）。
	if c, status := dial(); status != http.StatusServiceUnavailable {
		if c != nil {
			c.Close()
		}
		t.Fatalf("超出上限的连接应返回 503，实际状态码 %d", status)
	}

	// 关闭一个连接后，新连接被接受（服务端异步回收，先等待计数下降）。
	conns[0].Close()
	deadline := time.Now().Add(2 * time.Second)
	for a.connCount() != limit-1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if a.connCount() != limit-1 {
		t.Fatalf("关闭连接后服务端应回收（removeConn 递减），当前计数 %d", a.connCount())
	}
	c, status := dial()
	if status != http.StatusSwitchingProtocols {
		if c != nil {
			c.Close()
		}
		t.Fatalf("关闭一个连接后新连接应被接受，实际状态码 %d", status)
	}
	conns = append(conns[1:], c)

	for _, c := range conns {
		c.Close()
	}
}

// TestAddConnEnforcesLimit verifies addConn itself rejects over-limit inserts
// under lock, closing the check-then-insert race window.
func TestAddConnEnforcesLimit(t *testing.T) {
	a := New(map[string]interface{}{"id": "test", "ws_reverse_max_conns": float64(2)}, nil, nil)
	// 直接构造未连接的 *websocket.Conn：addConn 只登记不读写。
	for i := 0; i < 2; i++ {
		if err := a.addConn(&websocket.Conn{}); err != nil {
			t.Fatalf("第 %d 个 addConn 应成功，实际 %v", i+1, err)
		}
	}
	if err := a.addConn(&websocket.Conn{}); err == nil {
		t.Fatal("超过上限的 addConn 应返回错误")
	}
	if got := a.connCount(); got != 2 {
		t.Fatalf("addConn 拒绝后计数不应变化，当前 %d", got)
	}
}
