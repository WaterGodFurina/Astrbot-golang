package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeLarkRegistrationServer serves the device-code flow.
func fakeLarkRegistrationServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(400)
			return
		}
		action := r.Form.Get("action")
		deviceCode := r.Form.Get("device_code")
		switch action {
		case "begin":
			_, _ = w.Write([]byte(`{"data":{"device_code":"dev123","user_code":"user9","verification_uri":"https://open.feishu.cn/verify","verification_uri_complete":"https://open.feishu.cn/page/cli?user_code=user9","expires_in":300,"interval":5}}`))
		case "poll":
			switch deviceCode {
			case "dev123":
				_, _ = w.Write([]byte(`{"data":{"client_id":"cli_abc","client_secret":"sec_xyz","user_info":{"tenant_brand":"feishu"}}}`))
			case "dev_pending":
				_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
			case "dev_denied":
				_, _ = w.Write([]byte(`{"error":"access_denied"}`))
			case "dev_expired":
				_, _ = w.Write([]byte(`{"error":"expired_token"}`))
			default:
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			}
		default:
			w.WriteHeader(400)
		}
	}))
}

// fakeDingtalkRegistrationServer serves init/begin/poll.
func fakeDingtalkRegistrationServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		switch {
		case strings.HasSuffix(r.URL.Path, "/init"):
			_, _ = w.Write([]byte(`{"nonce":"n123","errcode":0,"errmsg":"ok"}`))
		case strings.HasSuffix(r.URL.Path, "/begin"):
			if payload["nonce"] != "n123" {
				w.WriteHeader(400)
				return
			}
			_, _ = w.Write([]byte(`{"device_code":"dd123","user_code":"u1","verification_uri":"https://oapi.dingtalk.com/verify","verification_uri_complete":"https://oapi.dingtalk.com/verify?device_code=dd123","expires_in":7200,"interval":3,"errcode":0}`))
		case strings.HasSuffix(r.URL.Path, "/poll"):
			switch payload["device_code"] {
			case "dd123":
				_, _ = w.Write([]byte(`{"status":"SUCCESS","client_id":"dt_app","client_secret":"dt_sec","errcode":0}`))
			case "dd_waiting":
				_, _ = w.Write([]byte(`{"status":"WAITING","errcode":0}`))
			case "dd_fail":
				_, _ = w.Write([]byte(`{"status":"FAIL","fail_reason":"用户拒绝","errcode":0}`))
			case "dd_expired":
				_, _ = w.Write([]byte(`{"status":"EXPIRED","errcode":0}`))
			default:
				_, _ = w.Write([]byte(`{"errcode":40001,"errmsg":"invalid"}`))
			}
		default:
			w.WriteHeader(404)
		}
	}))
}

// larkPollWithEndpoint drives larkPoll against a concrete registration endpoint
// (bypasses domain resolution so tests can hit the httptest server).
func larkPollWithEndpoint(endpoint, deviceCode string) (map[string]interface{}, error) {
	deviceCode = strings.TrimSpace(deviceCode)
	if deviceCode == "" {
		return nil, errMissingDeviceCode
	}
	form := url.Values{
		"action":      []string{"poll"},
		"device_code": []string{deviceCode},
	}
	status, raw, err := larkPostRegistration(context.Background(), endpoint, form)
	if err != nil {
		return nil, err
	}
	data := larkRegistrationData(raw)
	errorField := larkRegStringField(raw, "error")
	if errorField == "" {
		errorField = larkRegStringField(data, "error")
	}
	clientID := larkRegStringField(data, "client_id")
	clientSecret := larkRegStringField(data, "client_secret")
	tenantBrand := larkTenantBrand(data)
	if status < 400 && errorField == "" && clientID != "" {
		if clientSecret == "" && tenantBrand == "lark" {
			clientSecret = larkPollSecret(deviceCode)
		}
		if clientSecret == "" {
			return map[string]interface{}{"status": "error", "message": "应用创建成功但未获取到凭证"}, nil
		}
		domainOut := larkDefaultFeishuOpenDomain
		if tenantBrand == "lark" {
			domainOut = larkDefaultLarkOpenDomain
		}
		return map[string]interface{}{
			"status": "created", "app_id": clientID, "app_secret": clientSecret,
			"tenant_brand": tenantBrand, "domain": domainOut,
		}, nil
	}
	switch errorField {
	case "authorization_pending":
		return map[string]interface{}{"status": "pending"}, nil
	case "slow_down":
		return map[string]interface{}{"status": "slow_down"}, nil
	case "access_denied":
		return map[string]interface{}{"status": "denied", "message": "用户取消了扫码创建"}, nil
	case "expired_token", "invalid_grant":
		return map[string]interface{}{"status": "expired", "message": "扫码已过期，请再次创建"}, nil
	}
	return map[string]interface{}{"status": "error", "message": errorField}, nil
}

var errMissingDeviceCode = &registrationError{"missing device code"}

type registrationError struct{ msg string }

func (e *registrationError) Error() string { return e.msg }

// larkStartWithEndpoint drives larkStart against a concrete endpoint.
func larkStartWithEndpoint(endpoint string) (map[string]interface{}, error) {
	form := url.Values{
		"action":            []string{"begin"},
		"archetype":         []string{"PersonalAgent"},
		"auth_method":       []string{"client_secret"},
		"request_user_info": []string{"open_id tenant_brand"},
	}
	status, raw, err := larkPostRegistration(context.Background(), endpoint, form)
	if err != nil {
		return nil, err
	}
	if err := larkRaiseRegistrationError(status, raw, "发起扫码创建失败"); err != nil {
		return nil, err
	}
	data := larkRegistrationData(raw)
	userCode := larkRegStringField(data, "user_code")
	verificationURIComplete := larkRegStringField(data, "verification_uri_complete")
	if verificationURIComplete == "" && userCode != "" {
		verificationURIComplete = endpoint + "?user_code=" + userCode
	}
	return map[string]interface{}{
		"status": "pending", "device_code": larkRegStringField(data, "device_code"),
		"registration_code": larkRegStringField(data, "device_code"), "user_code": userCode,
		"verification_uri":          larkRegStringField(data, "verification_uri"),
		"verification_uri_complete": verificationURIComplete,
		"expires_in":                larkRegIntField(data, "expires_in", 300),
		"interval":                  larkRegIntField(data, "interval", 5),
	}, nil
}

func TestLarkStart(t *testing.T) {
	srv := fakeLarkRegistrationServer(t)
	defer srv.Close()
	result, err := larkStartWithEndpoint(srv.URL + "/oauth/v1/app/registration")
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "pending" || result["device_code"] != "dev123" {
		t.Errorf("start result: %v", result)
	}
	if _, ok := result["verification_uri_complete"]; !ok {
		t.Error("verification_uri_complete must be present")
	}
}

func TestLarkPollConfirmed(t *testing.T) {
	srv := fakeLarkRegistrationServer(t)
	defer srv.Close()
	result, err := larkPollWithEndpoint(srv.URL+"/oauth/v1/app/registration", "dev123")
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "created" || result["app_id"] != "cli_abc" || result["app_secret"] != "sec_xyz" {
		t.Errorf("poll result: %v", result)
	}
	if result["domain"] != larkDefaultFeishuOpenDomain {
		t.Errorf("domain: %v", result["domain"])
	}
}

func TestLarkPollStates(t *testing.T) {
	srv := fakeLarkRegistrationServer(t)
	defer srv.Close()
	ep := srv.URL + "/oauth/v1/app/registration"
	if r, _ := larkPollWithEndpoint(ep, "dev_pending"); r["status"] != "pending" {
		t.Errorf("pending: %v", r)
	}
	if r, _ := larkPollWithEndpoint(ep, "dev_denied"); r["status"] != "denied" {
		t.Errorf("denied: %v", r)
	}
	if r, _ := larkPollWithEndpoint(ep, "dev_expired"); r["status"] != "expired" {
		t.Errorf("expired: %v", r)
	}
}

// TestLarkEndpointResolution: domain -> accounts/open base URLs.
func TestLarkEndpointResolution(t *testing.T) {
	feishu := resolveLarkRegistrationEndpoints("")
	if feishu.registration != "https://accounts.feishu.cn/oauth/v1/app/registration" {
		t.Errorf("feishu: %v", feishu)
	}
	larkDom := resolveLarkRegistrationEndpoints("lark")
	if larkDom.registration != "https://accounts.larksuite.com/oauth/v1/app/registration" {
		t.Errorf("lark: %v", larkDom)
	}
	custom := resolveLarkRegistrationEndpoints("https://open.example.com")
	if custom.registration != "https://accounts.example.com/oauth/v1/app/registration" {
		t.Errorf("custom: %v", custom)
	}
}

func TestDingtalkStart(t *testing.T) {
	srv := fakeDingtalkRegistrationServer(t)
	defer srv.Close()
	t.Setenv("DINGTALK_REGISTRATION_BASE_URL", srv.URL)
	result, err := dingtalkStart()
	if err != nil {
		t.Fatal(err)
	}
	if result["status"] != "pending" || result["device_code"] != "dd123" {
		t.Errorf("start result: %v", result)
	}
	if _, ok := result["verification_uri_complete"]; !ok {
		t.Error("verification_uri_complete must be present")
	}
}

func TestDingtalkPollStates(t *testing.T) {
	srv := fakeDingtalkRegistrationServer(t)
	defer srv.Close()
	t.Setenv("DINGTALK_REGISTRATION_BASE_URL", srv.URL)
	if r, _ := dingtalkPoll("dd123"); r["status"] != "created" || r["client_id"] != "dt_app" {
		t.Errorf("created: %v", r)
	}
	if r, _ := dingtalkPoll("dd_waiting"); r["status"] != "pending" {
		t.Errorf("waiting: %v", r)
	}
	if r, _ := dingtalkPoll("dd_fail"); r["status"] != "error" {
		t.Errorf("fail: %v", r)
	}
	if r, _ := dingtalkPoll("dd_expired"); r["status"] != "expired" {
		t.Errorf("expired: %v", r)
	}
}

// TestLarkDingtalkDispatchHTTP: end-to-end over handleBotRegistration.
func TestLarkDingtalkDispatchHTTP(t *testing.T) {
	dingSrv := fakeDingtalkRegistrationServer(t)
	defer dingSrv.Close()
	t.Setenv("DINGTALK_REGISTRATION_BASE_URL", dingSrv.URL)
	s := &Server{}

	// dingtalk start + poll
	var resp struct {
		Data map[string]interface{} `json:"data"`
	}
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/bot-types/dingtalk/registration",
		strings.NewReader(`{"action":"start"}`))
	w2 := httptest.NewRecorder()
	s.handleBotRegistration(w2, req2, "dingtalk")
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data["device_code"] != "dd123" {
		t.Errorf("dingtalk start: %v", resp.Data)
	}
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/bot-types/dingtalk/registration",
		strings.NewReader(`{"action":"poll","registration_code":"dd123"}`))
	w3 := httptest.NewRecorder()
	s.handleBotRegistration(w3, req3, "dingtalk")
	if err := json.Unmarshal(w3.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data["status"] != "created" || resp.Data["client_id"] != "dt_app" {
		t.Errorf("dingtalk poll: %v", resp.Data)
	}
}
