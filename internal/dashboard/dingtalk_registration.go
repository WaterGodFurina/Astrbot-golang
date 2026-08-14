// Package dashboard - DingTalk (钉钉) one-click registration.
// Ported 1:1 from
// astrbot/core/platform/sources/dingtalk/app_registration.py: three-step
// device-code flow (app/registration/init -> begin -> poll) to create a
// DingTalk app by scanning, returning client_id/client_secret.
package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// getenv reads an environment variable (os.Getenv).
func getenv(key string) string {
	return os.Getenv(key)
}

const (
	dingtalkDefaultRegistrationBaseURL = "https://oapi.dingtalk.com"
	dingtalkDefaultRegistrationSource  = "DING_DWS_CLAW"
)

func dingtalkRegistrationBaseURL() string {
	base := strings.TrimSpace(getenv("DINGTALK_REGISTRATION_BASE_URL"))
	if base == "" {
		base = dingtalkDefaultRegistrationBaseURL
	}
	return strings.TrimRight(base, "/")
}

func dingtalkRegistrationSource() string {
	src := strings.TrimSpace(getenv("DINGTALK_REGISTRATION_SOURCE"))
	if src == "" {
		src = dingtalkDefaultRegistrationSource
	}
	return src
}

func dingtalkRegStringField(data map[string]interface{}, key string) string {
	if v, ok := data[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func dingtalkRegIntField(data map[string]interface{}, key string, def int) int {
	switch v := data[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return def
}

// dingtalkPostRegistration POSTs a JSON payload (Python _post_registration).
func dingtalkPostRegistration(ctx context.Context, path string, payload map[string]string) (int, map[string]interface{}, error) {
	raw, _ := json.Marshal(payload)
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost,
		dingtalkRegistrationBaseURL()+path, bytes.NewBuffer(raw))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	var out map[string]interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		return resp.StatusCode, nil, fmt.Errorf("DingTalk registration response format is invalid")
	}
	return resp.StatusCode, out, nil
}

// dingtalkRaiseRegistrationError mirrors _raise_dingtalk_registration_error.
func dingtalkRaiseRegistrationError(status int, raw map[string]interface{}, action string) error {
	errcode := 0
	switch v := raw["errcode"].(type) {
	case float64:
		errcode = int(v)
	case int:
		errcode = v
	}
	if status < 400 && errcode == 0 {
		return nil
	}
	errmsg := dingtalkRegStringField(raw, "errmsg")
	if errmsg == "" {
		errmsg = "unknown error"
	}
	return fmt.Errorf("[%s] %s (errcode=%d)", action, errmsg, errcode)
}

// dingtalkStart runs init -> begin and returns the device-code flow info
// (Python request_dingtalk_app_registration).
func dingtalkStart() (map[string]interface{}, error) {
	ctx := context.Background()
	// init
	status, initRaw, err := dingtalkPostRegistration(ctx, "/app/registration/init",
		map[string]string{"source": dingtalkRegistrationSource()})
	if err != nil {
		return nil, err
	}
	if err := dingtalkRaiseRegistrationError(status, initRaw, "init"); err != nil {
		return nil, err
	}
	nonce := dingtalkRegStringField(initRaw, "nonce")
	if nonce == "" {
		return nil, fmt.Errorf("[init] missing nonce")
	}
	// begin
	status, beginRaw, err := dingtalkPostRegistration(ctx, "/app/registration/begin",
		map[string]string{"nonce": nonce})
	if err != nil {
		return nil, err
	}
	if err := dingtalkRaiseRegistrationError(status, beginRaw, "begin"); err != nil {
		return nil, err
	}
	deviceCode := dingtalkRegStringField(beginRaw, "device_code")
	verificationURIComplete := dingtalkRegStringField(beginRaw, "verification_uri_complete")
	if deviceCode == "" {
		return nil, fmt.Errorf("[begin] missing device_code")
	}
	if verificationURIComplete == "" {
		return nil, fmt.Errorf("[begin] missing verification_uri_complete")
	}
	return map[string]interface{}{
		"status":                    "pending",
		"device_code":               deviceCode,
		"registration_code":         deviceCode,
		"user_code":                 dingtalkRegStringField(beginRaw, "user_code"),
		"verification_uri":          dingtalkRegStringField(beginRaw, "verification_uri"),
		"verification_uri_complete": verificationURIComplete,
		"expires_in":                dingtalkRegIntField(beginRaw, "expires_in", 7200),
		"interval":                  dingtalkRegIntField(beginRaw, "interval", 3),
	}, nil
}

// dingtalkPoll polls the registration status (Python poll_dingtalk_app_registration_once).
func dingtalkPoll(deviceCode string) (map[string]interface{}, error) {
	deviceCode = strings.TrimSpace(deviceCode)
	if deviceCode == "" {
		return nil, fmt.Errorf("缺少 device_code")
	}
	status, raw, err := dingtalkPostRegistration(context.Background(), "/app/registration/poll",
		map[string]string{"device_code": deviceCode})
	if err != nil {
		return nil, err
	}
	if err := dingtalkRaiseRegistrationError(status, raw, "poll"); err != nil {
		return nil, err
	}
	return dingtalkRegistrationPollResult(raw), nil
}

// dingtalkRegistrationPollResult mirrors dingtalk_registration_poll_result.
func dingtalkRegistrationPollResult(raw map[string]interface{}) map[string]interface{} {
	statusRaw := strings.ToUpper(dingtalkRegStringField(raw, "status"))
	switch statusRaw {
	case "WAITING":
		return map[string]interface{}{"status": "pending"}
	case "SUCCESS":
		clientID := dingtalkRegStringField(raw, "client_id")
		clientSecret := dingtalkRegStringField(raw, "client_secret")
		if clientID == "" || clientSecret == "" {
			return map[string]interface{}{"status": "error", "message": "扫码成功但未获取到钉钉应用凭证"}
		}
		return map[string]interface{}{
			"status":        "created",
			"client_id":     clientID,
			"client_secret": clientSecret,
		}
	case "FAIL":
		reason := dingtalkRegStringField(raw, "fail_reason")
		if reason == "" {
			reason = "钉钉扫码创建失败"
		}
		return map[string]interface{}{"status": "error", "message": reason}
	case "EXPIRED":
		return map[string]interface{}{"status": "expired", "message": "钉钉扫码已过期，请重新创建"}
	}
	return map[string]interface{}{
		"status":  "error",
		"message": fmt.Sprintf("钉钉扫码创建返回未知状态: %s", statusRaw),
	}
}

// dingtalkRegistration dispatches the one-click registration for dingtalk.
func (s *Server) dingtalkRegistration(action, deviceCode string) (map[string]interface{}, error) {
	switch action {
	case "start":
		return dingtalkStart()
	case "poll":
		return dingtalkPoll(deviceCode)
	}
	return nil, fmt.Errorf("不支持的 action: %s", action)
}
