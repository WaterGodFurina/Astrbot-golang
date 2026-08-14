package dingtalk

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// media utils: 对应 Python media_utils.py 的音频/视频转换与时长获取。
// 依赖本机 ffmpeg/ffprobe, 不可用时降级处理 (返回原文件/时长 0)。

// ffmpegAvailable 检查 ffmpeg 是否可用。
func ffmpegAvailable() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// ffprobeAvailable 检查 ffprobe 是否可用。
func ffprobeAvailable() bool {
	_, err := exec.LookPath("ffprobe")
	return err == nil
}

// convertAudioFormat 将音频转换为目标格式 (ogg/amr)。
// 返回转换后的文件路径与是否发生了转换。ffmpeg 不可用时返回原文件 (不转换)。
// 对应 Python convert_audio_format。
func convertAudioFormat(inputPath, target string) (string, bool) {
	lower := strings.ToLower(inputPath)
	if strings.HasSuffix(lower, "."+target) {
		return inputPath, false
	}
	if !ffmpegAvailable() {
		logger.I18nWarn("未检测到 ffmpeg, 跳过音频格式转换: %s", inputPath)
		return inputPath, false
	}
	outPath := strings.TrimSuffix(inputPath, filepath.Ext(inputPath)) + "." + target
	cmd := exec.Command("ffmpeg", "-y", "-i", inputPath, "-acodec", "libopus", "-b:a", "32k", outPath)
	if target == "amr" {
		cmd = exec.Command("ffmpeg", "-y", "-i", inputPath, "-acodec", "libopencore_amrnb", outPath)
	}
	if err := cmd.Run(); err != nil {
		logger.I18nWarn("ffmpeg 音频转换失败 (%s): %v", target, err)
		return inputPath, false
	}
	return outPath, outPath != inputPath
}

// convertVideoToMP4 将视频转换为 mp4 格式。
// 对应 Python convert_video_format。返回转换后的路径与是否发生了转换。
func convertVideoToMP4(inputPath string) (string, bool) {
	lower := strings.ToLower(inputPath)
	if strings.HasSuffix(lower, ".mp4") {
		return inputPath, false
	}
	if !ffmpegAvailable() {
		logger.I18nWarn("未检测到 ffmpeg, 跳过视频格式转换: %s", inputPath)
		return inputPath, false
	}
	outPath := strings.TrimSuffix(inputPath, filepath.Ext(inputPath)) + ".mp4"
	cmd := exec.Command("ffmpeg", "-y", "-i", inputPath, "-c:v", "libx264", "-c:a", "aac", "-strict", "experimental", outPath)
	if err := cmd.Run(); err != nil {
		logger.I18nWarn("ffmpeg 视频转换失败: %v", err)
		return inputPath, false
	}
	return outPath, true
}

// extractVideoCover 提取视频封面图 (jpg)。
// 对应 Python extract_video_cover。返回封面路径, 失败时返回空字符串。
func extractVideoCover(videoPath string) string {
	if !ffmpegAvailable() {
		logger.I18nWarn("未检测到 ffmpeg, 无法提取视频封面: %s", videoPath)
		return ""
	}
	coverPath := strings.TrimSuffix(videoPath, filepath.Ext(videoPath)) + "_cover.jpg"
	cmd := exec.Command("ffmpeg", "-y", "-i", videoPath, "-frames:v", "1", coverPath)
	if err := cmd.Run(); err != nil {
		logger.I18nWarn("ffmpeg 提取视频封面失败: %v", err)
		return ""
	}
	return coverPath
}

// getMediaDuration 获取媒体时长(毫秒)。
// 对应 Python get_media_duration。ffprobe 不可用时返回 0。
func getMediaDuration(path string) int64 {
	if !ffprobeAvailable() {
		return 0
	}
	cmd := exec.Command("ffprobe", "-v", "quiet", "-show_entries", "format=duration", "-of", "csv=p=0", path)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0
	}
	return int64(seconds * 1000)
}

// safeRemoveFile 安全删除临时文件 (对应 Python _safe_remove_file)。
func safeRemoveFile(path string) {
	if path == "" {
		return
	}
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		if err := os.Remove(path); err != nil {
			logger.I18nWarn("清理临时文件失败: %s, %v", path, err)
		}
	}
}
