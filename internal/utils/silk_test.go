package utils

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestWrapAndParseWAVPCM(t *testing.T) {
	// 生成 1 秒 24000Hz 的 16-bit 单声道静音/测试数据
	sampleRate := 24000
	numSamples := sampleRate
	pcmIn := make([]byte, numSamples*2)
	for i := 0; i < numSamples; i++ {
		pcmIn[i*2] = byte(i & 0xFF)
		pcmIn[i*2+1] = byte((i >> 8) & 0xFF)
	}

	wavBytes, err := WrapPCM16ToWAV(pcmIn, sampleRate, 1)
	if err != nil {
		t.Fatalf("WrapPCM16ToWAV failed: %v", err)
	}
	if len(wavBytes) != 44+len(pcmIn) {
		t.Fatalf("unexpected wav length: got %d, want %d", len(wavBytes), 44+len(pcmIn))
	}

	parsedPCM, parsedRate, duration, err := ParseWAVToPCM16Mono(wavBytes, 24000)
	if err != nil {
		t.Fatalf("ParseWAVToPCM16Mono failed: %v", err)
	}
	if parsedRate != sampleRate {
		t.Errorf("expected rate %d, got %d", sampleRate, parsedRate)
	}
	if len(parsedPCM) != len(pcmIn) {
		t.Errorf("expected pcm length %d, got %d", len(pcmIn), len(parsedPCM))
	}
	if duration < 0.99 || duration > 1.01 {
		t.Errorf("expected duration ~1.0, got %f", duration)
	}
}

func TestSilkRoundTrip(t *testing.T) {
	// 构造 1 秒 24kHz 单声道 PCM 并编码为 SILK，再解码回 WAV
	tmpDir := t.TempDir()
	wavFile := filepath.Join(tmpDir, "test.wav")
	silkFile := filepath.Join(tmpDir, "test.silk")
	outWavFile := filepath.Join(tmpDir, "out.wav")

	sampleRate := 24000
	numSamples := sampleRate
	pcmIn := make([]byte, numSamples*2)
	// 写入正弦波特征或简单波形
	for i := 0; i < numSamples; i++ {
		pcmIn[i*2] = byte(i % 128)
		pcmIn[i*2+1] = 0
	}

	wavBytes, err := WrapPCM16ToWAV(pcmIn, sampleRate, 1)
	if err != nil {
		t.Fatalf("WrapPCM16ToWAV failed: %v", err)
	}
	if err := os.WriteFile(wavFile, wavBytes, 0600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	ctx := context.Background()
	dur, err := WAVToTencentSilk(ctx, wavFile, silkFile)
	if err != nil {
		t.Fatalf("WAVToTencentSilk failed: %v", err)
	}
	if dur <= 0 {
		t.Errorf("expected positive duration, got %f", dur)
	}

	// 验证 silk 产物
	silkData, err := os.ReadFile(silkFile)
	if err != nil {
		t.Fatalf("read silk failed: %v", err)
	}
	if len(silkData) == 0 {
		t.Fatalf("silk file is empty")
	}
	if silkData[0] != 0x02 {
		t.Errorf("expected 0x02 Tencent prefix, got %x", silkData[0])
	}

	// 解码回 WAV
	decodedWav, err := TencentSilkToWAV(ctx, silkFile, outWavFile)
	if err != nil {
		t.Fatalf("TencentSilkToWAV failed: %v", err)
	}
	if decodedWav != outWavFile {
		t.Errorf("expected out path %s, got %s", outWavFile, decodedWav)
	}

	outWavData, err := os.ReadFile(outWavFile)
	if err != nil {
		t.Fatalf("read out wav failed: %v", err)
	}
	if len(outWavData) < 44 {
		t.Fatalf("decoded wav file is too short: %d", len(outWavData))
	}
}
