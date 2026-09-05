package lark

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 飞书媒体转码：对齐本体 lark_event.py:594-665（音频 opus）、667-738（视频 mp4）
// 与 media_utils.convert_audio_to_opus / convert_video_format / get_media_duration。
// 依赖宿主 ffmpeg/ffprobe，不可用时降级为原文件直接上传。

// transcodeTimeout 限制单次 ffmpeg 转码时长，避免异常输入长时间占用发送路径。
const transcodeTimeout = 120 * time.Second

// ffmpegReady 检查 ffmpeg 是否可用。
func ffmpegReady() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// ffprobeReady 检查 ffprobe 是否可用。
func ffprobeReady() bool {
	_, err := exec.LookPath("ffprobe")
	return err == nil
}

// getMediaDuration 获取媒体时长（毫秒），对齐本体 get_media_duration；
// 探测失败返回 0（上传时不带 duration）。
func getMediaDuration(path string) int {
	if !ffprobeReady() {
		return 0
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
		return 0
	}
	sec, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil || sec <= 0 {
		return 0
	}
	return int(sec * 1000)
}

// convertAudioToOpus 将音频转换为 opus（对齐 media_utils.convert_audio_format
// 的 opus 分支：-acodec libopus -ac 1 -ar 16000）。
// 已是 .opus 后缀时原样返回；ffmpeg 不可用/转换失败返回 (原路径, false)。
func convertAudioToOpus(ctx context.Context, inputPath string) (string, bool) {
	if strings.HasSuffix(strings.ToLower(inputPath), ".opus") {
		return inputPath, false
	}
	if !ffmpegReady() {
		logger.I18nWarn("未检测到 ffmpeg，跳过 opus 转码，将原样上传: %s", inputPath)
		return inputPath, false
	}
	outPath := filepath.Join(os.TempDir(), "lark_audio_"+strconv.FormatInt(time.Now().UnixNano(), 10)+".opus")
	cctx, cancel := context.WithTimeout(ctx, transcodeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "ffmpeg", "-y",
		"-i", inputPath,
		"-acodec", "libopus",
		"-ac", "1",
		"-ar", "16000",
		outPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.Error("ffmpeg opus 转码失败，将尝试直接上传原文件: %v: %s", err, truncateFFmpegLog(out))
		_ = os.Remove(outPath)
		return inputPath, false
	}
	return outPath, true
}

// convertVideoToMp4 将视频转换为 mp4（对齐 media_utils.convert_video_format：
// -c:v libx264 -c:a aac -strict experimental）。
// 已是 .mp4 后缀时原样返回；ffmpeg 不可用/转换失败返回 (原路径, false)。
func convertVideoToMp4(ctx context.Context, inputPath string) (string, bool) {
	if strings.HasSuffix(strings.ToLower(inputPath), ".mp4") {
		return inputPath, false
	}
	if !ffmpegReady() {
		logger.I18nWarn("未检测到 ffmpeg，跳过 mp4 转码，将原样上传: %s", inputPath)
		return inputPath, false
	}
	outPath := filepath.Join(os.TempDir(), "lark_video_"+strconv.FormatInt(time.Now().UnixNano(), 10)+".mp4")
	cctx, cancel := context.WithTimeout(ctx, transcodeTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "ffmpeg", "-y",
		"-i", inputPath,
		"-c:v", "libx264",
		"-c:a", "aac",
		"-strict", "experimental",
		outPath,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.Error("ffmpeg mp4 转码失败，将尝试直接上传原文件: %v: %s", err, truncateFFmpegLog(out))
		_ = os.Remove(outPath)
		return inputPath, false
	}
	return outPath, true
}

// truncateFFmpegLog 压缩 ffmpeg 报错输出便于日志查看。
func truncateFFmpegLog(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

// safeRemove 清理转码产生的临时文件（忽略不存在/失败）。
func safeRemove(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		logger.Debug("删除飞书转码临时文件失败 %s: %v", path, err)
	}
}
