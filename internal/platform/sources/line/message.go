// LINE 消息链 → LINE Messaging API 消息对象转换。
// 1:1 移植自 astrbot/core/platform/sources/line/line_event.py。
//
// 与 Python 的差异说明（媒体处理）：
//   - Python 通过 register_to_file_service() 把本地文件注册到 AstrBot 文件服务
//     （callback_api_base/api/file/{token}），Go 端在本包内内置了一个轻量 HTTP
//     文件服务器，按同样的 URL 形态（/api/file/{token}）提供本地媒体文件。
//   - 音频时长/视频预览图依赖 ffmpeg/ffprobe，Go 端做尽力而为调用，
//     不可用时回退到默认时长 1000ms / 跳过该消息段。
package line

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/line/line-bot-sdk-go/v8/linebot/messaging_api"

	"github.com/WaterGodFurina/Astrbot-golang/internal/utils"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// 文本消息长度上限（LINE 限制为 5000 字符，Python 侧同样截断）。
const lineTextLimit = 5000

// truncateRunes 按 Unicode 码点截断字符串（LINE 的 5000 限制按字符计，
// 按字节截断会产生 UTF-8 乱码）。
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// fileMessage 是 LINE file 消息对象（官方 SDK 未生成该模型，按协议手写）。
// 对应 Python 的 {"type": "file", "fileName": ..., "fileSize": ..., "originalContentUrl": ...}。
type fileMessage struct {
	Type               string `json:"type"`
	FileName           string `json:"fileName"`
	FileSize           int64  `json:"fileSize"`
	OriginalContentURL string `json:"originalContentUrl"`
}

// GetType 实现 messaging_api.MessageInterface。
func (m *fileMessage) GetType() string { return "file" }

// buildLineMessages 将消息链转换为 LINE 消息对象列表。
// 对应 Python 的 build_line_messages：最多 5 条，超限丢弃并告警。
func (a *Adapter) buildLineMessages(ctx context.Context, chain *message.MessageChain) ([]messaging_api.MessageInterface, error) {
	if chain == nil {
		return nil, nil
	}
	var messages []messaging_api.MessageInterface
	for _, comp := range chain.Chain {
		obj := a.componentToMessageObject(ctx, comp)
		if obj != nil {
			messages = append(messages, obj)
		}
	}
	if len(messages) == 0 {
		return nil, nil
	}
	if len(messages) > 5 {
		lineLogger.I18nWarn("[LINE] 消息数量超过 5 条，多余消息段将被丢弃")
		messages = messages[:5]
	}
	return messages, nil
}

// componentToMessageObject 将单个消息组件转换为 LINE 消息对象。
// 对应 Python 的 _component_to_message_object。
func (a *Adapter) componentToMessageObject(ctx context.Context, comp message.Component) messaging_api.MessageInterface {
	switch c := comp.(type) {
	case *message.Plain:
		text := strings.TrimSpace(c.Text)
		if text == "" {
			return nil
		}
		if len([]rune(text)) > lineTextLimit {
			text = truncateRunes(text, lineTextLimit)
		}
		return &messaging_api.TextMessage{Text: text}
	case *message.At:
		name := strings.TrimSpace(c.Name)
		if name == "" {
			name = strings.TrimSpace(c.TargetID)
		}
		if name == "" {
			return nil
		}
		text := "@" + name
		if len([]rune(text)) > lineTextLimit {
			text = truncateRunes(text, lineTextLimit)
		}
		return &messaging_api.TextMessage{Text: text}
	case *message.Image:
		imageURL := a.resolveImageURL(ctx, c)
		if imageURL == "" {
			return nil
		}
		return &messaging_api.ImageMessage{
			OriginalContentUrl: imageURL,
			PreviewImageUrl:    imageURL,
		}
	case *message.Record:
		audioURL := a.resolveRecordURL(ctx, c)
		if audioURL == "" {
			return nil
		}
		return &messaging_api.AudioMessage{
			OriginalContentUrl: audioURL,
			Duration:           int64(a.resolveRecordDuration(ctx, c)),
		}
	case *message.Video:
		videoURL := a.resolveVideoURL(ctx, c)
		if videoURL == "" {
			return nil
		}
		previewURL := a.resolveVideoPreviewURL(ctx, c)
		if previewURL == "" {
			return nil
		}
		return &messaging_api.VideoMessage{
			OriginalContentUrl: videoURL,
			PreviewImageUrl:    previewURL,
		}
	case *message.File:
		fileURL := a.resolveFileURL(ctx, c)
		if fileURL == "" {
			return nil
		}
		fileName := strings.TrimSpace(c.Name)
		if fileName == "" {
			fileName = "file.bin"
		}
		fileSize := a.resolveFileSize(ctx, c)
		if fileSize <= 0 {
			return nil
		}
		return &fileMessage{
			Type:               "file",
			FileName:           fileName,
			FileSize:           fileSize,
			OriginalContentURL: fileURL,
		}
	}
	return nil
}

// resolveImageURL 解析图片 URL。
// 对应 Python 的 _resolve_image_url：https 直连，本地文件注册到文件服务。
func (a *Adapter) resolveImageURL(ctx context.Context, img *message.Image) string {
	candidate := strings.TrimSpace(img.URL)
	if candidate == "" {
		candidate = strings.TrimSpace(img.File)
	}
	if candidate == "" {
		candidate = strings.TrimSpace(img.Path)
	}
	if strings.HasPrefix(candidate, "https://") {
		return candidate
	}
	path := candidate
	if path == "" {
		path = img.Path
	}
	if path == "" {
		path = img.File
	}
	if path != "" && isLocalFile(path) {
		return a.registerToFileService(path)
	}
	return ""
}

// resolveRecordURL 解析音频 URL。
// 对应 Python 的 _resolve_record_url。
func (a *Adapter) resolveRecordURL(ctx context.Context, rec *message.Record) string {
	candidate := strings.TrimSpace(rec.URL)
	if candidate == "" {
		candidate = strings.TrimSpace(rec.File)
	}
	if candidate == "" {
		candidate = strings.TrimSpace(rec.Path)
	}
	if strings.HasPrefix(candidate, "https://") {
		return candidate
	}
	path := candidate
	if path == "" {
		path = rec.Path
	}
	if path == "" {
		path = rec.File
	}
	if path != "" && isLocalFile(path) {
		return a.registerToFileService(path)
	}
	return ""
}

// resolveRecordDuration 解析音频时长（毫秒）。
// 对应 Python 的 _resolve_record_duration：优先 ffprobe，失败回退 1000ms。
func (a *Adapter) resolveRecordDuration(ctx context.Context, rec *message.Record) int {
	path := strings.TrimSpace(rec.Path)
	if path == "" {
		path = strings.TrimSpace(rec.File)
	}
	if path == "" {
		path = strings.TrimSpace(rec.URL)
	}
	if duration, ok := probeMediaDuration(path); ok {
		return duration
	}
	return 1000
}

// resolveVideoURL 解析视频 URL。
// 对应 Python 的 _resolve_video_url。
func (a *Adapter) resolveVideoURL(ctx context.Context, v *message.Video) string {
	candidate := strings.TrimSpace(v.URL)
	if candidate == "" {
		candidate = strings.TrimSpace(v.Path)
	}
	if strings.HasPrefix(candidate, "https://") {
		return candidate
	}
	path := candidate
	if path == "" {
		path = v.Path
	}
	if path != "" && isLocalFile(path) {
		return a.registerToFileService(path)
	}
	return ""
}

// resolveVideoPreviewURL 解析视频预览图 URL。
// 对应 Python 的 _resolve_video_preview_url：优先 cover 字段，
// 否则用 ffmpeg 提取首帧生成缩略图。
func (a *Adapter) resolveVideoPreviewURL(ctx context.Context, v *message.Video) string {
	cover := strings.TrimSpace(v.URL)
	if strings.HasPrefix(cover, "https://") {
		return cover
	}
	if cover != "" && isLocalFile(cover) {
		return a.registerToFileService(cover)
	}

	videoPath := strings.TrimSpace(v.Path)
	if videoPath != "" && isLocalFile(videoPath) {
		if thumb, ok := extractVideoFrame(videoPath); ok {
			return a.registerOwnedFileToService(thumb)
		}
	}
	return ""
}

// resolveFileURL 解析文件 URL。
// 对应 Python 的 _resolve_file_url。
func (a *Adapter) resolveFileURL(ctx context.Context, f *message.File) string {
	candidate := strings.TrimSpace(f.URL)
	if strings.HasPrefix(candidate, "https://") {
		return candidate
	}
	path := candidate
	if path == "" {
		path = f.Path
	}
	if path != "" && isLocalFile(path) {
		return a.registerToFileService(path)
	}
	return ""
}

// resolveFileSize 解析文件大小（字节）。
// 对应 Python 的 _resolve_file_size：本地文件 stat 大小，失败返回 0。
func (a *Adapter) resolveFileSize(ctx context.Context, f *message.File) int64 {
	if f.Size > 0 {
		return f.Size
	}
	path := strings.TrimSpace(f.Path)
	if path == "" {
		path = strings.TrimSpace(f.URL)
	}
	if !isLocalFile(path) {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// isLocalFile 判断路径是否为本地存在的文件。
func isLocalFile(path string) bool {
	if path == "" || strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// ── 本地媒体文件服务 ──────────────────────────────────────────────
//
// 对应 Python 的 register_to_file_service()（callback_api_base + 文件 token
// 服务）。由于本包不能依赖外部文件服务，这里内置一个按需启动的 HTTP 文件
// 服务器：URL 形态为 http://<host>:<port>/api/file/{token}，token 与文件路径
// 一一对应，供 LINE 服务器回源拉取本地媒体。

// fileServer 是包级单例的本地媒体文件服务。
var (
	mediaFileServer *mediaServer
	mediaServerOnce sync.Once
)

// mediaTokenTTL 是媒体文件 token 的有效期：LINE 在发送后立即回源拉取，
// 超过该时间未拉取的 token（及适配器创建的临时文件）将被清理，
// 防止 tokens 只增不减。
const mediaTokenTTL = time.Hour

// mediaTokenEntry 记录 token 对应的文件路径、注册时间与归属。
type mediaTokenEntry struct {
	path  string
	owned bool // owned 表示文件由本适配器创建的临时文件，token 过期时一并删除
	at    time.Time
}

// mediaServer 提供本地媒体文件的 HTTP 访问。
type mediaServer struct {
	mux     *http.ServeMux
	srv     *http.Server
	port    int
	tokens  map[string]mediaTokenEntry // token -> 文件信息
	tokenMu sync.RWMutex
}

// startMediaServer 启动（或复用）本地媒体文件服务器，返回 baseURL。
func startMediaServer() (string, error) {
	var err error
	mediaServerOnce.Do(func() {
		ms := &mediaServer{
			mux:    http.NewServeMux(),
			tokens: map[string]mediaTokenEntry{},
		}
		ms.mux.HandleFunc("/api/file/", ms.handleFile)
		// 在 127.0.0.1 上寻找可用端口（从 6185 起尝试）
		port := 6185
		var ln net.Listener
		for {
			ln, err = net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
			if err == nil {
				break
			}
			port++
			if port > 6200 {
				return
			}
		}
		ms.port = port
		ms.srv = &http.Server{Handler: ms.mux}
		go func() {
			if serveErr := ms.srv.Serve(ln); serveErr != nil && serveErr != http.ErrServerClosed {
				lineLogger.I18nWarn("[LINE] 本地媒体文件服务器退出: %v", serveErr)
			}
		}()
		mediaFileServer = ms
	})
	if err != nil || mediaFileServer == nil {
		return "", fmt.Errorf("启动本地媒体文件服务器失败: %v", err)
	}
	return fmt.Sprintf("http://127.0.0.1:%d", mediaFileServer.port), nil
}

// handleFile 提供 /api/file/{token} 的媒体文件下载。
func (ms *mediaServer) handleFile(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.URL.Path, "/api/file/")
	ms.tokenMu.RLock()
	entry := ms.tokens[token]
	ms.tokenMu.RUnlock()
	if entry.path == "" {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, entry.path)
}

// sweepExpiredTokensLocked 删除过期 token；owned 的临时文件一并清理。
// 调用方需持有 tokenMu 写锁。
func (ms *mediaServer) sweepExpiredTokensLocked() {
	now := time.Now()
	for token, entry := range ms.tokens {
		if now.Sub(entry.at) > mediaTokenTTL {
			if entry.owned {
				_ = os.Remove(entry.path)
			}
			delete(ms.tokens, token)
		}
	}
}

// registerFile 把本地文件注册到文件服务并返回可访问 URL。
// owned 表示该文件由本适配器创建的临时文件，token 过期时一并删除。
func (ms *mediaServer) registerFile(path string, owned bool) (string, error) {
	ms.tokenMu.Lock()
	ms.sweepExpiredTokensLocked()
	token := randomHex(16)
	ms.tokens[token] = mediaTokenEntry{path: path, owned: owned, at: time.Now()}
	ms.tokenMu.Unlock()
	return fmt.Sprintf("/api/file/%s", token), nil
}

// fileServiceBase 确保本地媒体文件服务器已启动，返回其 baseURL（加锁保护 mediaBaseURL）。
func (a *Adapter) fileServiceBase() string {
	a.mediaMu.Lock()
	defer a.mediaMu.Unlock()
	if a.mediaBaseURL == "" {
		base, err := startMediaServer()
		if err != nil {
			lineLogger.I18nWarn("[LINE] 注册文件到文件服务失败: %v", err)
			return ""
		}
		a.mediaBaseURL = base
	}
	return a.mediaBaseURL
}

// registerToFileService 把本地文件注册到媒体文件服务，返回完整 URL。
// 对应 Python 的 segment.register_to_file_service()：优先使用配置的
// callback_api_base 公网地址拼接，缺失时回退到本机地址。
func (a *Adapter) registerToFileService(path string) string {
	return a.registerFileToService(path, false)
}

// registerOwnedFileToService 注册本适配器创建的临时文件（token 过期时随文件一并清理）。
func (a *Adapter) registerOwnedFileToService(path string) string {
	return a.registerFileToService(path, true)
}

// registerFileToService 把本地文件注册到媒体文件服务并返回完整 URL。
func (a *Adapter) registerFileToService(path string, owned bool) string {
	base := a.fileServiceBase()
	if base == "" {
		return ""
	}
	rel, err := mediaFileServer.registerFile(path, owned)
	if err != nil {
		return ""
	}
	cb := strings.TrimRight(a.callbackAPIBase, "/")
	if cb == "" {
		lineLogger.I18nWarn("[LINE] 未配置 callback_api_base，媒体 URL 将使用本机地址，LINE 服务器无法访问")
		return base + rel
	}
	if !strings.HasPrefix(cb, "https://") {
		lineLogger.I18nWarn("[LINE] callback_api_base 不是 HTTPS 地址（LINE 要求公网 HTTPS），媒体消息发送可能失败: %s", cb)
	}
	return cb + rel
}

// probeMediaDuration 通过 ffprobe 获取媒体时长（毫秒），失败返回 (0, false)。
func probeMediaDuration(path string) (int, bool) {
	if !isLocalFile(path) {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path,
	).Output()
	if err != nil {
		return 0, false
	}
	sec, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || sec <= 0 {
		return 0, false
	}
	return int(sec * 1000), true
}

// extractVideoFrame 通过 ffmpeg 提取视频第 1 秒的一帧作为预览图。
// 对应 Python 的 _resolve_video_preview_url 中 ffmpeg 提取缩略图的逻辑。
func extractVideoFrame(videoPath string) (string, bool) {
	thumbPath := utils.TempFilePath("line_video_preview_" + randomHex(6) + ".jpg")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y", "-ss", "00:00:01",
		"-i", videoPath,
		"-frames:v", "1",
		thumbPath,
	)
	_ = cmd.Run()
	if _, err := os.Stat(thumbPath); err != nil {
		return "", false
	}
	return thumbPath, true
}

// randomHex 生成指定字节长度的随机十六进制字符串。
func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(buf)
}

// marshalMessage 将消息对象序列化为 JSON（测试辅助）。
func marshalMessage(m messaging_api.MessageInterface) (string, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
