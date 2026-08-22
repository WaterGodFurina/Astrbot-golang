// Slack 消息链 → Slack Blocks 转换与文件上传。
// 1:1 移植自 astrbot/core/platform/sources/slack/slack_event.py。
//
// 说明：Python 的 files_upload_v2 返回完整的文件对象（url_private/permalink），
// slack-go v0.27.0 的 UploadFile 只返回 FileSummary(ID/Title)，因此上传后
// 再调用 files.info 获取文件详细信息，行为对齐 Python。
package slack

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/slack-go/slack"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// blockSection 构造一个 mrkdwn 文本段落块。
func blockSection(text string) *slack.SectionBlock {
	return slack.NewSectionBlock(
		slack.NewTextBlockObject("mrkdwn", text, false, false),
		nil, nil,
	)
}

// parseSlackBlocks 将消息链解析为 Slack Blocks。
// 对应 Python 的 _parse_slack_blocks：连续 Plain 合并为一个文本块，
// 其他组件单独成块；返回 (blocks, text)。
func parseSlackBlocks(ctx context.Context, chain *message.MessageChain, client *slack.Client) ([]slack.Block, string) {
	if chain == nil {
		return nil, ""
	}
	var blocks []slack.Block
	var textContent strings.Builder

	for _, comp := range chain.Chain {
		plain, isPlain := comp.(*message.Plain)
		if isPlain {
			textContent.WriteString(plain.Text)
			continue
		}
		// 有累积文本时先输出文本块
		if strings.TrimSpace(textContent.String()) != "" {
			blocks = append(blocks, blockSection(textContent.String()))
			textContent.Reset()
		}
		// 添加其他类型的块
		block := fromSegmentToSlackBlock(ctx, comp, client)
		if block != nil {
			blocks = append(blocks, block)
		}
	}
	// 结尾剩余文本
	if strings.TrimSpace(textContent.String()) != "" {
		blocks = append(blocks, blockSection(textContent.String()))
	}

	if len(blocks) > 0 {
		return blocks, ""
	}
	return nil, textContent.String()
}

// fromSegmentToSlackBlock 将单个消息段转换为 Slack 块。
// 对应 Python 的 _from_segment_to_slack_block。
func fromSegmentToSlackBlock(ctx context.Context, comp message.Component, client *slack.Client) slack.Block {
	switch c := comp.(type) {
	case *message.Plain:
		return blockSection(c.Text)
	case *message.Image:
		url := c.URL
		if url == "" {
			url = c.Path
		}
		if url == "" {
			url = c.File
		}
		if url != "" && strings.HasPrefix(url, "http") {
			// 公共 URL 直接引用
			return slack.NewImageBlock(url, "图片", "", nil)
		}
		// 本地文件：上传后引用 url_private
		path := url
		if path == "" {
			path = c.Path
		}
		if path == "" {
			path = c.File
		}
		imageURL, ok := uploadAndGetURL(ctx, client, path, filepath.Base(path))
		if !ok {
			return blockSection("图片上传失败")
		}
		// 对应 Python 的 {"type":"image","slack_file":{"url":...},"alt_text":"图片"}
		return &slack.ImageBlock{
			Type: slack.MBTImage,
			SlackFile: &slack.SlackFileObject{
				URL: imageURL,
			},
			AltText: "图片",
		}
	case *message.File:
		// 本地路径优先（materialize 已把远程 URL 下载到 Path），
		// 避免把 URL 字符串直接喂给 os.Stat。
		if c.Path == "" || !fileExists(c.Path) {
			return blockSection("文件上传失败（无本地文件）")
		}
		name := c.Name
		if name == "" {
			name = "file"
		}
		permalink, ok := uploadAndGetPermalink(ctx, client, c.Path, name)
		if !ok {
			return blockSection("文件上传失败")
		}
		// 对应 Python 的 "文件: <url|name>"
		return blockSection(fmt.Sprintf("文件: <%s|%s>", permalink, name))
	}
	return nil
}

// fileExists 判断路径是否为本地存在的常规文件。
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// uploadAndGetURL 上传本地文件并返回 url_private。
func uploadAndGetURL(ctx context.Context, client *slack.Client, path, filename string) (string, bool) {
	if path == "" {
		return "", false
	}
	fileID, ok := uploadFile(ctx, client, path, filename)
	if !ok {
		return "", false
	}
	file, _, _, err := client.GetFileInfoContext(ctx, fileID, 0, 1)
	if err != nil {
		logger.I18nWarn("Slack 获取上传文件信息失败: %v", err)
		return "", false
	}
	if file == nil || file.URLPrivate == "" {
		return "", false
	}
	return file.URLPrivate, true
}

// uploadAndGetPermalink 上传本地文件并返回 permalink。
func uploadAndGetPermalink(ctx context.Context, client *slack.Client, path, filename string) (string, bool) {
	if path == "" {
		return "", false
	}
	fileID, ok := uploadFile(ctx, client, path, filename)
	if !ok {
		return "", false
	}
	file, _, _, err := client.GetFileInfoContext(ctx, fileID, 0, 1)
	if err != nil {
		logger.I18nWarn("Slack 获取上传文件信息失败: %v", err)
		return "", false
	}
	if file == nil || file.Permalink == "" {
		return "", false
	}
	return file.Permalink, true
}

// uploadFile 上传本地文件（files.upload，对应 Python 的 files_upload_v2）。
func uploadFile(ctx context.Context, client *slack.Client, path, filename string) (string, bool) {
	if filename == "" {
		filename = "file"
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		logger.I18nWarn("Slack 上传文件不存在: %s", path)
		return "", false
	}
	summary, err := client.UploadFileContext(ctx, slack.UploadFileParameters{
		File:     path,
		Filename: filename,
		FileSize: int(info.Size()),
	})
	if err != nil {
		logger.I18nError("Slack 文件上传失败: %v", err)
		return "", false
	}
	return summary.ID, true
}
