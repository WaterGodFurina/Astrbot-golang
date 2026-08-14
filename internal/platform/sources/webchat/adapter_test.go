package webchat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDefaultPort(t *testing.T) {
	// 未配置端口时应使用独立默认端口 6195，避免与 dashboard 的 6185 冲突（M-56 回归）。
	a := New(map[string]interface{}{"id": "webchat-test"}, nil, nil)
	if a.Port != 6195 {
		t.Errorf("期望默认端口 6195，实际 %d", a.Port)
	}

	// 显式配置端口时应生效
	b := New(map[string]interface{}{"id": "webchat-test", "port": float64(8080)}, nil, nil)
	if b.Port != 8080 {
		t.Errorf("期望端口 8080，实际 %d", b.Port)
	}
}

func TestPollClientCleanup(t *testing.T) {
	// /poll 结束后 pollClients 通道应被清理，不随任意 session_id 无限增长（L-42 回归）。
	a := New(map[string]interface{}{"id": "webchat-test"}, nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/poll?session_id=leaky", nil).WithContext(ctx)
	req.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	go a.handlePoll(w, req)

	// 等待通道注册
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		_, ok := a.pollClients["leaky"]
		a.mu.Unlock()
		if ok {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	a.mu.Lock()
	_, registered := a.pollClients["leaky"]
	a.mu.Unlock()
	if !registered {
		t.Fatal("轮询通道应已注册")
	}

	// 取消上下文，触发轮询结束与通道清理
	cancel()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		_, ok := a.pollClients["leaky"]
		refs := a.pollRefs["leaky"]
		a.mu.Unlock()
		if !ok && refs == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	a.mu.Lock()
	_, left := a.pollClients["leaky"]
	a.mu.Unlock()
	if left {
		t.Error("轮询结束后通道应被清理")
	}
}
