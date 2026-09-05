// 企业微信智能机器人音频转码工具。
// 对齐本体 media_utils.convert_audio_format（amr 分支）：
// 消息推送 webhook 的语音上传要求 amr 格式，非 amr 音频先经 ffmpeg 转码
// （wecomai_webhook.py:199-218）。依赖宿主 ffmpeg，不可用/转换失败时
// 降级为原样返回（复用 discord/media.go 先例）。
package wecom_ai_bot

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// audioTranscodeTimeout 限制单次 ffmpeg 转码时长，避免异常输入长时间占用发送路径。
const audioTranscodeTimeout = 30 * time.Second

// convertAudioToAMR 将本地音频文件转为 amr 并返回新文件路径。
// 对齐本体 convert_audio_format 的 amr 分支参数（amr_nb 8000Hz）：
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
	// AMR 魔数 "#!AMR"：无需转码。
	if detectAMRMagic(path) {
		return path
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		logger.I18nWarn("未检测到 ffmpeg，企业微信智能机器人语音跳过 amr 转码，原样上传")
		return path
	}
	// 转码产物以 astrbot_wecomai_ 前缀命名，便于发送后统一清理。
	outPath := filepath.Join(os.TempDir(), "astrbot_wecomai_audio_"+randomHexID()+".amr")
	args := []string{"-y", "-i", path,
		"-ac", "1",
		"-ar", "8000",
		"-ab", "12.2k",
		"-af", "highpass=f=310:poles=2,lowpass=f=3720:poles=2,equalizer=f=3150:width_type=h:width=1000:g=7.5,loudnorm=I=-18.5:TP=-1.5:LRA=6,aresample=8000",
		outPath,
	}
	ctx, cancel := context.WithTimeout(context.Background(), audioTranscodeTimeout)
	defer cancel()
	// #nosec G204 -- args 各元素均为固定参数或受控路径，无外部命令注入面。
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

// detectAMRMagic 通过文件头魔数判断是否为 AMR 音频。
func detectAMRMagic(path string) bool {
	// #nosec G304 -- path 为消息组件指定的待检测音频文件，属预期功能。
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
