// Package mattermost - Mattermost 平台适配器单元测试。
// 测试不连接真实网络：消息解析 / 发送参数构造 / WebSocket 事件处理用纯函数与 httptest 验证。
package mattermost

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// ---------- 消息解析 ----------

func TestParseTextComponents(t *testing.T) {
	adapter := New(map[string]interface{}{
		"id":                   "mattermost",
		"mattermost_url":       "https://chat.example.com",
		"mattermost_bot_token": "token",
	}, nil, nil)
	adapter.botUsername = "astrbot"
	adapter.botSelfID = "bot-1"

	// 普通文本（无提及）
	comps := adapter.parseTextComponents("hello world")
	if len(comps) != 1 || comps[0].Type() != message.CompPlain {
		t.Fatalf("期望一个 Plain 组件，得到 %v", comps)
	}

	// 提及机器人：应拆成 Plain + At + Plain
	comps = adapter.parseTextComponents("hi @astrbot how are you")
	if len(comps) != 3 {
		t.Fatalf("期望 3 个组件，得到 %d: %v", len(comps), comps)
	}
	if comps[1].Type() != message.CompAt {
		t.Fatalf("期望第二个组件为 At，得到 %v", comps[1].Type())
	}
	at := comps[1].(*message.At)
	if at.TargetID != "bot-1" || at.Name != "astrbot" {
		t.Fatalf("At 组件字段错误: %+v", at)
	}

	// 大小写不敏感（_build_mention_pattern 使用 re.IGNORECASE）
	comps = adapter.parseTextComponents("@ASTRBOT hi")
	if len(comps) != 2 || comps[0].Type() != message.CompAt {
		t.Fatalf("期望 2 个组件且第一个为 At，得到 %v", comps)
	}

	// 类似用户名不应误匹配：@astrbot_foo 不是提及
	comps = adapter.parseTextComponents("see @astrbot_foo")
	if len(comps) != 1 || comps[0].Type() != message.CompPlain {
		t.Fatalf("@astrbot_foo 不应被识别为提及: %v", comps)
	}

	// 空文本
	if comps := adapter.parseTextComponents(""); len(comps) != 0 {
		t.Fatalf("空文本应返回空组件，得到 %v", comps)
	}
}

func TestBuildMessageStr(t *testing.T) {
	// 开头的自我提及（前无文本）应被跳过
	comps := []message.Component{
		&message.At{TargetID: "bot-1", Name: "astrbot"},
		&message.Plain{Text: "你好"},
	}
	if got := buildMessageStr(comps, "", "bot-1"); got != "你好" {
		t.Fatalf("开头自我提及应被跳过，得到 %q", got)
	}

	// 非开头自我提及保留
	comps = []message.Component{
		&message.Plain{Text: "看看 "},
		&message.At{TargetID: "bot-1", Name: "astrbot"},
	}
	if got := buildMessageStr(comps, "", "bot-1"); got != "看看 @astrbot" {
		t.Fatalf("非开头自我提及应保留，得到 %q", got)
	}

	// 空结果回退到 fallback
	if got := buildMessageStr(nil, " fallback ", "bot-1"); got != "fallback" {
		t.Fatalf("空结果应回退 fallback，得到 %q", got)
	}

	// At 无 name 时使用 TargetID
	comps = []message.Component{
		&message.Plain{Text: "hi "},
		&message.At{TargetID: "user-9"},
	}
	if got := buildMessageStr(comps, "", "bot-1"); got != "hi @user-9" {
		t.Fatalf("At 无 name 时应使用 TargetID，得到 %q", got)
	}
}

func TestParseTimestamp(t *testing.T) {
	// 毫秒时间戳转为秒
	if got := parseTimestamp(float64(1730000000000)); got != 1730000000 {
		t.Fatalf("毫秒时间戳转换错误: %d", got)
	}
	// 秒级时间戳保持不变
	if got := parseTimestamp(float64(1730000000)); got != 1730000000 {
		t.Fatalf("秒级时间戳应保持不变: %d", got)
	}
	// 非法值使用当前时间
	if got := parseTimestamp("nope"); got <= 0 {
		t.Fatalf("非法时间戳应回退到当前时间: %d", got)
	}
}

func TestParseWebsocketPost(t *testing.T) {
	raw := `{"id":"p1","user_id":"u1","channel_id":"c1","message":"hello","create_at":1730000000000}`
	post := ParseWebsocketPost(raw)
	if post == nil {
		t.Fatal("合法 JSON 应被解析")
	}
	if post["id"] != "p1" || post["user_id"] != "u1" {
		t.Fatalf("解析字段错误: %v", post)
	}

	// 非法 JSON 返回 nil
	if post := ParseWebsocketPost("not json{{"); post != nil {
		t.Fatal("非法 JSON 应返回 nil")
	}
	// 非对象 JSON 返回 nil
	if post := ParseWebsocketPost(`[1,2,3]`); post != nil {
		t.Fatal("数组 JSON 应返回 nil")
	}
}

func TestDedup(t *testing.T) {
	adapter := New(nil, nil, nil)
	adapter.dedupTTL = 300
	if adapter.isDuplicatePost("p1") {
		t.Fatal("首次出现不应判重")
	}
	if !adapter.isDuplicatePost("p1") {
		t.Fatal("重复帖子应判重")
	}
	if adapter.isDuplicatePost("p2") {
		t.Fatal("其他帖子不应判重")
	}
}

// ---------- 发送参数构造（httptest 模拟 Mattermost 服务端） ----------

// newMattermostTestServer 启动一个模拟 Mattermost 服务端，
// 记录收到的 POST 请求以便断言发送参数。
func newMattermostTestServer(t *testing.T) (*httptest.Server, *[]map[string]interface{}) {
	t.Helper()
	posts := make([]map[string]interface{}, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v4/users/me" {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "bot-1", "username": "astrbot",
			})
			return
		}
		if r.URL.Path == "/api/v4/files" && r.Method == http.MethodPost {
			// multipart 上传：校验 channel_id 字段存在
			if err := r.ParseMultipartForm(10 << 20); err != nil {
				t.Errorf("multipart 解析失败: %v", err)
				w.WriteHeader(400)
				return
			}
			fileHeaders := r.MultipartForm.File["files"]
			if len(fileHeaders) == 0 {
				t.Errorf("multipart 缺少 files 字段")
				w.WriteHeader(400)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"file_infos": []map[string]interface{}{
					{"id": "file-uploaded", "name": fileHeaders[0].Filename},
				},
			})
			return
		}
		if r.URL.Path == "/api/v4/posts" && r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			var post map[string]interface{}
			if err := json.Unmarshal(body, &post); err != nil {
				t.Errorf("post 请求体解析失败: %v", err)
				w.WriteHeader(400)
				return
			}
			posts = append(posts, post)
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "sent-post"})
			return
		}
		w.WriteHeader(404)
	}))
	return srv, &posts
}

func TestSendMessageChainPayload(t *testing.T) {
	srv, posts := newMattermostTestServer(t)
	defer srv.Close()

	client := NewMattermostClient(srv.URL, "test-token")

	// 构造一个本地临时图片文件
	tmpDir := t.TempDir()
	imgPath := filepath.Join(tmpDir, "pic.png")
	pngBytes := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00}
	if err := os.WriteFile(imgPath, pngBytes, 0644); err != nil {
		t.Fatal(err)
	}

	chain := message.NewMessageChain(
		&message.Plain{Text: "你好 "},
		&message.At{TargetID: "user-9", Name: "user9"},
		&message.Reply{MessageID: "reply-1"},
		&message.Image{Path: imgPath},
	)

	ctx := context.Background()
	if _, err := client.SendMessageChain(ctx, "channel-1", chain); err != nil {
		t.Fatalf("发送消息链失败: %v", err)
	}

	if len(*posts) != 1 {
		t.Fatalf("期望一次 POST /posts，实际 %d 次", len(*posts))
	}
	post := (*posts)[0]
	if post["channel_id"] != "channel-1" {
		t.Fatalf("channel_id 错误: %v", post["channel_id"])
	}
	// 文本与 At 合并，首尾去除空白
	if post["message"] != "你好 @user9" {
		t.Fatalf("message 构造错误: %q", post["message"])
	}
	// 图片上传后 file_id 应附加在 file_ids 中
	fileIDs, _ := post["file_ids"].([]interface{})
	if len(fileIDs) != 1 || fileIDs[0] != "file-uploaded" {
		t.Fatalf("file_ids 错误: %v", post["file_ids"])
	}
	// Reply 应作为 root_id
	if post["root_id"] != "reply-1" {
		t.Fatalf("root_id 错误: %v", post["root_id"])
	}
}

func TestUploadFilePayload(t *testing.T) {
	srv, _ := newMattermostTestServer(t)
	defer srv.Close()

	client := NewMattermostClient(srv.URL, "test-token")
	fileID, err := client.UploadFile(context.Background(), "channel-2", []byte("hello"), "doc.txt", "text/plain")
	if err != nil {
		t.Fatalf("上传失败: %v", err)
	}
	if fileID != "file-uploaded" {
		t.Fatalf("file id 错误: %s", fileID)
	}
}

func TestParsePostAttachments(t *testing.T) {
	// 附件解析需要 files/{id}/info 与 files/{id} 两个端点
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/info") {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "f1", "name": "cat.png", "mime_type": "image/png",
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/f1") {
			_, _ = w.Write([]byte{0x89, 0x50, 0x4E, 0x47})
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()

	client := NewMattermostClient(srv.URL, "test-token")
	components, tempPaths := client.ParsePostAttachments(context.Background(), []string{"f1"})
	if len(components) != 1 || components[0].Type() != message.CompImage {
		t.Fatalf("期望一个 Image 组件，得到 %v", components)
	}
	if len(tempPaths) != 1 {
		t.Fatalf("期望 1 个临时文件，得到 %v", tempPaths)
	}
	img := components[0].(*message.Image)
	if _, err := os.Stat(img.Path); err != nil {
		t.Fatalf("临时文件不存在: %v", err)
	}
	_ = os.Remove(img.Path)
}

// ---------- WebSocket 事件处理 ----------

func TestHandleWsEvent(t *testing.T) {
	adapter := New(map[string]interface{}{
		"id": "mattermost", "mattermost_url": "https://x", "mattermost_bot_token": "t",
	}, nil, nil)
	adapter.botSelfID = "bot-1"
	adapter.botUsername = "astrbot"

	// 非 posted 事件被忽略
	adapter.handleWsEvent(map[string]interface{}{"event": "hello"})

	// posted 事件：构造标准负载
	rawPost := `{"id":"p1","user_id":"u1","channel_id":"c1","message":"@astrbot 你好","create_at":1730000000000}`
	adapter.handleWsEvent(map[string]interface{}{
		"event": "posted",
		"data": map[string]interface{}{
			"channel_type": "O",
			"sender_name":  "@alice",
			"post":         rawPost,
		},
	})

	// 无法直接断言事件总线（EventBus 为 nil 时仅记录日志），
	// 这里验证转换结果：重复帖子应被去重（第二次调用不产生新转换）
	if !adapter.isDuplicatePost("p1") {
		t.Fatal("handleWsEvent 后 p1 应被记入去重表")
	}

	// Bot 自己的帖子被忽略
	ownPost := `{"id":"p2","user_id":"bot-1","channel_id":"c1","message":"hi"}`
	adapter.handleWsEvent(map[string]interface{}{
		"event": "posted",
		"data":  map[string]interface{}{"channel_type": "O", "post": ownPost},
	})
	if adapter.isDuplicatePost("p2") {
		t.Fatal("机器人自己的帖子不应被处理")
	}

	// 带 type 的帖子（系统消息）被忽略
	systemPost := `{"id":"p3","user_id":"u1","channel_id":"c1","message":"hi","type":"system_join_leave"}`
	adapter.handleWsEvent(map[string]interface{}{
		"event": "posted",
		"data":  map[string]interface{}{"channel_type": "O", "post": systemPost},
	})
	if adapter.isDuplicatePost("p3") {
		t.Fatal("系统消息不应被处理")
	}
}

func TestConvertMessageGroup(t *testing.T) {
	adapter := New(nil, nil, nil)
	adapter.botSelfID = "bot-1"
	adapter.botUsername = "astrbot"

	post := map[string]interface{}{
		"id": "p1", "user_id": "u1", "channel_id": "c1",
		"message": "@astrbot 你好", "create_at": float64(1730000000000),
	}
	data := map[string]interface{}{"channel_type": "O", "sender_name": "@alice"}
	abm := adapter.convertMessage(post, data)
	if abm == nil {
		t.Fatal("转换失败")
	}
	if abm.Type != "GroupMessage" {
		t.Fatalf("频道类型错误: %s", abm.Type)
	}
	if abm.SessionID != "c1" || abm.MessageID != "p1" {
		t.Fatalf("会话/消息 ID 错误: %+v", abm)
	}
	if abm.Sender.UserID != "u1" || abm.Sender.Nickname != "alice" {
		t.Fatalf("发送者错误: %+v", abm.Sender)
	}
	if abm.Timestamp != 1730000000 {
		t.Fatalf("时间戳错误: %d", abm.Timestamp)
	}
	// 组件应包含 At + Plain，message_str 跳过开头自我提及
	if len(abm.Message) != 2 || abm.Message[0].Type() != message.CompAt {
		t.Fatalf("组件错误: %v", abm.Message)
	}
	if abm.MessageStr != "你好" {
		t.Fatalf("message_str 错误: %q", abm.MessageStr)
	}
}

func TestConvertMessageDirect(t *testing.T) {
	adapter := New(nil, nil, nil)
	adapter.botSelfID = "bot-1"
	adapter.botUsername = "astrbot"

	post := map[string]interface{}{
		"id": "p1", "user_id": "u1", "channel_id": "c1", "message": "hi",
	}
	data := map[string]interface{}{"channel_type": "D"}
	abm := adapter.convertMessage(post, data)
	if abm.Type != "FriendMessage" {
		t.Fatalf("私聊类型错误: %s", abm.Type)
	}
	if abm.Group != nil {
		t.Fatal("私聊不应有 Group")
	}
}

func TestBuildWSURL(t *testing.T) {
	if got := buildWSURL("https://chat.example.com"); got != "wss://chat.example.com/api/v4/websocket" {
		t.Fatalf("https 转换错误: %s", got)
	}
	if got := buildWSURL("http://chat.example.com:8065"); got != "ws://chat.example.com:8065/api/v4/websocket" {
		t.Fatalf("http 转换错误: %s", got)
	}
}

// ── L-37：大小写折叠不改变字节长度 ──────────────────────────────

func TestAsciiLower(t *testing.T) {
	if got := asciiLower("ABCxyz"); got != "abcxyz" {
		t.Errorf("asciiLower(ABCxyz) = %q", got)
	}
	// 非 ASCII 字符原样保留，字节长度不变
	s := "İ@Astrbot"
	if lower := asciiLower(s); len(lower) != len(s) {
		t.Errorf("asciiLower 应保持字节长度: %d -> %d", len(s), len(lower))
	}
}

func TestFindMentionSpansUnicode(t *testing.T) {
	// İ 经 strings.ToLower 字节长度会变（2→3），旧实现在 lowerText 上取下标
	// 再对原文切片，导致提及切分错位；asciiLower 保持等长。
	text := "İ@astrbot hi"
	spans := findMentionSpans(text, "astrbot")
	if len(spans) != 1 {
		t.Fatalf("期望 1 个提及 span，得到 %v", spans)
	}
	if spans[0][0] != 2 || spans[0][1] != 10 {
		t.Fatalf("span 应为 [2,10]，得到 %v", spans)
	}
	// 切分后的文本片段应保持完整可读（不包含半截字节/错位）
	adapter := New(nil, nil, nil)
	adapter.botSelfID = "bot-1"
	adapter.botUsername = "astrbot"
	comps := adapter.parseTextComponents(text)
	if len(comps) != 3 {
		t.Fatalf("期望 Plain+At+Plain 三个组件，得到 %d: %v", len(comps), comps)
	}
	if plain, ok := comps[0].(*message.Plain); !ok || plain.Text != "İ" {
		t.Errorf("组件 0 应为 Plain(İ)，得到 %v", comps[0])
	}
	if at, ok := comps[1].(*message.At); !ok || at.Name != "astrbot" {
		t.Errorf("组件 1 应为 At(astrbot)，得到 %v", comps[1])
	}
	if plain, ok := comps[2].(*message.Plain); !ok || plain.Text != " hi" {
		t.Errorf("组件 2 应为 Plain( hi)，得到 %v", comps[2])
	}
}
