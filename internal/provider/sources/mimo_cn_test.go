package sources

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AstrBotDevs/AstrBot/internal/provider"
)

func TestMimoCNCreateProvider(t *testing.T) {
	for _, typ := range []string{"volcengine_tts", "gemini_tts", "mimo_tts_api", "mimo_stt_api"} {
		p, err := provider.CreateProvider(typ, map[string]interface{}{
			"type": typ, "model": "m",
		}, map[string]interface{}{})
		if err != nil {
			t.Fatalf("create %s: %v", typ, err)
		}
		if p.Meta().Type != typ {
			t.Errorf("type mismatch: got %q want %q", p.Meta().Type, typ)
		}
	}
}

func TestMimoTTSApiSourceGetAudio(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	_ = os.Chdir(dir)
	t.Cleanup(func() { _ = os.Chdir(old) })

	var gotModel string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"audio":{"data":"`+
			base64.StdEncoding.EncodeToString([]byte("fakewavdata"))+`"}}}]}`)
	})

	src := NewMiMoTTSApiSource(map[string]interface{}{
		"api_base": srv.URL,
		"api_key":  "sk-test",
		"model":    "mimo-v2.5-tts",
	}, map[string]interface{}{})

	path, err := src.GetAudio(context.Background(), "你好")
	if err != nil {
		t.Fatalf("get audio: %v", err)
	}
	if path == "" {
		t.Fatalf("empty output path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(data) != "fakewavdata" {
		t.Errorf("unexpected audio content: %q", data)
	}
	if gotModel != "mimo-v2.5-tts" {
		t.Errorf("unexpected model sent: %q", gotModel)
	}
	if src.Meta().ProviderType != provider.CapTextToSpeech {
		t.Errorf("capability not set: %v", src.Meta().ProviderType)
	}
	_ = os.Remove(path)
}

func TestVolcengineTTSSourceGetAudio(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	_ = os.Chdir(dir)
	t.Cleanup(func() { _ = os.Chdir(old) })

	var gotAppID string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		app, _ := body["app"].(map[string]interface{})
		gotAppID, _ = app["appid"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":"`+
			base64.StdEncoding.EncodeToString([]byte("mp3data"))+`"}`)
	})

	src := NewVolcengineTTSSource(map[string]interface{}{
		"api_base":              srv.URL,
		"api_key":               "sk-test",
		"appid":                 "testappid",
		"volcengine_cluster":    "volcano_tts",
		"volcengine_voice_type": "BV001_streaming",
	}, map[string]interface{}{})

	path, err := src.GetAudio(context.Background(), "你好")
	if err != nil {
		t.Fatalf("get audio: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(data) != "mp3data" {
		t.Errorf("unexpected audio content: %q", data)
	}
	if gotAppID != "testappid" {
		t.Errorf("unexpected appid sent: %q", gotAppID)
	}
	if src.Meta().ProviderType != provider.CapTextToSpeech {
		t.Errorf("capability not set: %v", src.Meta().ProviderType)
	}
	_ = os.Remove(path)
}

func TestGeminiTTSSourceGetAudio(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	_ = os.Chdir(dir)
	t.Cleanup(func() { _ = os.Chdir(old) })

	var gotPath, gotVoice string
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		gc, _ := body["generationConfig"].(map[string]interface{})
		sc, _ := gc["speechConfig"].(map[string]interface{})
		vc, _ := sc["voiceConfig"].(map[string]interface{})
		pvc, _ := vc["prebuiltVoiceConfig"].(map[string]interface{})
		gotVoice, _ = pvc["voiceName"].(string)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"candidates":[{"content":{"parts":[{"inlineData":{"data":"`+
			base64.StdEncoding.EncodeToString([]byte("pcmdata"))+`"}}]}}]}`)
	})

	src := NewGeminiTTSSource(map[string]interface{}{
		"gemini_tts_api_base": srv.URL,
		"gemini_tts_api_key":  "sk-test",
	}, map[string]interface{}{})

	path, err := src.GetAudio(context.Background(), "你好")
	if err != nil {
		t.Fatalf("get audio: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.HasPrefix(string(data), "RIFF") || !strings.Contains(string(data), "WAVE") {
		t.Errorf("unexpected wav content: %q", data[:16])
	}
	if !strings.HasSuffix(path, ".wav") {
		t.Errorf("expected .wav output, got %q", path)
	}
	if !strings.HasSuffix(gotPath, ":generateContent") {
		t.Errorf("unexpected API path: %q", gotPath)
	}
	if gotVoice != "Leda" {
		t.Errorf("unexpected voice name: %q", gotVoice)
	}
	if src.Meta().ProviderType != provider.CapTextToSpeech {
		t.Errorf("capability not set: %v", src.Meta().ProviderType)
	}
	_ = os.Remove(path)
}

func TestMimoSTTApiSourceGetText(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	_ = os.Chdir(dir)
	t.Cleanup(func() { _ = os.Chdir(old) })

	// Fake wav with a valid RIFF/WAVE header (required by the provider).
	wav := []byte("RIFF\x24\x00\x00\x00WAVEfmt data")
	audio := filepath.Join(dir, "voice.wav")
	if err := os.WriteFile(audio, wav, 0644); err != nil {
		t.Fatal(err)
	}

	var gotMessages int
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		messages, _ := body["messages"].([]interface{})
		gotMessages = len(messages)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":" 你好世界 "}}]}`)
	})

	src := NewMiMoSTTApiSource(map[string]interface{}{
		"api_base": srv.URL,
		"api_key":  "sk-test",
		"model":    "mimo-v2.5-asr",
	}, map[string]interface{}{})

	text, err := src.GetText(context.Background(), audio)
	if err != nil {
		t.Fatalf("get text: %v", err)
	}
	if text != "你好世界" {
		t.Errorf("unexpected transcript: %q", text)
	}
	if gotMessages != 1 {
		t.Errorf("ASR model should send a single user message, got %d", gotMessages)
	}
	if src.Meta().ProviderType != provider.CapSpeechToText {
		t.Errorf("capability not set: %v", src.Meta().ProviderType)
	}
}

func TestMimoSTTApiSourceRemoteURL(t *testing.T) {
	dir := t.TempDir()
	old, _ := os.Getwd()
	_ = os.Chdir(dir)
	t.Cleanup(func() { _ = os.Chdir(old) })

	wav := []byte("RIFF\x24\x00\x00\x00WAVEfmt data")
	audioSrv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(wav)
	})
	transcribeSrv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"hi"}}]}`)
	})

	src := NewMiMoSTTApiSource(map[string]interface{}{
		"api_base": transcribeSrv.URL,
		"api_key":  "sk-test",
		"model":    "mimo-v2.5-asr",
	}, map[string]interface{}{})

	text, err := src.GetText(context.Background(), audioSrv.URL+"/audio.wav")
	if err != nil {
		t.Fatalf("get text from url: %v", err)
	}
	if text != "hi" {
		t.Errorf("unexpected transcript: %q", text)
	}
}
