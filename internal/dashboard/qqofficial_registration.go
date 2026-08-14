// Package dashboard - QQ Official Bot QR login registration.
// Ported from astrbot/core/platform/sources/qqofficial/login_registration.py
package dashboard

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	qqOfficialDefaultBindHost       = "q.qq.com"
	qqOfficialDefaultQrPollInterval = 2
	qqOfficialDefaultAPITimeoutMS   = 10000

	qqOfficialBindStatusNone      = 0
	qqOfficialBindStatusPending   = 1
	qqOfficialBindStatusCompleted = 2
	qqOfficialBindStatusExpired   = 3
)

// qqOfficialRegistration handles the QQ official bot QR registration flow.
// action: "start" creates a bind task and returns the QR URL; "poll" checks status.
func (s *Server) qqOfficialRegistration(action string, platformConfig map[string]interface{}, taskID, bindKey string) (map[string]interface{}, error) {
	if action == "start" {
		return s.qqOfficialStart(platformConfig)
	}
	if action == "poll" {
		return s.qqOfficialPoll(platformConfig, taskID, bindKey)
	}
	return nil, fmt.Errorf("unsupported action: %s", action)
}

func qqOfficialBindHost(config map[string]interface{}) string {
	host, _ := config["qqofficial_bind_host"].(string)
	host = strings.TrimSpace(host)
	if host == "" {
		host = qqOfficialDefaultBindHost
	}
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")
	if host == "" {
		host = qqOfficialDefaultBindHost
	}
	return host
}

func qqOfficialConnectURL(taskID, host string) string {
	return "https://" + host + "/qqbot/openclaw/connect.html?task_id=" + url.QueryEscape(taskID) + "&_wv=2"
}

func qqOfficialPostJSON(ctx context.Context, apiURL string, payload map[string]interface{}, timeoutMS int) (map[string]interface{}, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if err := validateOutboundURL(apiURL); err != nil {
		return nil, err
	}
	client := newOutboundClient(time.Duration(timeoutMS) * time.Millisecond)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	dataBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var data map[string]interface{}
	if err := json.Unmarshal(dataBytes, &data); err != nil {
		return nil, fmt.Errorf("QQ 机器人绑定接口响应格式异常")
	}
	if retcode, ok := data["retcode"]; ok && retcode != nil {
		if rc, err := strconv.Atoi(fmt.Sprintf("%v", retcode)); err == nil && rc != 0 {
			msg, _ := data["msg"].(string)
			if msg == "" {
				msg, _ = data["message"].(string)
			}
			if msg == "" {
				msg = "QQ 机器人绑定接口返回失败"
			}
			return nil, fmt.Errorf("%s", msg)
		}
	}
	return data, nil
}

func qqOfficialGenerateBindKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

func qqOfficialDecryptSecret(encryptedSecret, bindKey string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(bindKey)
	if err != nil {
		return "", fmt.Errorf("QQ 机器人凭证解码失败")
	}
	raw, err := base64.StdEncoding.DecodeString(encryptedSecret)
	if err != nil {
		return "", fmt.Errorf("QQ 机器人凭证解码失败")
	}
	if len(key) != 32 || len(raw) <= 28 {
		return "", fmt.Errorf("QQ 机器人凭证密文格式异常 (key=%d raw=%d)", len(key), len(raw))
	}
	nonce := raw[:12]
	// Go's gcm.Open expects the ciphertext to include the trailing 16-byte GCM tag
	// (unlike Python's decrypt_and_verify which takes them separately).
	ciphertextWithTag := raw[12:]
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("QQ 机器人凭证解密失败")
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("QQ 机器人凭证解密失败")
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertextWithTag, nil)
	if err != nil {
		return "", fmt.Errorf("QQ 机器人凭证解密失败 (raw=%d err=%v)", len(raw), err)
	}
	return string(plaintext), nil
}

func qqOfficialLoginResult(data map[string]interface{}, bindKey string) map[string]interface{} {
	payload, _ := data["data"].(map[string]interface{})
	if payload == nil {
		payload = map[string]interface{}{}
	}
	rawStatus := qqOfficialBindStatusNone
	if v, ok := payload["status"]; ok {
		if n, err := strconv.Atoi(fmt.Sprintf("%v", v)); err == nil {
			rawStatus = n
		}
	}
	switch rawStatus {
	case qqOfficialBindStatusCompleted:
		appid, _ := payload["bot_appid"].(string)
		encryptedSecret, _ := payload["bot_encrypt_secret"].(string)
		appid = strings.TrimSpace(appid)
		encryptedSecret = strings.TrimSpace(encryptedSecret)
		if appid == "" || encryptedSecret == "" {
			return map[string]interface{}{
				"status":    "error",
				"qr_status": rawStatus,
				"message":   "扫码成功但未返回完整 QQ 机器人凭证",
			}
		}
		secret, err := qqOfficialDecryptSecret(encryptedSecret, bindKey)
		if err != nil {
			return map[string]interface{}{
				"status":    "error",
				"qr_status": rawStatus,
				"message":   err.Error(),
			}
		}
		return map[string]interface{}{
			"status":             "created",
			"qr_status":          rawStatus,
			"appid":              appid,
			"secret":             secret,
			"platform_id_suffix": "_" + appid,
		}
	case qqOfficialBindStatusExpired:
		return map[string]interface{}{
			"status":    "expired",
			"qr_status": rawStatus,
			"message":   "二维码已过期",
		}
	default:
		return map[string]interface{}{
			"status":    "pending",
			"qr_status": rawStatus,
		}
	}
}

func (s *Server) qqOfficialStart(platformConfig map[string]interface{}) (map[string]interface{}, error) {
	host := qqOfficialBindHost(platformConfig)
	timeoutMS := qqOfficialDefaultAPITimeoutMS
	if v, ok := platformConfig["qqofficial_api_timeout_ms"]; ok {
		if n, err := strconv.Atoi(fmt.Sprintf("%v", v)); err == nil && n >= 1000 {
			timeoutMS = n
		}
	}
	interval := qqOfficialDefaultQrPollInterval
	if v, ok := platformConfig["qqofficial_qr_poll_interval"]; ok {
		if n, err := strconv.Atoi(fmt.Sprintf("%v", v)); err == nil && n >= 1 {
			interval = n
		}
	}
	bindKey, err := qqOfficialGenerateBindKey()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()
	data, err := qqOfficialPostJSON(ctx, "https://"+host+"/lite/create_bind_task", map[string]interface{}{
		"key": bindKey,
	}, timeoutMS)
	if err != nil {
		return nil, err
	}
	payload, _ := data["data"].(map[string]interface{})
	if payload == nil {
		payload = map[string]interface{}{}
	}
	taskID, _ := payload["task_id"].(string)
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, fmt.Errorf("QQ 机器人绑定任务响应缺少 task_id")
	}
	qrcode := qqOfficialConnectURL(taskID, host)
	return map[string]interface{}{
		"status":             "pending",
		"registration_code":  taskID,
		"task_id":            taskID,
		"bind_key":           bindKey,
		"qrcode":             qrcode,
		"qrcode_img_content": qrcode,
		"interval":           interval,
	}, nil
}

func (s *Server) qqOfficialPoll(platformConfig map[string]interface{}, taskID, bindKey string) (map[string]interface{}, error) {
	taskID = strings.TrimSpace(taskID)
	bindKey = strings.TrimSpace(bindKey)
	if taskID == "" {
		return nil, fmt.Errorf("missing task_id")
	}
	if bindKey == "" {
		return nil, fmt.Errorf("missing bind_key")
	}
	host := qqOfficialBindHost(platformConfig)
	timeoutMS := qqOfficialDefaultAPITimeoutMS
	if v, ok := platformConfig["qqofficial_api_timeout_ms"]; ok {
		if n, err := strconv.Atoi(fmt.Sprintf("%v", v)); err == nil && n >= 1000 {
			timeoutMS = n
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMS)*time.Millisecond)
	defer cancel()
	data, err := qqOfficialPostJSON(ctx, "https://"+host+"/lite/poll_bind_result", map[string]interface{}{
		"task_id": taskID,
	}, timeoutMS)
	if err != nil {
		return nil, err
	}
	return qqOfficialLoginResult(data, bindKey), nil
}
