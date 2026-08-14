// 企业微信（WeCom）REST API 客户端。
// 1:1 移植自 wechatpy.enterprise.WeChatClient 及其 kf / kf_message / message / media 子 API：
//   - gettoken 获取 access_token 并缓存；
//   - message/send 发送应用消息；
//   - media/upload、media/get 上传/下载素材；
//   - kf/sync_msg、kf/account/list、kf/add_contact_way、kf/send_msg 微信客服接口。
package wecom

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// WeChatClientError 企业微信 API 错误（对应 wechatpy 的 WeChatClientException，
// 带 errcode / errmsg 属性，如 40096 表示无效的 external_userid）。
type WeChatClientError struct {
	ErrCode int
	ErrMsg  string
}

func (e *WeChatClientError) Error() string {
	return fmt.Sprintf("企业微信 API 错误: errcode=%d errmsg=%s", e.ErrCode, e.ErrMsg)
}

// WeChatClient 企业微信 API 客户端（corpid + secret）。
type WeChatClient struct {
	corpID   string
	secret   string
	apiBase  string // 形如 https://qyapi.weixin.qq.com/cgi-bin/
	http     *http.Client

	mu          sync.Mutex
	accessToken string
	tokenExpire time.Time
}

// NewWeChatClient 构造企业微信 API 客户端。
func NewWeChatClient(corpID, secret, apiBase string) *WeChatClient {
	if !strings.HasSuffix(apiBase, "/") {
		apiBase += "/"
	}
	return &WeChatClient{
		corpID:  corpID,
		secret:  secret,
		apiBase: apiBase,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// GetAccessToken 获取 access_token（带缓存，提前 5 分钟过期）。
// 对应 wechatpy 的 client.get_access_token。
func (c *WeChatClient) GetAccessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.accessToken != "" && time.Now().Before(c.tokenExpire) {
		return c.accessToken, nil
	}
	query := url.Values{}
	query.Set("corpid", c.corpID)
	query.Set("corpsecret", c.secret)
	data, err := c.request(ctx, http.MethodGet, "gettoken", query, nil)
	if err != nil {
		return "", err
	}
	token, _ := data["access_token"].(string)
	if token == "" {
		return "", fmt.Errorf("gettoken 响应缺少 access_token: %v", data)
	}
	expiresIn := 7200
	if v, ok := data["expires_in"].(float64); ok && int(v) > 0 {
		expiresIn = int(v)
	}
	c.accessToken = token
	c.tokenExpire = time.Now().Add(time.Duration(expiresIn-300) * time.Second)
	return token, nil
}

// request 发起企业微信 API 请求并解析 JSON 响应（errcode != 0 时返回 WeChatClientError）。
func (c *WeChatClient) request(ctx context.Context, method, apiPath string, query url.Values, body interface{}) (map[string]interface{}, error) {
	if query == nil {
		query = url.Values{}
	}
	// gettoken 接口本身用于获取凭证，不携带 access_token
	if apiPath != "gettoken" {
		token, err := c.GetAccessToken(ctx)
		if err != nil {
			return nil, err
		}
		query.Set("access_token", token)
	}
	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+apiPath+"?"+query.Encode(), bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("企业微信 API 请求失败: HTTP %d, body=%s", resp.StatusCode, string(raw))
	}
	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("解析企业微信响应失败: %w, body=%s", err, string(raw))
	}
	if errCode, ok := data["errcode"].(float64); ok && int(errCode) != 0 {
		errMsg, _ := data["errmsg"].(string)
		return nil, &WeChatClientError{ErrCode: int(errCode), ErrMsg: errMsg}
	}
	return data, nil
}

// SendMessage 发送企业微信应用消息（对应 wechatpy client.message.send）。
func (c *WeChatClient) SendMessage(ctx context.Context, agentID, touser, msgType string, payload map[string]interface{}) error {
	msg := map[string]interface{}{
		"touser":  touser,
		"msgtype": msgType,
		"agentid": agentID,
		"safe":    0,
	}
	for k, v := range payload {
		msg[k] = v
	}
	_, err := c.request(ctx, http.MethodPost, "message/send", nil, msg)
	return err
}

// SendText 发送应用文本消息。
func (c *WeChatClient) SendText(ctx context.Context, agentID, touser, content string) error {
	return c.SendMessage(ctx, agentID, touser, "text", map[string]interface{}{
		"text": map[string]interface{}{"content": content},
	})
}

// SendImage 发送应用图片消息。
func (c *WeChatClient) SendImage(ctx context.Context, agentID, touser, mediaID string) error {
	return c.SendMessage(ctx, agentID, touser, "image", map[string]interface{}{
		"image": map[string]interface{}{"media_id": mediaID},
	})
}

// SendVoice 发送应用语音消息。
func (c *WeChatClient) SendVoice(ctx context.Context, agentID, touser, mediaID string) error {
	return c.SendMessage(ctx, agentID, touser, "voice", map[string]interface{}{
		"voice": map[string]interface{}{"media_id": mediaID},
	})
}

// SendVideo 发送应用视频消息。
func (c *WeChatClient) SendVideo(ctx context.Context, agentID, touser, mediaID string) error {
	return c.SendMessage(ctx, agentID, touser, "video", map[string]interface{}{
		"video": map[string]interface{}{"media_id": mediaID},
	})
}

// SendFile 发送应用文件消息。
func (c *WeChatClient) SendFile(ctx context.Context, agentID, touser, mediaID string) error {
	return c.SendMessage(ctx, agentID, touser, "file", map[string]interface{}{
		"file": map[string]interface{}{"media_id": mediaID},
	})
}

// UploadMedia 上传临时素材（对应 wechatpy client.media.upload）。
// mediaType: image / voice / video / file。
func (c *WeChatClient) UploadMedia(ctx context.Context, mediaType, filePath string) (string, error) {
	query := url.Values{}
	query.Set("access_token", "")
	query.Set("type", mediaType)
	token, err := c.GetAccessToken(ctx)
	if err != nil {
		return "", err
	}
	query.Set("access_token", token)

	f, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	part, err := writer.CreateFormFile("media", filepath.Base(filePath))
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase+"media/upload?"+query.Encode(), &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	var data map[string]interface{}
	if err := json.Unmarshal(raw, &data); err != nil {
		return "", fmt.Errorf("解析素材上传响应失败: %w", err)
	}
	if errCode, ok := data["errcode"].(float64); ok && int(errCode) != 0 {
		errMsg, _ := data["errmsg"].(string)
		return "", &WeChatClientError{ErrCode: int(errCode), ErrMsg: errMsg}
	}
	mediaID, _ := data["media_id"].(string)
	if mediaID == "" {
		return "", fmt.Errorf("素材上传响应缺少 media_id: %v", data)
	}
	return mediaID, nil
}

// DownloadMedia 下载临时素材（对应 wechatpy client.media.download）。
// 返回素材内容与响应头（Content-Disposition 中可能包含文件名）。
func (c *WeChatClient) DownloadMedia(ctx context.Context, mediaID string) ([]byte, http.Header, error) {
	token, err := c.GetAccessToken(ctx)
	if err != nil {
		return nil, nil, err
	}
	query := url.Values{}
	query.Set("access_token", token)
	query.Set("media_id", mediaID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiBase+"media/get?"+query.Encode(), nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, nil, err
	}
	// 出错时响应体是 JSON
	if resp.StatusCode != http.StatusOK {
		var data map[string]interface{}
		if json.Unmarshal(raw, &data) == nil {
			if errCode, ok := data["errcode"].(float64); ok && int(errCode) != 0 {
				errMsg, _ := data["errmsg"].(string)
				return nil, nil, &WeChatClientError{ErrCode: int(errCode), ErrMsg: errMsg}
			}
		}
		return nil, nil, fmt.Errorf("素材下载失败: HTTP %d", resp.StatusCode)
	}
	return raw, resp.Header, nil
}

// KFSyncMsg 获取微信客服消息（对应 WeChatKF.sync_msg）。
func (c *WeChatClient) KFSyncMsg(ctx context.Context, token, openKFID, cursor string, limit int) (map[string]interface{}, error) {
	if limit <= 0 {
		limit = 1000
	}
	body := map[string]interface{}{
		"token":     token,
		"cursor":    cursor,
		"limit":     limit,
		"open_kfid": openKFID,
	}
	return c.request(ctx, http.MethodPost, "kf/sync_msg", nil, body)
}

// KFGetAccountList 获取客服帐号列表（对应 WeChatKF.get_account_list）。
func (c *WeChatClient) KFGetAccountList(ctx context.Context) (map[string]interface{}, error) {
	return c.request(ctx, http.MethodGet, "kf/account/list", nil, nil)
}

// KFAddContactWay 获取客服帐号链接（对应 WeChatKF.add_contact_way）。
func (c *WeChatClient) KFAddContactWay(ctx context.Context, openKFID, scene string) (string, error) {
	data, err := c.request(ctx, http.MethodPost, "kf/add_contact_way", nil, map[string]interface{}{
		"open_kfid": openKFID,
		"scene":     scene,
	})
	if err != nil {
		return "", err
	}
	link, _ := data["url"].(string)
	return link, nil
}

// KFSend 发送微信客服消息（对应 WeChatKFMessage.send）。
// payload 示例：{"msgtype": "text", "text": {"content": "..."}}。
func (c *WeChatClient) KFSend(ctx context.Context, touser, openKFID, msgID string, payload map[string]interface{}) error {
	body := map[string]interface{}{
		"touser":   touser,
		"open_kfid": openKFID,
	}
	if msgID != "" {
		body["msgid"] = msgID
	}
	for k, v := range payload {
		body[k] = v
	}
	_, err := c.request(ctx, http.MethodPost, "kf/send_msg", nil, body)
	return err
}

// KFSendText 发送微信客服文本消息。
func (c *WeChatClient) KFSendText(ctx context.Context, touser, openKFID, content string) error {
	return c.KFSend(ctx, touser, openKFID, "", map[string]interface{}{
		"msgtype": "text",
		"text":    map[string]interface{}{"content": content},
	})
}

// KFSendImage 发送微信客服图片消息。
func (c *WeChatClient) KFSendImage(ctx context.Context, touser, openKFID, mediaID string) error {
	return c.KFSend(ctx, touser, openKFID, "", map[string]interface{}{
		"msgtype": "image",
		"image":   map[string]interface{}{"media_id": mediaID},
	})
}

// KFSendVoice 发送微信客服语音消息。
func (c *WeChatClient) KFSendVoice(ctx context.Context, touser, openKFID, mediaID string) error {
	return c.KFSend(ctx, touser, openKFID, "", map[string]interface{}{
		"msgtype": "voice",
		"voice":   map[string]interface{}{"media_id": mediaID},
	})
}

// KFSendVideo 发送微信客服视频消息。
func (c *WeChatClient) KFSendVideo(ctx context.Context, touser, openKFID, mediaID string) error {
	return c.KFSend(ctx, touser, openKFID, "", map[string]interface{}{
		"msgtype": "video",
		"video":   map[string]interface{}{"media_id": mediaID},
	})
}

// KFSendFile 发送微信客服文件消息。
func (c *WeChatClient) KFSendFile(ctx context.Context, touser, openKFID, mediaID string) error {
	return c.KFSend(ctx, touser, openKFID, "", map[string]interface{}{
		"msgtype": "file",
		"file":    map[string]interface{}{"media_id": mediaID},
	})
}

// IsErrCode 判断错误是否为指定 errcode（如 40096 无效的 external_userid）。
func IsErrCode(err error, code int) bool {
	var weErr *WeChatClientError
	if ok := asError(err, &weErr); ok {
		return weErr.ErrCode == code
	}
	return false
}

// asError 尝试将 err 断言为目标类型。
func asError(err error, target interface{}) bool {
	switch t := target.(type) {
	case **WeChatClientError:
		if we, ok := err.(*WeChatClientError); ok {
			*t = we
			return true
		}
	}
	return false
}
