package discord

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// 音频转码工具：对齐本体 discord_platform_event.py:201-236 —— Record 组件经
// MediaResolver(target_format="wav") 转为 wav 后以 audio.wav 上传。
// 依赖宿主 ffmpeg，不可用时降级为原样上传（复用 dingtalk/line 的 ffmpeg 先例）。

// maxTranscodeSeconds 限制单次 ffmpeg 转码时长，避免异常输入长时间占用发送路径。
const maxTranscodeSeconds = 30 * time.Second

// ffmpegAvailable 检查 ffmpeg 是否可用。
func ffmpegAvailable() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// convertAudioToWav 将内存中的音频数据转为 wav（对齐 media_utils.convert_audio_format
// 的 wav 分支：ffmpeg -y -i in out.wav，无额外编码参数）。
// 输入已是 wav（RIFF 魔数）时原样返回；ffmpeg 不可用或转换失败时返回原数据降级上传。
func convertAudioToWav(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	// RIFF/WAV 魔数：无需转码。
	if bytes.Equal(data[:4], []byte("RIFF")) {
		return data
	}
	if !ffmpegAvailable() {
		logger.I18nWarn("未检测到 ffmpeg，Discord 语音跳过 wav 转码，原样上传")
		return data
	}
	inPath, outPath, err := writeTempAudioPair(data)
	if err != nil {
		logger.I18nWarn("Discord 语音转码创建临时文件失败: %v", err)
		return data
	}
	defer func() {
		_ = os.Remove(inPath)
		_ = os.Remove(outPath)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), maxTranscodeSeconds)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-i", inPath, outPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.I18nWarn("ffmpeg wav 转码失败: %v: %s", err, truncateOutput(out))
		return data
	}
	converted, err := os.ReadFile(outPath)
	if err != nil {
		logger.I18nWarn("读取 wav 转码结果失败: %v", err)
		return data
	}
	return converted
}

// writeTempAudioPair 为转码写入输入临时文件并返回 (输入路径, 输出路径)。
func writeTempAudioPair(data []byte) (string, string, error) {
	tmpDir := os.TempDir()
	inFile, err := os.CreateTemp(tmpDir, "discord_audio_in_*")
	if err != nil {
		return "", "", err
	}
	inPath := inFile.Name()
	if _, err := inFile.Write(data); err != nil {
		_ = inFile.Close()
		_ = os.Remove(inPath)
		return "", "", err
	}
	if err := inFile.Close(); err != nil {
		_ = os.Remove(inPath)
		return "", "", err
	}
	outPath := filepath.Join(tmpDir, fmt.Sprintf("discord_audio_out_%d.wav", time.Now().UnixNano()))
	return inPath, outPath, nil
}

// truncateOutput 压缩 ffmpeg 报错输出便于日志查看。
func truncateOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > 300 {
		return s[:300]
	}
	return s
}

// pickFirst 返回第一个非空字符串（Record 组件字段优先级辅助）。
func pickFirst(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// recordAudioData 拉取 Record 组件的音频字节：优先本地路径，其次 base64，
// 最后经 SSRF 校验的 URL 下载。全部失败返回 nil。
func (a *Adapter) recordAudioData(url, path, base64Data string) []byte {
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			return data
		}
		logger.I18nWarn("Discord 语音文件读取失败: %s", path)
		return nil
	}
	if base64Data != "" {
		data, err := base64.StdEncoding.DecodeString(base64Data)
		if err != nil {
			logger.I18nWarn("Discord 语音 base64 解码失败: %v", err)
			return nil
		}
		return data
	}
	if url != "" {
		f := fetchFile(url, "audio")
		if f == nil {
			return nil
		}
		defer func() {
			if rc, ok := f.Reader.(io.Closer); ok {
				_ = rc.Close()
			}
		}()
		data, err := io.ReadAll(f.Reader)
		if err != nil {
			logger.I18nWarn("Discord 语音下载失败: %v", err)
			return nil
		}
		return data
	}
	return nil
}
