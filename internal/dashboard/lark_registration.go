// Package dashboard - Lark (飞书) one-click registration.
// Ported 1:1 from
// astrbot/core/platform/sources/lark/app_registration.py: OAuth device-code
// flow (POST {accounts}/oauth/v1/app/registration with action=begin/poll) to
// create a new Lark app by scanning a QR code, returning app_id/app_secret.
package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	larkDefaultFeishuOpenDomain = "https://open.feishu.cn"
	larkDefaultLarkOpenDomain   = "https://open.larksuite.com"
	larkAppRegistrationPath     = "/oauth/v1/app/registration"
)

// larkRegistrationEndpoints resolves the accounts/open base URLs (Python
// resolve_app_registration_endpoints).
type larkRegistrationEndpoints struct {
	accountsBase string
	openBase     string
	registration string
}

func resolveLarkRegistrationEndpoints(domain string) larkRegistrationEndpoints {
	normalized := strings.TrimRight(strings.TrimSpace(domain), "/")
	if normalized == "" || normalized == "feishu" || normalized == larkDefaultFeishuOpenDomain {
		return larkRegistrationEndpoints{
			accountsBase: "https://accounts.feishu.cn",
			openBase:     larkDefaultFeishuOpenDomain,
			registration: "https://accounts.feishu.cn" + larkAppRegistrationPath,
		}
	}
	if normalized == "lark" || normalized == larkDefaultLarkOpenDomain {
		return larkRegistrationEndpoints{
			accountsBase: "https://accounts.larksuite.com",
			openBase:     larkDefaultLarkOpenDomain,
			registration: "https://accounts.larksuite.com" + larkAppRegistrationPath,
		}
	}
	accountsBase := strings.Replace(normalized, "://open.", "://accounts.", 1)
	return larkRegistrationEndpoints{
		accountsBase: accountsBase,
		openBase:     normalized,
		registration: accountsBase + larkAppRegistrationPath,
	}
}

func larkRegStringField(data map[string]interface{}, key string) string {
	if v, ok := data[key].(string); ok {
		return v
	}
	return ""
}

func larkRegIntField(data map[string]interface{}, key string, def int) int {
	switch v := data[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return def
}

// larkRegistrationData unwraps the response "data" object (Python
// _registration_data).
func larkRegistrationData(raw map[string]interface{}) map[string]interface{} {
	if d, ok := raw["data"].(map[string]interface{}); ok {
		return d
	}
	return raw
}

// larkPostRegistration POSTs the device-code form (Python _post_registration).
func larkPostRegistration(ctx context.Context, endpoint string, form url.Values) (int, map[string]interface{}, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewBufferString(form.Encode()))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := validateOutboundURL(endpoint); err != nil {
		return 0, nil, err
	}
	resp, err := newOutboundClient(15 * time.Second).Do(req)
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
		return resp.StatusCode, nil, fmt.Errorf("飞书应用创建响应格式异常")
	}
	return resp.StatusCode, out, nil
}

// larkStart begins the app registration (Python request_app_registration).
func larkStart(domain string) (map[string]interface{}, error) {
	endpoints := resolveLarkRegistrationEndpoints(domain)
	form := url.Values{
		"action":            []string{"begin"},
		"archetype":         []string{"PersonalAgent"},
		"auth_method":       []string{"client_secret"},
		"request_user_info": []string{"open_id tenant_brand"},
	}
	status, raw, err := larkPostRegistration(context.Background(), endpoints.registration, form)
	if err != nil {
		return nil, err
	}
	if err := larkRaiseRegistrationError(status, raw, "发起扫码创建失败"); err != nil {
		return nil, err
	}
	data := larkRegistrationData(raw)
	userCode := larkRegStringField(data, "user_code")
	verificationURI := larkRegStringField(data, "verification_uri")
	verificationURIComplete := larkRegStringField(data, "verification_uri_complete")
	if verificationURIComplete == "" && userCode != "" {
		verificationURIComplete = fmt.Sprintf("%s/page/cli?%s", endpoints.openBase,
			url.Values{"user_code": []string{userCode}}.Encode())
	}
	expiresIn := larkRegIntField(data, "expires_in", 300)
	interval := larkRegIntField(data, "interval", 5)
	return map[string]interface{}{
		"status":                    "pending",
		"device_code":               larkRegStringField(data, "device_code"),
		"registration_code":         larkRegStringField(data, "device_code"),
		"user_code":                 userCode,
		"verification_uri":          verificationURI,
		"verification_uri_complete": verificationURIComplete,
		"expires_in":                expiresIn,
		"interval":                  interval,
	}, nil
}

// larkRaiseRegistrationError mirrors _raise_registration_error.
func larkRaiseRegistrationError(status int, raw map[string]interface{}, fallback string) error {
	data := larkRegistrationData(raw)
	if status < 400 && larkRegStringField(raw, "error") == "" && larkRegStringField(data, "error") == "" {
		return nil
	}
	message := larkRegStringField(raw, "error_description")
	if message == "" {
		message = larkRegStringField(data, "error_description")
	}
	if message == "" {
		message = larkRegStringField(raw, "error")
	}
	if message == "" {
		message = larkRegStringField(data, "error")
	}
	if message == "" {
		message = fallback
	}
	return fmt.Errorf("%s", message)
}

// larkTenantBrand reads the tenant_brand from user_info (Python _tenant_brand).
func larkTenantBrand(data map[string]interface{}) string {
	if ui, ok := data["user_info"].(map[string]interface{}); ok {
		if v := larkRegStringField(ui, "tenant_brand"); v != "" {
			return v
		}
	}
	return larkRegStringField(data, "tenant_brand")
}

// larkPoll polls the registration status (Python poll_app_registration_once).
func larkPoll(domain, deviceCode string) (map[string]interface{}, error) {
	deviceCode = strings.TrimSpace(deviceCode)
	if deviceCode == "" {
		return nil, fmt.Errorf("缺少 device_code")
	}
	endpoints := resolveLarkRegistrationEndpoints(domain)
	form := url.Values{
		"action":      []string{"poll"},
		"device_code": []string{deviceCode},
	}
	status, raw, err := larkPostRegistration(context.Background(), endpoints.registration, form)
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
			"status":       "created",
			"app_id":       clientID,
			"app_secret":   clientSecret,
			"tenant_brand": tenantBrand,
			"domain":       domainOut,
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
	message := larkRegStringField(raw, "error_description")
	if message == "" {
		message = larkRegStringField(data, "error_description")
	}
	if message == "" {
		message = errorField
	}
	if message == "" {
		message = "获取扫码创建状态失败"
	}
	return map[string]interface{}{"status": "error", "message": message}, nil
}

// larkPollSecret re-polls against the lark domain to fetch the client secret
// (Python _poll_lark_secret).
func larkPollSecret(deviceCode string) string {
	endpoints := resolveLarkRegistrationEndpoints(larkDefaultLarkOpenDomain)
	form := url.Values{
		"action":      []string{"poll"},
		"device_code": []string{deviceCode},
	}
	status, raw, err := larkPostRegistration(context.Background(), endpoints.registration, form)
	if err != nil || status >= 400 || larkRegStringField(raw, "error") != "" {
		return ""
	}
	return larkRegStringField(larkRegistrationData(raw), "client_secret")
}

// larkRegistration dispatches the one-click registration for lark.
func (s *Server) larkRegistration(action, domain, deviceCode string) (map[string]interface{}, error) {
	switch action {
	case "start":
		return larkStart(domain)
	case "poll":
		return larkPoll(domain, deviceCode)
	}
	return nil, fmt.Errorf("不支持的 action: %s", action)
}
