package sources

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/gorilla/websocket"
)

func chdirTemp(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	old, _ := os.Getwd()
	_ = os.Chdir(dir)
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func TestTTSApiCreateProvider(t *testing.T) {
	for _, typ := range []string{"azure_tts", "elevenlabs_tts_api", "fishaudio_tts_api", "minimax_tts_api", "edge_tts"} {
		p, err := provider.CreateProvider(typ, map[string]interface{}{
			"type": typ, "model": "m",
		}, map[string]interface{}{})
		if err != nil {
			t.Fatalf("create %s: %v", typ, err)
		}
		if p.Meta().Type != typ {
			t.Errorf("type mismatch: got %q want %q", p.Meta().Type, typ)
		}
		if p.Meta().ProviderType != provider.CapTextToSpeech {
			t.Errorf("capability not set for %s: %v", typ, p.Meta().ProviderType)
		}
	}
}

func TestAzureTTSNativeGetAudio(t *testing.T) {
	chdirTemp(t)
	var gotToken, gotSSML bool
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			if r.Header.Get("Ocp-Apim-Subscription-Key") == "" {
				t.Errorf("missing subscription key header")
			}
			gotToken = true
			_, _ = io.WriteString(w, "faketoken123")
		case "/synthesize":
			if r.Header.Get("Authorization") != "Bearer faketoken123" {
				t.Errorf("unexpected auth: %q", r.Header.Get("Authorization"))
			}
			if r.Header.Get("Content-Type") != "application/ssml+xml" {
				t.Errorf("unexpected content type: %q", r.Header.Get("Content-Type"))
			}
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "zh-CN-YunxiaNeural") {
				t.Errorf("ssml missing voice")
			}
			gotSSML = true
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write([]byte("RIFFazurefake"))
		default:
			http.NotFound(w, r)
		}
	})

	src := NewAzureTTSSource(map[string]interface{}{
		"azure_tts_subscription_key": "0123456789abcdef0123456789abcdef",
		"model":                      "azure_tts",
	}, map[string]interface{}{})
	src.endpoint = srv.URL + "/synthesize"
	src.tokenURL = srv.URL + "/token"

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
	if !bytes.HasPrefix(data, []byte("RIFF")) {
		t.Errorf("unexpected audio content: %q", data)
	}
	if !gotSSML || !gotToken {
		t.Errorf("expected token + synthesize calls, token=%v ssml=%v", gotToken, gotSSML)
	}
	_ = os.Remove(path)
	if src.SupportStream() {
		t.Errorf("expected no streaming support")
	}
}

// TestAzureTTSNativeConcurrentGetAudio runs concurrent synthesis so the
// token/timeOffset shared state fields are exercised under the race detector
// (M-40b).
func TestAzureTTSNativeConcurrentGetAudio(t *testing.T) {
	chdirTemp(t)
	var tokenCalls, synthCalls int32
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			atomic.AddInt32(&tokenCalls, 1)
			_, _ = io.WriteString(w, "faketoken-"+r.URL.Query().Get("n"))
		case "/synthesize":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer faketoken-") {
				t.Errorf("unexpected auth: %q", r.Header.Get("Authorization"))
			}
			atomic.AddInt32(&synthCalls, 1)
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write([]byte("RIFFconcurrent"))
		default:
			http.NotFound(w, r)
		}
	})

	src := NewAzureTTSSource(map[string]interface{}{
		"azure_tts_subscription_key": "0123456789abcdef0123456789abcdef",
	}, map[string]interface{}{})
	src.endpoint = srv.URL + "/synthesize"
	src.tokenURL = srv.URL + "/token?n=x"

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path, err := src.GetAudio(context.Background(), "并发测试")
			if err != nil {
				t.Errorf("GetAudio: %v", err)
				return
			}
			if data, err := os.ReadFile(path); err != nil || !bytes.HasPrefix(data, []byte("RIFF")) {
				t.Errorf("unexpected audio output: %q err=%v", data, err)
			}
			_ = os.Remove(path)
		}()
	}
	wg.Wait()
	if atomic.LoadInt32(&synthCalls) == 0 {
		t.Errorf("expected at least one synthesize call")
	}
	if atomic.LoadInt32(&tokenCalls) == 0 {
		t.Errorf("expected at least one token refresh")
	}
}

func TestAzureTTSOTTSGetAudio(t *testing.T) {
	chdirTemp(t)
	ottsSrv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/time":
			_, _ = io.WriteString(w, `{"timestamp":1700000000}`)
		case "/otts":
			if r.URL.Query().Get("sign") == "" {
				t.Errorf("missing sign query param")
			}
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse form: %v", err)
			}
			if r.Form.Get("text") != "你好" || r.Form.Get("voice") != "zh-CN-YunxiaNeural" {
				t.Errorf("unexpected form values: %+v", r.Form)
			}
			w.Header().Set("Content-Type", "audio/wav")
			_, _ = w.Write([]byte("RIFFottsfake"))
		default:
			http.NotFound(w, r)
		}
	})

	ottsJSON := fmt.Sprintf(`{"OTTS_SKEY":"skey","OTTS_URL":"%s/otts","OTTS_AUTH_TIME":"%s/time"}`, ottsSrv.URL, ottsSrv.URL)
	src := NewAzureTTSSource(map[string]interface{}{
		"azure_tts_subscription_key": "other[" + ottsJSON + "]",
	}, map[string]interface{}{})

	if err := src.Test(context.Background()); err != nil {
		t.Fatalf("config invalid: %v", err)
	}
	path, err := src.GetAudio(context.Background(), "你好")
	if err != nil {
		t.Fatalf("get audio: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.HasPrefix(data, []byte("RIFF")) {
		t.Errorf("unexpected audio content: %q", data)
	}
	_ = os.Remove(path)
}

func TestElevenLabsTTSGetAudio(t *testing.T) {
	chdirTemp(t)
	var gotBody map[string]interface{}
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/text-to-speech/test-voice" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("output_format") != "mp3_44100_128" {
			t.Errorf("unexpected output_format: %q", r.URL.Query().Get("output_format"))
		}
		if r.Header.Get("xi-api-key") != "sk-test" {
			t.Errorf("unexpected api key header")
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write([]byte("MP3ElevenFake"))
	})

	src := NewElevenLabsTTSSource(map[string]interface{}{
		"api_base":                 srv.URL,
		"api_key":                  "sk-test",
		"elevenlabs-tts-voice-id":  "test-voice",
		"elevenlabs-tts-stability": 0.5,
	}, map[string]interface{}{})

	path, err := src.GetAudio(context.Background(), "hello")
	if err != nil {
		t.Fatalf("get audio: %v", err)
	}
	if gotBody["text"] != "hello" || gotBody["model_id"] != "eleven_multilingual_v2" {
		t.Errorf("unexpected request body: %+v", gotBody)
	}
	vs, _ := gotBody["voice_settings"].(map[string]interface{})
	if vs == nil || vs["stability"] != 0.5 {
		t.Errorf("voice_settings not sent: %+v", gotBody["voice_settings"])
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.HasPrefix(string(data), "MP3") {
		t.Errorf("unexpected audio content: %q", data)
	}
	_ = os.Remove(path)
	if src.SupportStream() {
		t.Errorf("expected no streaming support")
	}
}

func TestFishAudioTTSGetAudio(t *testing.T) {
	chdirTemp(t)
	var contentType string
	var body []byte
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tts" {
			http.NotFound(w, r)
			return
		}
		contentType = r.Header.Get("Content-Type")
		body, _ = io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("unexpected auth header")
		}
		if r.Header.Get("model") != "s2-pro" {
			t.Errorf("model must be sent as header, got %q", r.Header.Get("model"))
		}
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write([]byte("WAVFishFake"))
	})

	src := NewFishAudioTTSSource(map[string]interface{}{
		"api_base":                   srv.URL,
		"api_key":                    "sk-test",
		"fishaudio-tts-reference-id": "626bb6d3f3364c9cbc3aa6a67300a664",
		"model":                      "s2-pro",
	}, map[string]interface{}{})

	path, err := src.GetAudio(context.Background(), "你好")
	if err != nil {
		t.Fatalf("get audio: %v", err)
	}
	if contentType != "application/msgpack" {
		t.Errorf("unexpected content type: %q", contentType)
	}
	if len(body) == 0 || body[0] != 0x88 {
		t.Errorf("expected 8-entry msgpack map, got prefix %v", body[:min(len(body), 4)])
	}
	if !bytes.Contains(body, []byte("wav")) || !bytes.Contains(body, []byte("626bb6d3f3364c9cbc3aa6a67300a664")) {
		t.Errorf("msgpack body missing expected fields: %q", body)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.HasPrefix(string(data), "WAV") {
		t.Errorf("unexpected audio content: %q", data)
	}
	_ = os.Remove(path)
	if src.SupportStream() {
		t.Errorf("expected no streaming support")
	}
}

func TestMiniMaxTTSGetAudio(t *testing.T) {
	chdirTemp(t)
	var gotBody map[string]interface{}
	srv := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("GroupId") != "g123" {
			t.Errorf("unexpected GroupId: %q", r.URL.Query().Get("GroupId"))
		}
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("unexpected auth header")
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"data\":{\"audio\":\"aa11\",\"status\":1}}\n\n"+
			"data: {\"data\":{\"audio\":\"bb22\",\"status\":2}}\n\n"+
			"data: {\"extra_info\":{\"x\":1}}\n\n"+
			"data: {\"data\":{\"audio\":\"cc\",\"status\":3}}\n\n")
	})

	src := NewMiniMaxTTSSource(map[string]interface{}{
		"api_base":         srv.URL,
		"api_key":          "sk-test",
		"minimax-group-id": "g123",
		"model":            "speech-02-hd",
	}, map[string]interface{}{})

	path, err := src.GetAudio(context.Background(), "你好")
	if err != nil {
		t.Fatalf("get audio: %v", err)
	}
	if gotBody["model"] != "speech-02-hd" || gotBody["stream"] != true {
		t.Errorf("unexpected request body: %+v", gotBody)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	want := []byte{0xaa, 0x11, 0xbb, 0x22, 0xcc}
	if !bytes.Equal(data, want) {
		t.Errorf("unexpected audio content: %v", data)
	}
	_ = os.Remove(path)
	if src.SupportStream() {
		t.Errorf("expected no streaming support")
	}
}

func TestEdgeTTSGetAudio(t *testing.T) {
	chdirTemp(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("read speech.config: %v", err)
			return
		}
		_, ssmlMsg, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read ssml: %v", err)
			return
		}
		if !strings.Contains(string(ssmlMsg), "Path:ssml") || !strings.Contains(string(ssmlMsg), "zh-CN-XiaoxiaoNeural") {
			t.Errorf("unexpected ssml: %s", ssmlMsg)
		}
		header := "X-RequestId:test\r\nContent-Type:audio/mpeg\r\nPath:audio\r\n\r\n"
		audio := []byte("EDGEAUDIO")
		frame := make([]byte, 0, 2+len(header)+len(audio))
		frame = append(frame, byte(len(header)>>8), byte(len(header)))
		frame = append(frame, header...)
		frame = append(frame, audio...)
		if err := conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			t.Errorf("write audio: %v", err)
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte("X-RequestId:test\r\nContent-Type:application/json; charset=utf-8\r\nPath:turn.end\r\n\r\n{\"type\":\"turn.end\"}"))
	}))
	t.Cleanup(srv.Close)

	src := NewEdgeTTSSource(map[string]interface{}{
		"edge-tts-ws-url": strings.Replace(srv.URL, "http", "ws", 1),
	}, map[string]interface{}{})

	path, err := src.GetAudio(context.Background(), "你好")
	if err != nil {
		t.Fatalf("get audio: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !bytes.Equal(data, []byte("EDGEAUDIO")) {
		t.Errorf("unexpected audio content: %q", data)
	}
	_ = os.Remove(path)
	if src.SupportStream() {
		t.Errorf("expected no streaming support")
	}
}
