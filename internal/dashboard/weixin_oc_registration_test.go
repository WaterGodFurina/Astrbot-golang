package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeWeixinOCServer serves get_bot_qrcode / get_qrcode_status.
func fakeWeixinOCServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "get_bot_qrcode"):
			_, _ = w.Write([]byte(`{"qrcode":"qr_abc123","qrcode_img_content":"data:image/png;base64,AAA"}`))
		case strings.HasSuffix(r.URL.Path, "get_qrcode_status"):
			status := r.URL.Query().Get("qrcode")
			switch status {
			case "qr_abc123":
				_, _ = w.Write([]byte(`{"status":"confirmed","bot_token":"tok123","ilink_bot_id":"bot_1","ilink_user_id":"u_1"}`))
			case "qr_expired":
				_, _ = w.Write([]byte(`{"status":"expired"}`))
			case "qr_cancel":
				_, _ = w.Write([]byte(`{"status":"canceled"}`))
			default:
				_, _ = w.Write([]byte(`{"status":"wait"}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestWeixinOCStart(t *testing.T) {
	srv := fakeWeixinOCServer(t)
	defer srv.Close()
	cfg := map[string]interface{}{"weixin_oc_base_url": srv.URL, "weixin_oc_bot_type": "3"}
	result, err := weixinOCStart(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "pending" || result["qrcode"] != "qr_abc123" {
		t.Errorf("start result: %v", result)
	}
	if _, ok := result["qrcode_img_content"]; !ok {
		t.Error("qrcode_img_content must be present")
	}
}

func TestWeixinOCPollConfirmed(t *testing.T) {
	srv := fakeWeixinOCServer(t)
	defer srv.Close()
	cfg := map[string]interface{}{"weixin_oc_base_url": srv.URL}
	result, err := weixinOCPoll(cfg, "qr_abc123")
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "created" {
		t.Fatalf("poll result: %v", result)
	}
	if result["weixin_oc_token"] != "tok123" || result["weixin_oc_account_id"] != "bot_1" {
		t.Errorf("token/account: %v", result)
	}
	if _, ok := result["platform_id_suffix"]; !ok {
		t.Error("platform_id_suffix must be present")
	}
}

func TestWeixinOCPollExpiredAndCancel(t *testing.T) {
	srv := fakeWeixinOCServer(t)
	defer srv.Close()
	cfg := map[string]interface{}{"weixin_oc_base_url": srv.URL}
	if r, _ := weixinOCPoll(cfg, "qr_expired"); r["status"] != "expired" {
		t.Errorf("expired: %v", r)
	}
	if r, _ := weixinOCPoll(cfg, "qr_cancel"); r["status"] != "denied" {
		t.Errorf("canceled: %v", r)
	}
}

func TestWeixinOCPollMissingQR(t *testing.T) {
	if _, err := weixinOCPoll(map[string]interface{}{}, "  "); err == nil {
		t.Error("missing qrcode must error")
	}
}

func TestWeixinOCRegistrationDispatch(t *testing.T) {
	srv := fakeWeixinOCServer(t)
	defer srv.Close()
	s := &Server{}
	cfg := map[string]interface{}{"weixin_oc_base_url": srv.URL}
	// start
	r, err := s.weixinOCRegistration("start", cfg, "")
	if err != nil || r["qrcode"] != "qr_abc123" {
		t.Errorf("dispatch start: %v %v", r, err)
	}
	// poll
	r, err = s.weixinOCRegistration("poll", cfg, "qr_abc123")
	if err != nil || r["status"] != "created" {
		t.Errorf("dispatch poll: %v %v", r, err)
	}
	// bad action
	if _, err := s.weixinOCRegistration("bogus", cfg, ""); err == nil {
		t.Error("bogus action must error")
	}
}

// TestWeixinOCRegistrationHTTP: end-to-end over the real handler.
func TestWeixinOCRegistrationHTTP(t *testing.T) {
	srv := fakeWeixinOCServer(t)
	defer srv.Close()
	s := &Server{}

	// start
	startBody := `{"action":"start","platform_config":{"weixin_oc_base_url":"` + srv.URL + `"}}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/bot-types/weixin_oc/registration", strings.NewReader(startBody))
	w := httptest.NewRecorder()
	s.handleBotRegistration(w, req, "weixin_oc")
	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data["qrcode"] != "qr_abc123" {
		t.Errorf("http start: %v", resp.Data)
	}

	// poll
	pollBody := `{"action":"poll","platform_config":{"weixin_oc_base_url":"` + srv.URL + `"},"registration_code":"qr_abc123"}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/bot-types/weixin_oc/registration", strings.NewReader(pollBody))
	w2 := httptest.NewRecorder()
	s.handleBotRegistration(w2, req2, "weixin_oc")
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data["status"] != "created" || resp.Data["weixin_oc_token"] != "tok123" {
		t.Errorf("http poll: %v", resp.Data)
	}
}
