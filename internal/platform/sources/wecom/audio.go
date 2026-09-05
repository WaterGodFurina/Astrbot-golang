// 企业微信音频转码工具。
// 对齐本体 media_utils.convert_audio_format（amr 分支）与
// MediaResolver(target_format="wav") 的 wav 分支：
//   - 接收：企业微信语音为 amr，经 ffmpeg 转 wav 后投递进消息管线
//     （wecom_adapter.py:396-404 / convert_wechat_kf_message voice 分支）；
//   - 发送：任意格式语音经 ffmpeg 转 amr 后上传（wecom_event.py:131-133, 230-233
//     convert_audio_to_amr）。
//
// 依赖宿主 ffmpeg，不可用/转换失败时降级为原样返回（复用 discord/media.go 先例）。
package wecom

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// audioTranscodeTimeout 限制单次 ffmpeg 转码时长，避免异常输入长时间占用收发路径。
const audioTranscodeTimeout = 30 * time.Second

// ffmpegAvailable 检查 ffmpeg 是否可用。
func ffmpegAvailable() bool {
	_, err := exec.LookPath("ffmpeg")
	return err == nil
}

// convertAudioToWav 将内存中的音频数据（企业微信语音为 amr）转为 wav。
// 对齐 MediaResolver(target_format="wav") → ensure_wav 的 wav 分支：
// ffmpeg -y -i in out.wav，无额外编码参数。
// 输入已是 wav（RIFF 魔数）时原样返回；ffmpeg 不可用或转换失败时返回原数据降级。
func convertAudioToWav(data []byte) []byte {
	if len(data) == 0 {
		return data
	}
	// RIFF/WAV 魔数：无需转码（对应 _get_audio_magic_type == "wav" 短路）。
	if len(data) >= 4 && bytes.Equal(data[:4], []byte("RIFF")) {
		return data
	}
	if !ffmpegAvailable() {
		logger.I18nWarn("未检测到 ffmpeg，企业微信语音跳过 wav 转码，原样保留 amr 文件")
		return data
	}
	inPath, outPath, err := writeTempAudioPair(data, ".wav")
	if err != nil {
		logger.I18nWarn("企业微信语音转码创建临时文件失败: %v", err)
		return data
	}
	defer func() {
		_ = os.Remove(inPath)
		_ = os.Remove(outPath)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), audioTranscodeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-i", inPath, outPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.I18nError("转换音频失败: %v: %s。如果没有安装 ffmpeg 请先安装。", err, truncateFFmpegOutput(out))
		return data
	}
	converted, err := os.ReadFile(outPath)
	if err != nil {
		logger.I18nWarn("读取 wav 转码结果失败: %v", err)
		return data
	}
	return converted
}

// convertAudioToAMR 将本地音频文件转为 amr 并返回新文件路径。
// 对齐本体 media_utils.convert_audio_format 的 amr 分支（amr_nb 8000Hz）：
//
//	ffmpeg -y -i in -ac 1 -ar 8000 -ab 12.2k \
//	  -af "highpass=f=310:poles=2,lowpass=f=3720:poles=2,\
//	equalizer=f=3150:width_type=h:width=1000:g=7.5,\
//	loudnorm=I=-18.5:TP=-1.5:LRA=6,aresample=8000" out.amr
//
// 输入已是 amr 魔数时原样返回；ffmpeg 不可用或转换失败时返回原路径降级上传。
func convertAudioToAMR(path string) string {
	if path == "" {
		return path
	}
	// AMR 魔数 "#!AMR"：无需转码（对应 suffix/format 短路判断）。
	if detectAMRMagic(path) {
		return path
	}
	if !ffmpegAvailable() {
		logger.I18nWarn("未检测到 ffmpeg，企业微信语音跳过 amr 转码，原样上传")
		return path
	}
	outPath := filepath.Join(os.TempDir(), "astrbot_wecom_media_audio_"+randomHex(8)+".amr")
	args := []string{"-y", "-i", path,
		"-ac", "1",
		"-ar", "8000",
		"-ab", "12.2k",
		"-af", "highpass=f=310:poles=2,lowpass=f=3720:poles=2,equalizer=f=3150:width_type=h:width=1000:g=7.5,loudnorm=I=-18.5:TP=-1.5:LRA=6,aresample=8000",
		outPath,
	}
	ctx, cancel := context.WithTimeout(context.Background(), audioTranscodeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		logger.I18nWarn("ffmpeg amr 转码失败: %v: %s", err, truncateFFmpegOutput(out))
		return path
	}
	if _, err := os.Stat(outPath); err != nil {
		logger.I18nWarn("amr 转码结果不存在: %v", err)
		return path
	}
	return outPath
}

// writeTempAudioPair 为内存音频转码写入输入临时文件，返回 (输入路径, 输出路径)。
func writeTempAudioPair(data []byte, outSuffix string) (string, string, error) {
	inFile, err := os.CreateTemp(os.TempDir(), "astrbot_wecom_audio_in_*")
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
	outPath := inPath + outSuffix
	return inPath, outPath, nil
}

// detectAMRMagic 通过文件头魔数判断是否为 AMR 音频。
func detectAMRMagic(path string) bool {
	// #nosec G304 -- path 为调用方指定的待检测音频文件（消息组件/素材路径）。
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	header := make([]byte, 5)
	n, _ := f.Read(header)
	return n >= 5 && bytes.Equal(header[:5], []byte("#!AMR"))
}

// truncateFFmpegOutput 压缩 ffmpeg 报错输出便于日志查看。
func truncateFFmpegOutput(out []byte) string {
	s := strings.TrimSpace(string(out))
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
