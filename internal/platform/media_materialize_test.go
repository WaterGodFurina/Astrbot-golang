package platform

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// newMediaServer 提供按路径返回不同内容的本地媒体服务（127.0.0.1，放行于 host 守卫）。
func newMediaServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/image.png":
			_, _ = w.Write([]byte("image-bytes"))
		case "/voice.ogg":
			_, _ = w.Write([]byte("voice-bytes"))
		case "/video.mp4":
			_, _ = w.Write([]byte("video-bytes"))
		case "/file.pdf":
			_, _ = w.Write([]byte("file-bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func assertTempContent(t *testing.T, path, want string) {
	t.Helper()
	if path == "" {
		t.Fatalf("expected a materialized local temp path, got empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read temp file %s: %v", path, err)
	}
	if string(data) != want {
		t.Fatalf("temp file %s content = %q, want %q", path, data, want)
	}
}

// TestMaterializeRemoteMediaDownloadsAllMediaTypes 验证 Image/Record/Video/File
// 的远程 URL 被下载为本地临时文件（Path/File 赋值），URL 保留，cleanup 删除全部临时文件。
func TestMaterializeRemoteMediaDownloadsAllMediaTypes(t *testing.T) {
	srv := newMediaServer(t)
	chain := message.NewMessageChain(
		message.ImageFromURL(srv.URL+"/image.png"),
		&message.Record{URL: srv.URL + "/voice.ogg"},
		&message.Video{URL: srv.URL + "/video.mp4"},
		&message.File{URL: srv.URL + "/file.pdf"},
	)

	cleanup := materializeRemoteMedia(chain)
	defer cleanup()

	img := chain.Chain[0].(*message.Image)
	if img.URL != srv.URL+"/image.png" {
		t.Fatalf("Image.URL 应保留, got %q", img.URL)
	}
	assertTempContent(t, img.Path, "image-bytes")
	if img.File != img.Path {
		t.Fatalf("Image.File 应等于 Path, got File=%q Path=%q", img.File, img.Path)
	}

	rec := chain.Chain[1].(*message.Record)
	assertTempContent(t, rec.Path, "voice-bytes")
	if rec.File != rec.Path {
		t.Fatalf("Record.File 应等于 Path, got File=%q Path=%q", rec.File, rec.Path)
	}

	vid := chain.Chain[2].(*message.Video)
	assertTempContent(t, vid.Path, "video-bytes")

	f := chain.Chain[3].(*message.File)
	assertTempContent(t, f.Path, "file-bytes")

	paths := []string{img.Path, rec.Path, vid.Path, f.Path}
	cleanup()
	for _, p := range paths {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("temp file %s 应在 cleanup 后被删除, stat err = %v", p, err)
		}
	}
}

// TestMaterializeNestedReplyAndNodes 验证嵌套的 Reply.Chain 与 Nodes.Content 也被递归物化。
func TestMaterializeNestedReplyAndNodes(t *testing.T) {
	srv := newMediaServer(t)
	chain := message.NewMessageChain(
		&message.Reply{Chain: []message.Component{
			message.ImageFromURL(srv.URL + "/image.png"),
			&message.Plain{Text: "reply-text"},
		}},
		&message.Nodes{Nodes: []*message.Node{
			{Content: []message.Component{
				&message.Record{URL: srv.URL + "/voice.ogg"},
			}},
		}},
	)

	cleanup := materializeRemoteMedia(chain)
	defer cleanup()

	rep := chain.Chain[0].(*message.Reply)
	img := rep.Chain[0].(*message.Image)
	assertTempContent(t, img.Path, "image-bytes")

	nodes := chain.Chain[1].(*message.Nodes)
	rec := nodes.Nodes[0].Content[0].(*message.Record)
	assertTempContent(t, rec.File, "voice-bytes")
}

// TestMaterializeSkipsExistingLocalPath 验证已有本地路径的组件不被下载、URL 保留。
func TestMaterializeSkipsExistingLocalPath(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "local.png")
	if err := os.WriteFile(local, []byte("local-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	chain := message.NewMessageChain(
		&message.Image{URL: "http://127.0.0.1:1/never.png", Path: local, File: local},
		&message.File{URL: "http://127.0.0.1:1/never.bin", Path: local},
	)
	cleanup := materializeRemoteMedia(chain)
	defer cleanup()

	img := chain.Chain[0].(*message.Image)
	if img.Path != local {
		t.Fatalf("本地路径组件不应被物化, Path=%q 应保持 %q", img.Path, local)
	}
	if img.URL != "http://127.0.0.1:1/never.png" {
		t.Fatalf("URL 应保留, got %q", img.URL)
	}
	f := chain.Chain[1].(*message.File)
	if f.Path != local {
		t.Fatalf("本地路径组件不应被物化, Path=%q 应保持 %q", f.Path, local)
	}
}

// TestMaterializeSkipsRejectedHost 验证被 host 守卫拒绝的 URL 不被下载、URL 保留。
func TestMaterializeSkipsRejectedHost(t *testing.T) {
	chain := message.NewMessageChain(
		&message.Record{URL: "http://169.254.169.254/latest/meta-data/"},
		&message.Video{URL: "http://[fe80::1]/v.mp4"},
	)
	cleanup := materializeRemoteMedia(chain)
	defer cleanup()

	rec := chain.Chain[0].(*message.Record)
	if rec.Path != "" || rec.File != "" {
		t.Fatalf("链路本地地址不应被下载, Path=%q File=%q", rec.Path, rec.File)
	}
	if rec.URL == "" {
		t.Fatal("被拒绝的 URL 应保留")
	}
	v := chain.Chain[1].(*message.Video)
	if v.Path != "" {
		t.Fatalf("链路本地地址不应被下载, Path=%q", v.Path)
	}
}

// TestMaterializeDownloadFailurePreservesURL 验证下载失败（不可达）时组件原样保留。
func TestMaterializeDownloadFailurePreservesURL(t *testing.T) {
	chain := message.NewMessageChain(
		&message.Video{URL: "http://127.0.0.1:1/v.mp4"},
		&message.Image{URL: "http://127.0.0.1:1/i.png"},
	)
	cleanup := materializeRemoteMedia(chain)
	defer cleanup()

	v := chain.Chain[0].(*message.Video)
	if v.Path != "" {
		t.Fatalf("下载失败不应设置 Path, got %q", v.Path)
	}
	if v.URL == "" {
		t.Fatal("下载失败后 URL 应保留")
	}
	img := chain.Chain[1].(*message.Image)
	if img.Path != "" {
		t.Fatalf("下载失败不应设置 Path, got %q", img.Path)
	}
}

// TestMediaHostAllowed 验证地址守卫：拒绝链路本地/组播/未指定，放行环回与 RFC1918 私网。
func TestMediaHostAllowed(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"http://127.0.0.1:8080/a.png", true},
		{"http://192.168.1.100/a.png", true},
		{"http://10.0.0.5/a.png", true},
		{"http://172.16.0.5/a.png", true},
		{"http://169.254.169.254/latest/meta-data/", false},
		{"http://[fe80::1]/a", false},
		{"http://[::1]/a", true},
		{"http://224.0.0.1/a", false},
		{"http://0.0.0.0/a", false},
		{"http://[::]/a", false},
		{"http:///a", false},
		{"not a url", false},
		{"http://nonexistent.invalid/a", false}, // DNS 失败 → fail-closed
	}
	for _, c := range cases {
		if got := mediaHostAllowed(c.url); got != c.want {
			t.Errorf("mediaHostAllowed(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

// TestDownloadMediaToTempSuccess 验证下载成功、写入临时文件并保留净化扩展名。
func TestDownloadMediaToTempSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello-media"))
	}))
	defer srv.Close()

	path, err := downloadMediaToTemp(srv.URL + "/record.wav?token=abc")
	if err != nil {
		t.Fatalf("download failed: %v", err)
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello-media" {
		t.Fatalf("content = %q, want %q", data, "hello-media")
	}
	if !strings.HasSuffix(path, ".wav") {
		t.Fatalf("临时文件应保留 .wav 扩展名, got %s", path)
	}
}

// TestDownloadMediaToTempNon2xx 验证非 2xx 响应直接失败。
func TestDownloadMediaToTempNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := downloadMediaToTemp(srv.URL + "/a.png"); err == nil {
		t.Fatal("非 2xx 响应应失败")
	}
}

// TestDownloadMediaToTempOversize 验证超过大小上限的媒体下载失败。
func TestDownloadMediaToTempOversize(t *testing.T) {
	old := maxMediaDownloadSize
	maxMediaDownloadSize = 1024
	t.Cleanup(func() { maxMediaDownloadSize = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 4096))
	}))
	defer srv.Close()

	_, err := downloadMediaToTemp(srv.URL + "/big.bin")
	if err == nil || !strings.Contains(err.Error(), "超过") {
		t.Fatalf("超限下载应失败, got %v", err)
	}
}

// TestDownloadMediaToTempTimeout 验证下载超时失败。
func TestDownloadMediaToTempTimeout(t *testing.T) {
	old := mediaDownloadTimeout
	mediaDownloadTimeout = 150 * time.Millisecond
	t.Cleanup(func() { mediaDownloadTimeout = old })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	if _, err := downloadMediaToTemp(srv.URL + "/slow.bin"); err == nil {
		t.Fatal("下载超时应失败")
	}
}

// TestSanitizedURLExt 验证扩展名净化：忽略 query、无扩展名、垃圾后缀。
func TestSanitizedURLExt(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://x.com/a.mp3", ".mp3"},
		{"https://x.com/a.mp3?token=1&sig=2", ".mp3"},
		{"https://x.com/a", ""},
		{"https://x.com/", ""},
		{"https://x.com/a.MP3", ".mp3"},
		{"https://x.com/a.bad-ext", ".badext"},
		{"https://x.com/a.mp3/trailer", ""},
		{"not a url", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := sanitizedURLExt(c.in); got != c.want {
			t.Errorf("sanitizedURLExt(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// materializeTestAdapter 是一个通过 PlatformManager.Send 验证物化契约的假适配器：
// 在 Send 期间读取已下载的本地临时文件并校验内容，可选地模拟发送失败。
type materializeTestAdapter struct {
	id   string
	typ  string
	seen int
	fail bool
}

func (a *materializeTestAdapter) ID() string   { return a.id }
func (a *materializeTestAdapter) Type() string { return a.typ }
func (a *materializeTestAdapter) Start(context.Context) error {
	return nil
}
func (a *materializeTestAdapter) Stop() error { return nil }
func (a *materializeTestAdapter) Send(_ string, chain *message.MessageChain) error {
	a.seen++
	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.Image:
			if c.Path == "" || c.File == "" {
				return errors.New("image 未被物化为本地文件")
			}
			data, err := os.ReadFile(c.Path)
			if err != nil {
				return err
			}
			if string(data) != "image-bytes" {
				return errors.New("image 本地文件内容不匹配")
			}
		case *message.Record:
			if c.File == "" {
				return errors.New("record 未被物化为本地文件")
			}
			data, err := os.ReadFile(c.File)
			if err != nil {
				return err
			}
			if string(data) != "voice-bytes" {
				return errors.New("record 本地文件内容不匹配")
			}
		}
	}
	if a.fail {
		return errors.New("模拟发送失败")
	}
	return nil
}

// TestPlatformManagerSendMaterializesAndCleansUp 集成测试：经 PlatformManager.Send
// 发送含 URL Image/Record 的链，适配器在 Send 期间能读到已下载的本地文件，
// Send 返回后临时文件被 cleanup 删除。
func TestPlatformManagerSendMaterializesAndCleansUp(t *testing.T) {
	srv := newMediaServer(t)
	adapter := &materializeTestAdapter{id: "mock", typ: "mock"}
	pm := NewPlatformManager()
	pm.Register(adapter)

	chain := message.NewMessageChain(
		message.ImageFromURL(srv.URL+"/image.png"),
		&message.Record{URL: srv.URL + "/voice.ogg"},
	)
	if err := pm.Send("mock", "session-1", chain); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if adapter.seen != 1 {
		t.Fatalf("期望 1 次发送, got %d", adapter.seen)
	}

	// Send 返回后，materialize 的临时文件应已被 cleanup 删除
	img := chain.Chain[0].(*message.Image)
	if _, err := os.Stat(img.Path); !os.IsNotExist(err) {
		t.Fatalf("image 临时文件 %s 应在 Send 后被清理, stat err = %v", img.Path, err)
	}
	rec := chain.Chain[1].(*message.Record)
	if _, err := os.Stat(rec.File); !os.IsNotExist(err) {
		t.Fatalf("record 临时文件 %s 应在 Send 后被清理, stat err = %v", rec.File, err)
	}
}

// TestPlatformManagerSendCleansUpOnSendError 验证 adapter.Send 失败时临时文件仍被清理。
func TestPlatformManagerSendCleansUpOnSendError(t *testing.T) {
	srv := newMediaServer(t)
	adapter := &materializeTestAdapter{id: "mock", typ: "mock", fail: true}
	pm := NewPlatformManager()
	pm.Register(adapter)

	chain := message.NewMessageChain(
		message.ImageFromURL(srv.URL + "/image.png"),
	)
	if err := pm.Send("mock", "s", chain); err == nil {
		t.Fatal("模拟发送失败时应返回 error")
	}
	img := chain.Chain[0].(*message.Image)
	if _, err := os.Stat(img.Path); !os.IsNotExist(err) {
		t.Fatalf("Send 失败后临时文件 %s 也应被清理, stat err = %v", img.Path, err)
	}
}
