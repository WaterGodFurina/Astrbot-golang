// 企业微信（WeCom）消息发送逻辑。
// 1:1 移植自 wecom_event.py 的 WecomPlatformEvent.send：
//   - 企业微信应用模式：message/send 发送 text/image/voice/video/file；
//   - 微信客服模式：kf/send_msg 发送，errcode=40096 时回退到应用消息接口；
//   - 长文本按 2048 字符分割，每条之间间隔 0.5s。
package wecom

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/platform"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// sendChain 发送消息链（对应 Python WecomPlatformEvent.send）。
// selfID 为发送方标识（应用模式为 agent_id，客服模式为 open_kfid），touser 为接收者。
func (a *Adapter) sendChain(chain *message.MessageChain, selfID, touser string) error {
	isKF := a.kfName != ""
	ctx := context.Background()
	for _, comp := range chain.Chain {
		switch c := comp.(type) {
		case *message.Plain:
			chunks := SplitPlain(c.Text)
			for _, chunk := range chunks {
				if isKF {
					if err := a.client.KFSendText(ctx, touser, selfID, chunk); err != nil {
						if IsErrCode(err, 40096) {
							// 40096: invalid external userid, fallback to regular message API
							logger.I18nWarn("kf API error 40096 for user %s, falling back to regular message API", touser)
							if ferr := a.client.SendText(ctx, selfID, touser, chunk); ferr != nil {
								return ferr
							}
						} else {
							return err
						}
					}
				} else {
					if err := a.client.SendText(ctx, selfID, touser, chunk); err != nil {
						return err
					}
				}
				// 避免发送过快（对应 Python asyncio.sleep(0.5)）
				time.Sleep(500 * time.Millisecond)
			}
		case *message.Image:
			path, err := resolveComponentFile(c.Path, c.File, c.URL, c.Base64, ".jpg")
			if err != nil {
				logger.I18nError("准备图片失败: %v", err)
				return err
			}
			defer removeWecomTemp(path)
			mediaID, err := a.client.UploadMedia(ctx, "image", path)
			if err != nil {
				logger.I18nError("上传图片失败: %v", err)
				a.sendTextFallback(isKF, selfID, touser, fmt.Sprintf("图片上传失败: %v", err))
				return err
			}
			if isKF {
				if err := a.client.KFSendImage(ctx, touser, selfID, mediaID); err != nil {
					return err
				}
			} else if err := a.client.SendImage(ctx, selfID, touser, mediaID); err != nil {
				return err
			}
		case *message.Record:
			path, err := resolveComponentFile(c.Path, c.File, c.URL, c.Base64, ".amr")
			if err != nil {
				logger.I18nError("准备语音失败: %v", err)
				return err
			}
			defer removeWecomTemp(path)
			// Python 此处会将音频转码为 amr；Go 端直接上传原文件
			mediaID, err := a.client.UploadMedia(ctx, "voice", path)
			if err != nil {
				logger.I18nError("上传语音失败: %v", err)
				a.sendTextFallback(isKF, selfID, touser, fmt.Sprintf("语音上传失败: %v", err))
				return err
			}
			if isKF {
				if err := a.client.KFSendVoice(ctx, touser, selfID, mediaID); err != nil {
					return err
				}
			} else if err := a.client.SendVoice(ctx, selfID, touser, mediaID); err != nil {
				return err
			}
		case *message.File:
			path, err := resolveComponentFile(c.Path, "", c.URL, "", ".bin")
			if err != nil {
				logger.I18nError("准备文件失败: %v", err)
				return err
			}
			defer removeWecomTemp(path)
			mediaID, err := a.client.UploadMedia(ctx, "file", path)
			if err != nil {
				logger.I18nError("上传文件失败: %v", err)
				a.sendTextFallback(isKF, selfID, touser, fmt.Sprintf("文件上传失败: %v", err))
				return err
			}
			if isKF {
				if err := a.client.KFSendFile(ctx, touser, selfID, mediaID); err != nil {
					return err
				}
			} else if err := a.client.SendFile(ctx, selfID, touser, mediaID); err != nil {
				return err
			}
		case *message.Video:
			path, err := resolveComponentFile(c.Path, "", c.URL, "", ".mp4")
			if err != nil {
				logger.I18nError("准备视频失败: %v", err)
				return err
			}
			defer removeWecomTemp(path)
			mediaID, err := a.client.UploadMedia(ctx, "video", path)
			if err != nil {
				logger.I18nError("上传视频失败: %v", err)
				a.sendTextFallback(isKF, selfID, touser, fmt.Sprintf("视频上传失败: %v", err))
				return err
			}
			if isKF {
				if err := a.client.KFSendVideo(ctx, touser, selfID, mediaID); err != nil {
					return err
				}
			} else if err := a.client.SendVideo(ctx, selfID, touser, mediaID); err != nil {
				return err
			}
		default:
			logger.I18nWarn("还没实现这个消息类型的发送逻辑: %s。", comp.Type())
		}
	}
	return nil
}

// sendTextFallback 上传失败时以文本形式通知用户（对应 Python 递归调用 self.send(...)）。
func (a *Adapter) sendTextFallback(isKF bool, selfID, touser, text string) {
	ctx := context.Background()
	if isKF {
		if err := a.client.KFSendText(ctx, touser, selfID, text); err != nil {
			logger.I18nError("发送失败通知文本失败: %v", err)
		}
		return
	}
	if err := a.client.SendText(ctx, selfID, touser, text); err != nil {
		logger.I18nError("发送失败通知文本失败: %v", err)
	}
}

// resolveComponentFile 将组件媒体引用解析为本地文件路径：
//   - path / file：直接使用（存在性校验）；
//   - base64：解码写入临时文件；
//   - url：下载到临时文件。
func resolveComponentFile(path, file, url, b64, suffix string) (string, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		p = strings.TrimSpace(file)
	}
	if p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		logger.I18nWarn("媒体文件不存在，尝试其他方式: %s", p)
	}
	if b64 != "" {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
		if err != nil {
			return "", fmt.Errorf("base64 解码媒体失败: %w", err)
		}
		tmp := fmt.Sprintf("%s/astrbot_wecom_%d%s", os.TempDir(), time.Now().UnixNano(), suffix)
		if err := os.WriteFile(tmp, raw, 0600); err != nil {
			return "", err
		}
		return tmp, nil
	}
	if url != "" {
		tmp := fmt.Sprintf("%s/astrbot_wecom_%d%s", os.TempDir(), time.Now().UnixNano(), suffix)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := downloadToFile(ctx, url, tmp); err != nil {
			return "", err
		}
		return tmp, nil
	}
	return "", fmt.Errorf("媒体组件没有可用的 path/url/base64")
}

// downloadToFile 下载 URL 内容到本地文件（经 SSRF 校验与大小上限约束）。
func downloadToFile(ctx context.Context, url, dest string) error {
	data, err := platform.SafeDownloadBytes(ctx, url, 64<<20)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, data, 0600)
}

// removeWecomTemp 删除发送链路经 resolveComponentFile 创建在临时目录的媒体文件
// （仅限本模块创建的 astrbot_wecom_* 文件，避免误删调用方自备文件）。
func removeWecomTemp(p string) {
	if p != "" && strings.HasPrefix(p, os.TempDir()+string(os.PathSeparator)) &&
		strings.Contains(filepath.Base(p), "astrbot_wecom_") {
		_ = os.Remove(p)
	}
}
