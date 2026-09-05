// Package utils - Tencent SILK v3 audio conversion helpers.
// 对齐本体 astrbot/core/utils/tencent_record_helper.py 与 media_utils.py:
//   - 支持纯 Go (github.com/yoshino-s/silk-go) 对 Tencent 0x02-prefixed / 标准 SILK_V3 与 16-bit PCM WAV 进行互相转换；
//   - 支持检测与解析 WAV 头部、重采样/声道转换、封装合法标准 RIFF WAVE 文件；
//   - 若纯 Go silk 编解码失败或遇到非 PCM 格式，自动回退到 ffmpeg (带 -f silk 支持)，若 ffmpeg 未安装则提示用户安装。
package utils

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yoshino-s/silk-go"
)

// SilkSupportedRates 定义 SILK SDK 原生支持的采样率集合。
var SilkSupportedRates = map[int]bool{
	8000:  true,
	12000: true,
	16000: true,
	24000: true,
	32000: true,
	48000: true,
}

// TencentSilkToWAV 将 Tencent Silk 或标准 Silk 音频文件解码为 24kHz 单声道 16-bit PCM WAV 文件。
// 对齐本体 tencent_silk_to_wav(silk_path, output_path)。
// 优先使用纯 Go silk 解码；若纯 Go 解码失败则回退到 ffmpeg -f silk。
func TencentSilkToWAV(ctx context.Context, silkPath, outputPath string) (string, error) {
	// #nosec G304 -- silkPath 是调用方传入的待解码音频文件路径
	data, err := os.ReadFile(silkPath)
	if err != nil {
		return "", fmt.Errorf("读取 silk 文件失败: %w", err)
	}
	if len(data) == 0 {
		return "", errors.New("silk 数据为空")
	}

	// 1. 尝试使用纯 Go silk-go 库直接解码
	wavBytes, err := TencentSilkBytesToWAVBytes(data, 24000)
	if err == nil && len(wavBytes) > 0 {
		if err := os.MkdirAll(filepath.Dir(outputPath), 0750); err != nil {
			return "", err
		}
		if err := os.WriteFile(outputPath, wavBytes, 0600); err != nil {
			return "", fmt.Errorf("写入 WAV 文件失败: %w", err)
		}
		return outputPath, nil
	}

	// 2. 纯 Go 解码失败，尝试使用 ffmpeg 命令行回退 (-f silk)
	if ffmpegErr := decodeSilkWithFFmpeg(ctx, silkPath, outputPath); ffmpegErr == nil {
		return outputPath, nil
	} else if isFFmpegMissing(ffmpegErr) {
		return "", fmt.Errorf("SILK 转码失败且未检测到系统 ffmpeg，请安装 ffmpeg 后重试 (原生解码错误: %v)", err)
	} else {
		return "", fmt.Errorf("SILK 转码失败 (原生解码: %v, ffmpeg 错误: %v)", err, ffmpegErr)
	}
}

// WAVToTencentSilk 将 WAV 音频文件编码为 Tencent 兼容 (带 0x02 前缀) 的 SILK v3 文件。
// 对齐本体 wav_to_tencent_silk(wav_path, output_path)。
// 返回音频时长 (单位秒) 与错误。
func WAVToTencentSilk(ctx context.Context, wavPath, outputPath string) (float64, error) {
	// #nosec G304 -- wavPath 是调用方传入的输入文件路径
	wavData, err := os.ReadFile(wavPath)
	if err != nil {
		return 0, fmt.Errorf("读取 wav 文件失败: %w", err)
	}

	pcm, sampleRate, duration, err := ParseWAVToPCM16Mono(wavData, 24000)
	if err == nil && len(pcm) > 0 {
		// 使用纯 Go silk 编码
		silkBytes, encErr := silk.EncodePCM(pcm, silk.EncodeOptions{
			SampleRateHz:  int32(sampleRate),
			TencentCompat: 1, // 增加 0x02 前缀
		})
		if encErr == nil && len(silkBytes) > 0 {
			if err := os.MkdirAll(filepath.Dir(outputPath), 0750); err != nil {
				return 0, err
			}
			if err := os.WriteFile(outputPath, silkBytes, 0600); err != nil {
				return 0, fmt.Errorf("写入 silk 文件失败: %w", err)
			}
			return duration, nil
		}
	}

	// 尝试通过 ffmpeg 将原音频统一重采样转为 24k s16le PCM WAV 后再编码，或直接 ffmpeg 转 silk
	if ffmpegErr := encodeSilkWithFFmpeg(ctx, wavPath, outputPath); ffmpegErr == nil {
		// 读取输出并估算时长
		return duration, nil
	} else if isFFmpegMissing(ffmpegErr) {
		return 0, fmt.Errorf("WAV 转 SILK 失败且未检测到系统 ffmpeg，请安装 ffmpeg 后重试")
	} else {
		return 0, fmt.Errorf("WAV 转 SILK 失败: %v", ffmpegErr)
	}
}

// TencentSilkBytesToWAVBytes 将 Tencent Silk (或标准 SILK_V3) 内存字节直接解码并封装为 WAV 字节流。
func TencentSilkBytesToWAVBytes(silkData []byte, targetRate int) ([]byte, error) {
	if len(silkData) == 0 {
		return nil, errors.New("silk 数据为空")
	}
	if targetRate <= 0 {
		targetRate = 24000
	}

	// 去除可能存在的 QQ/微信 0x02 标记
	cleanData := silkData
	if cleanData[0] == 0x02 {
		cleanData = cleanData[1:]
	}

	pcm, err := silk.DecodePCM(cleanData, silk.DecodeOptions{
		SampleRateHz: int32(targetRate),
	})
	if err != nil {
		return nil, err
	}
	return WrapPCM16ToWAV(pcm, targetRate, 1)
}

// WrapPCM16ToWAV 将小端 int16 单声道/多声道 PCM 数据封装为标准 RIFF WAVE 文件字节。
func WrapPCM16ToWAV(pcmData []byte, sampleRate, numChannels int) ([]byte, error) {
	if numChannels <= 0 {
		numChannels = 1
	}
	if sampleRate <= 0 {
		sampleRate = 24000
	}

	dataSize := uint32(len(pcmData))
	fileSize := 36 + dataSize
	byteRate := uint32(sampleRate * numChannels * 2)
	blockAlign := uint16(numChannels * 2)

	buf := new(bytes.Buffer)
	buf.Grow(int(44 + dataSize))

	// RIFF Header
	buf.WriteString("RIFF")
	_ = binary.Write(buf, binary.LittleEndian, fileSize)
	buf.WriteString("WAVE")

	// fmt sub-chunk
	buf.WriteString("fmt ")
	_ = binary.Write(buf, binary.LittleEndian, uint32(16)) // Subchunk1Size for PCM
	_ = binary.Write(buf, binary.LittleEndian, uint16(1))  // AudioFormat 1 = PCM
	_ = binary.Write(buf, binary.LittleEndian, uint16(numChannels))
	_ = binary.Write(buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(buf, binary.LittleEndian, byteRate)
	_ = binary.Write(buf, binary.LittleEndian, blockAlign)
	_ = binary.Write(buf, binary.LittleEndian, uint16(16)) // BitsPerSample

	// data sub-chunk
	buf.WriteString("data")
	_ = binary.Write(buf, binary.LittleEndian, dataSize)
	buf.Write(pcmData)

	return buf.Bytes(), nil
}

// ParseWAVToPCM16Mono 解析 WAV 字节，提取 16-bit 单声道 PCM 数据，并在需要时进行简易声道混合/重采样。
func ParseWAVToPCM16Mono(wavData []byte, targetRate int) (pcm []byte, sampleRate int, duration float64, err error) {
	if len(wavData) < 44 {
		return nil, 0, 0, errors.New("wav 数据过短")
	}
	if string(wavData[:4]) != "RIFF" || string(wavData[8:12]) != "WAVE" {
		return nil, 0, 0, errors.New("非有效 RIFF/WAVE 文件")
	}

	reader := bytes.NewReader(wavData[12:])
	var (
		channels      uint16
		origRate      uint32
		bitsPerSample uint16
		pcmBytes      []byte
	)

	for {
		var chunkHeader struct {
			ID   [4]byte
			Size uint32
		}
		if err := binary.Read(reader, binary.LittleEndian, &chunkHeader); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, 0, 0, err
		}

		chunkID := string(chunkHeader.ID[:])
		if chunkID == "fmt " {
			var fmtChunk struct {
				AudioFormat   uint16
				NumChannels   uint16
				SampleRate    uint32
				ByteRate      uint32
				BlockAlign    uint16
				BitsPerSample uint16
			}
			if err := binary.Read(reader, binary.LittleEndian, &fmtChunk); err != nil {
				return nil, 0, 0, err
			}
			if fmtChunk.AudioFormat != 1 {
				return nil, 0, 0, fmt.Errorf("暂不支持非 PCM WAV 编码 (%d)", fmtChunk.AudioFormat)
			}
			channels = fmtChunk.NumChannels
			origRate = fmtChunk.SampleRate
			bitsPerSample = fmtChunk.BitsPerSample
			// 跳过 fmt 块多余的扩展字节
			if chunkHeader.Size > 16 {
				_, _ = reader.Seek(int64(chunkHeader.Size-16), io.SeekCurrent)
			}
		} else if chunkID == "data" {
			pcmBytes = make([]byte, chunkHeader.Size)
			if _, err := io.ReadFull(reader, pcmBytes); err != nil {
				return nil, 0, 0, err
			}
			break
		} else {
			// 跳过未知 chunk
			_, _ = reader.Seek(int64(chunkHeader.Size), io.SeekCurrent)
		}
	}

	if len(pcmBytes) == 0 {
		return nil, 0, 0, errors.New("wav 中未找到 data 数据块")
	}
	if bitsPerSample != 16 {
		return nil, 0, 0, fmt.Errorf("仅支持 16-bit PCM WAV (当前为 %d bit)", bitsPerSample)
	}

	sampleRate = int(origRate)
	if sampleRate <= 0 {
		sampleRate = 24000
	}

	// 转换为单声道
	monoPCM := pcmBytes
	if channels == 2 {
		monoPCM = stereoToMonoPCM16(pcmBytes)
	} else if channels > 2 {
		return nil, 0, 0, fmt.Errorf("暂不支持大于 2 声道的音频转换 (%d channels)", channels)
	}

	// 重采样判断 (如果 targetRate 指定且当前采样率不在 SILK 允许范围内)
	if !SilkSupportedRates[sampleRate] {
		if targetRate <= 0 {
			targetRate = 24000
		}
		monoPCM = simpleResamplePCM16(monoPCM, sampleRate, targetRate)
		sampleRate = targetRate
	}

	duration = float64(len(monoPCM)) / float64(sampleRate*2)
	return monoPCM, sampleRate, duration, nil
}

// stereoToMonoPCM16 双声道 s16le 转单声道 s16le。
func stereoToMonoPCM16(stereo []byte) []byte {
	numSamples := len(stereo) / 4
	mono := make([]byte, numSamples*2)
	for i := 0; i < numSamples; i++ {
		l := int16(binary.LittleEndian.Uint16(stereo[i*4 : i*4+2]))
		r := int16(binary.LittleEndian.Uint16(stereo[i*4+2 : i*4+4]))
		mixed := int16((int32(l) + int32(r)) / 2)
		binary.LittleEndian.PutUint16(mono[i*2:i*2+2], uint16(mixed))
	}
	return mono
}

// simpleResamplePCM16 简单线性插值重采样 (对齐基本语音转换场景)。
func simpleResamplePCM16(in []byte, inRate, outRate int) []byte {
	if inRate == outRate || len(in) < 2 {
		return in
	}
	inSamples := len(in) / 2
	outSamples := int(float64(inSamples) * float64(outRate) / float64(inRate))
	out := make([]byte, outSamples*2)

	for i := 0; i < outSamples; i++ {
		inPos := float64(i) * float64(inRate) / float64(outRate)
		idx0 := int(inPos)
		idx1 := idx0 + 1
		if idx1 >= inSamples {
			idx1 = inSamples - 1
		}
		frac := inPos - float64(idx0)

		s0 := int16(binary.LittleEndian.Uint16(in[idx0*2 : idx0*2+2]))
		s1 := int16(binary.LittleEndian.Uint16(in[idx1*2 : idx1*2+2]))
		val := int16(float64(s0)*(1.0-frac) + float64(s1)*frac)

		binary.LittleEndian.PutUint16(out[i*2:i*2+2], uint16(val))
	}
	return out
}

func decodeSilkWithFFmpeg(ctx context.Context, silkPath, outputPath string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, "ffmpeg", "-y",
		"-f", "silk",
		"-i", silkPath,
		"-acodec", "pcm_s16le",
		"-ar", "24000",
		"-ac", "1",
		outputPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg silk 解码失败: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func encodeSilkWithFFmpeg(ctx context.Context, inputPath, outputPath string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(cctx, "ffmpeg", "-y",
		"-i", inputPath,
		"-f", "silk",
		"-ar", "24000",
		"-ac", "1",
		outputPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg silk 编码失败: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func isFFmpegMissing(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, exec.ErrNotFound) {
		return true
	}
	return strings.Contains(err.Error(), "executable file not found")
}
