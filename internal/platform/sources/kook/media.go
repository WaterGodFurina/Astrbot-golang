// kook 媒体工具: 对应 Python kook_adapter.py 中 MediaResolver 的音频转码。
// 依赖本机 ffmpeg, 不可用时降级处理 (保留原文件)。
package kook

import (
	"context"
	"github.com/WaterGodFurina/Astrbot-golang/internal/utils"

	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// audioExtFromURL 从音频资源 URL 中提取原始格式扩展名 (去除 query 部分);
// 无法识别时返回 "audio"。
func audioExtFromURL(src string) string {
	u := src
	if parsed, err := url.Parse(src); err == nil {
		u = parsed.Path
	}
	ext := strings.TrimPrefix(filepath.Ext(u), ".")
	if ext == "" || len(ext) > 8 {
		return "audio"
	}
	return strings.ToLower(ext)
}

// convertAudioToWav 用 ffmpeg 将音频转换为 wav。
// 对应 Python kook_adapter.py:
// MediaResolver(file_url, media_type="audio", default_suffix=".wav").to_path(target_format="wav")。
// 支持 silk 优先纯 Go 快速解码；ffmpeg 不可用或转换失败时返回原路径 (降级保留原格式); 转换成功后清理原文件。
func convertAudioToWav(inputPath string) string {
	if strings.HasSuffix(strings.ToLower(inputPath), ".wav") {
		return inputPath
	}
	outPath := strings.TrimSuffix(inputPath, filepath.Ext(inputPath)) + ".wav"
	// 若是 silk 格式，优先纯 Go 转码
	if utils.DetectAudioFormat(inputPath) == "silk" {
		if _, err := utils.TencentSilkToWAV(context.Background(), inputPath, outPath); err == nil {
			_ = os.Remove(inputPath)
			return outPath
		}
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		logger.I18nWarn("[KOOK] 未检测到 ffmpeg, 跳过音频转 wav: %s。如果没有安装 ffmpeg 请先安装。", inputPath)
		return inputPath
	}
	cmd := exec.Command("ffmpeg", "-y", "-i", inputPath, outPath)
	if err := cmd.Run(); err != nil {
		logger.I18nWarn("[KOOK] ffmpeg 音频转 wav 失败: %v", err)
		return inputPath
	}
	if outPath != inputPath {
		_ = os.Remove(inputPath)
	}
	return outPath
}
