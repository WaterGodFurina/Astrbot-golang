package qqofficial

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// ---------------------------------------------------------------------------
// 发送媒体类型（M-53 回归）：Record 应为语音（3）、Video 为视频（2）。
// ---------------------------------------------------------------------------

func TestExtractSendPartsMediaType(t *testing.T) {
	// 语音（Record）→ fileTypeVoice
	plain, _, fileRef, _, fileType := extractSendParts(&message.MessageChain{
		Chain: []message.Component{&message.Record{URL: "https://x.com/v.silk"}},
	})
	if plain != "" || fileRef != "https://x.com/v.silk" {
		t.Errorf("Record 提取异常: plain=%q fileRef=%q", plain, fileRef)
	}
	if fileType != fileTypeVoice {
		t.Errorf("Record 应使用 fileTypeVoice(%d)，实际 %d", fileTypeVoice, fileType)
	}

	// 视频（Video）→ fileTypeVideo
	_, _, _, _, fileType = extractSendParts(&message.MessageChain{
		Chain: []message.Component{&message.Video{URL: "https://x.com/v.mp4"}},
	})
	if fileType != fileTypeVideo {
		t.Errorf("Video 应使用 fileTypeVideo(%d)，实际 %d", fileTypeVideo, fileType)
	}

	// 文件（File）→ fileTypeFile 且保留文件名
	_, _, _, fileName, fileType := extractSendParts(&message.MessageChain{
		Chain: []message.Component{&message.File{Name: "doc.pdf", URL: "https://x.com/d.pdf"}},
	})
	if fileName != "doc.pdf" || fileType != fileTypeFile {
		t.Errorf("File 提取异常: fileName=%q fileType=%d", fileName, fileType)
	}

	// 空链默认文件类型
	_, _, _, _, fileType = extractSendParts(&message.MessageChain{Chain: []message.Component{}})
	if fileType != fileTypeFile {
		t.Errorf("空链应默认 fileTypeFile(%d)，实际 %d", fileTypeFile, fileType)
	}
}

// ---------------------------------------------------------------------------
// WebSocket 并发写串行化（M-52 回归）：心跳与 identify/resume 写并发时
// 所有帧都应完整且可解析。
// ---------------------------------------------------------------------------

func TestConcurrentWritesAreSerialized(t *testing.T) {
	upgrader := websocket.Upgrader{}
	var mu sync.Mutex
	var total, badFrames int
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if json.Unmarshal(data, &map[string]interface{}{}) != nil {
				mu.Lock()
				badFrames++
				mu.Unlock()
			}
			mu.Lock()
			total++
			finish := total >= 200
			mu.Unlock()
			if finish {
				close(done)
				return
			}
		}
	}))
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/gateway"
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket 连接失败: %v", err)
	}
	defer ws.Close()

	a := New(map[string]interface{}{"id": "t"}, nil, nil)
	a.mu.Lock()
	a.ws = ws
	a.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.heartbeatLoop(ctx, time.Millisecond)

	// 与心跳并发写 identify/resume 帧
	for i := 0; i < 100; i++ {
		go func(i int) {
			_ = a.sendFrame(ws, wsIdentify, map[string]interface{}{"i": i})
		}(i)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("未收到足够的帧（写入可能被阻塞）")
	}
	mu.Lock()
	defer mu.Unlock()
	if badFrames > 0 {
		t.Errorf("收到 %d 个损坏帧：并发写未串行化", badFrames)
	}
}
