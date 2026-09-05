// 媒体下载/转码/上传工具。
// 对齐本体 weixin_offacc_adapter.py:460-484（voice 接收：media.download + ffmpeg amr→wav）
// 与 weixin_offacc_event.py:101-171（media.upload + 图片/语音回复）。
// 转码依赖宿主 ffmpeg，不可用时降级保留原格式（复用 discord/dingtalk 的 ffmpeg 先例）。
package weixin_official_account

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	wxmp "github.com/blusewang/wx/mp_api"
)

// transcodeTimeout 限制单次 ffmpeg 转码时长，避免异常输入长时间占用回调路径。
const transcodeTimeout = 30 * time.Second

// ffmpegAvailable 检查宿主 ffmpeg 是否可用。
func ffmpegAvailable() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// convertAudioToWavPath 将音频文件转为 wav（对齐 media_utils.convert_audio_format
// 的 wav 分支：ffmpeg -y -i in out.wav，无额外编码参数）。
// ffmpeg 不可用或转换失败时返回原路径（降级保留原格式）。
func convertAudioToWavPath(inputPath string) string {
	if !ffmpegAvailable() {
		logger.Warn("未检测到 ffmpeg，公众号语音跳过 wav 转码，保留原格式: %s", inputPath)
		return inputPath
	}
	outPath := strings.TrimSuffix(inputPath, filepath.Ext(inputPath)) + ".wav"
	ctx, cancel := context.WithTimeout(context.Background(), transcodeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-i", inputPath, outPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.Warn("ffmpeg wav 转码失败: %v: %s", err, truncateFFmpegOut(out))
		return inputPath
	}
	if _, err := os.Stat(outPath); err != nil {
		return inputPath
	}
	return outPath
}

// convertAudioToAmrPath 将音频文件转为 amr（公众号语音上传要求 amr/mp3，
// 对齐本体 convert_audio_to_amr：单声道 8kHz 12.2k + 带通滤波）。
// ffmpeg 不可用或转换失败时返回空串（调用方降级为文本回复）。
func convertAudioToAmrPath(inputPath string) string {
	if !ffmpegAvailable() {
		logger.Warn("未检测到 ffmpeg，公众号语音跳过 amr 转码: %s", inputPath)
		return ""
	}
	outPath := strings.TrimSuffix(inputPath, filepath.Ext(inputPath)) + ".amr"
	ctx, cancel := context.WithTimeout(context.Background(), transcodeTimeout)
	defer cancel()
	// 对齐 media_utils.convert_audio_format 的 amr 分支参数。
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-i", inputPath,
		"-ac", "1", "-ar", "8000", "-ab", "12.2k",
		"-af", "highpass=f=310:poles=2,lowpass=f=3720:poles=2,equalizer=f=3150:width_type=h:width=1000:g=7.5,loudnorm=I=-18.5:TP=-1.5:LRA=6,aresample=8000",
		outPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.Warn("ffmpeg amr 转码失败: %v: %s", err, truncateFFmpegOut(out))
		return ""
	}
	if _, err := os.Stat(outPath); err != nil {
		return ""
	}
	return outPath
}

// truncateFFmpegOut 压缩 ffmpeg 报错输出便于日志查看。
func truncateFFmpegOut(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

// downloadMediaByID 通过 media/get 接口下载临时素材（对应 wechatpy client.media.download）。
func (a *Adapter) downloadMediaByID(ctx context.Context, mediaID string) ([]byte, error) {
	resp, err := a.account.NewMpReq("cgi-bin/media/get").
		Query(&struct {
			MediaId string `url:"media_id"`
		}{MediaId: mediaID}).
		Download(ctx)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("media/get 返回 %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 20<<20))
	if err != nil {
		return nil, err
	}
	return data, nil
}

// mediaExtByFormat 依据语音 Format 字段推断扩展名。
func mediaExtByFormat(format string) string {
	if format == "" {
		return ".amr"
	}
	return "." + strings.ToLower(format)
}

// uploadMedia 上传临时素材并返回 media_id（对应 wechatpy client.media.upload）。
// mediaType: image / voice。
func (a *Adapter) uploadMedia(ctx context.Context, mediaType string, data []byte, ext string) (string, error) {
	f, err := os.CreateTemp("", "weixin_offacc_upload_*"+ext)
	if err != nil {
		return "", err
	}
	tmpPath := f.Name()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	_ = f.Close()
	defer os.Remove(tmpPath)

	in, err := os.Open(tmpPath)
	if err != nil {
		return "", err
	}
	defer in.Close()

	var res wxmp.MediaUploadRes
	req := a.account.NewMpReq(wxmp.MediaUpload).
		Query(&wxmp.MediaUploadQuery{Type: wxmp.MediaType(mediaType)}).
		Bind(&res)
	// SDK Upload 以 multipart/form-data 上传（filename 由 SDK 随机生成，扩展名取自参数）。
	if err := req.Upload(ctx, in, strings.TrimPrefix(ext, ".")); err != nil {
		return "", err
	}
	if res.MediaId == "" {
		return "", fmt.Errorf("media/upload 未返回 media_id")
	}
	return res.MediaId, nil
}

// sniffAudioExt 依据内容嗅探音频扩展名（微信语音通常为 amr）。
func sniffAudioExt(data []byte, fallback string) string {
	switch {
	case len(data) >= 6 && (string(data[:6]) == "#!AMR\n" || string(data[:6]) == "#!AMR\r"):
		return ".amr"
	case len(data) >= 4 && string(data[:4]) == "RIFF":
		return ".wav"
	case len(data) >= 3 && string(data[:3]) == "ID3", len(data) >= 2 && data[0] == 0xFF && data[1]&0xE0 == 0xE0:
		return ".mp3"
	default:
		return fallback
	}
}

// mimeExt 依据 mime 推断扩展名（wecom 同款兜底）。
// 供需要 Content-Type 推断的场景复用；公众号链路当前以内容嗅探为准。
func mimeExt(mimeType string) string {
	if ext, ok := map[string]string{
		"image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif",
		"image/webp": ".webp", "image/bmp": ".bmp",
	}[mimeType]; ok {
		return ext
	}
	if exts, err := mime.ExtensionsByType(mimeType); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}
