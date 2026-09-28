// Package pipeline - 自适应模型输入图片准备链路接线。
//
// 移植自 AstrBot Python v4.28.2：
//   - astrbot/core/pipeline/process_stage/method/agent_sub_stages/image_input.py
//     （prepare_request_images：逐引用准备、成功路径归属事件、失败占位）
//   - astrbot/core/pipeline/process_stage/method/agent_sub_stages/internal.py
//     （配置读取、CUA 抬高清图上限、超大图告警、失败回退 [Image unavailable]）
//
// 与 py 的刻意差异：
//   - py 在 build_main_agent 前后各调用一次 prepare_request_images（第一次
//     服务于独立引用图描述 provider）。Go 主链路没有该分支，只在 on_llm_request
//     钩子之后调用一次，等价于 py 的第二次调用（req 内容相同）。
//   - py 的引用图 fallback 抓取依赖平台 get_forward_msg；Go 未实现，这里只
//     收集消息链内嵌的引用/转发图片（与 collectFileAttachments 的边界一致）。
package pipeline

import (
	"context"
	"os"
	"strings"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
	"github.com/WaterGodFurina/Astrbot-golang/internal/provider"
	"github.com/WaterGodFurina/Astrbot-golang/internal/utils"
	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// cuaImageWarnBytes 是 CUA 会话透传大图的告警阈值。
// Anthropic 拒绝超过 5MB 的图片；OpenAI/Gemini 允许约 20MB。
// 对齐 py internal.py 的 _CUA_IMAGE_WARN_BYTES。
const cuaImageWarnBytes = 5 * 1024 * 1024

// cuaPixelPassthroughMaxSize 是 CUA（sandbox + cua booter）会话的静图边长
// 上限：像素工具按 1:1 读坐标，静图不缩放（合规图片逐字节透传）。
// 对齐 py internal.py 的 max_size = 1_000_000。
const cuaPixelPassthroughMaxSize = 1_000_000

// modelImagePrepareSettings 是 provider_settings 中图片准备相关配置的解析结果。
type modelImagePrepareSettings struct {
	enabled        bool
	maxSize        int
	montageMaxSize int
	quality        int
	cuaPixelMode   bool
}

// resolveModelImagePrepareSettings 读取 provider_settings：
//   - image_compress_enabled（默认 true，仅显式 false 关闭）
//   - image_compress_options.max_size（NormalizeModelImageMaxSize，无效回退 1280）
//   - image_compress_options.quality（bool/非整数回退默认；钳制 1-100）
//   - computer_use_runtime == "sandbox" && sandbox.booter == "cua" 时静图上限
//     抬到 cuaPixelPassthroughMaxSize，拼图仍用配置值。
//
// 对齐 py internal.py:246-289。
func resolveModelImagePrepareSettings(providerSettings map[string]interface{}) modelImagePrepareSettings {
	settings := modelImagePrepareSettings{
		enabled: true,
		quality: utils.ImageCompressDefaultQuality,
	}
	if enabled, ok := providerSettings["image_compress_enabled"].(bool); ok {
		settings.enabled = enabled
	}
	var rawMaxSize interface{}
	if options, ok := providerSettings["image_compress_options"].(map[string]interface{}); ok {
		rawMaxSize = options["max_size"]
	}
	settings.montageMaxSize = utils.NormalizeModelImageMaxSize(rawMaxSize)
	settings.maxSize = settings.montageMaxSize
	if runtimeMode, _ := providerSettings["computer_use_runtime"].(string); runtimeMode == "sandbox" {
		if sandbox, ok := providerSettings["sandbox"].(map[string]interface{}); ok {
			if booter, _ := sandbox["booter"].(string); booter == "cua" {
				settings.cuaPixelMode = true
				settings.maxSize = cuaPixelPassthroughMaxSize
			}
		}
	}
	if options, ok := providerSettings["image_compress_options"].(map[string]interface{}); ok {
		switch quality := options["quality"].(type) {
		case int:
			settings.quality = quality
		case int64:
			settings.quality = int(quality)
		case float64:
			if quality == float64(int(quality)) {
				settings.quality = int(quality)
			}
		}
	}
	if settings.quality < 1 {
		settings.quality = 1
	}
	if settings.quality > 100 {
		settings.quality = 100
	}
	return settings
}

// normalizeAndDedupeStrings 去首尾空白、丢弃空项、保序去重。
// 对齐 py normalize_and_dedupe_strings。
func normalizeAndDedupeStrings(items []string) []string {
	normalized := make([]string, 0, len(items))
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		cleaned := strings.TrimSpace(item)
		if cleaned == "" {
			continue
		}
		if _, ok := seen[cleaned]; ok {
			continue
		}
		seen[cleaned] = struct{}{}
		normalized = append(normalized, cleaned)
	}
	return normalized
}

// prepareRequestImages 替换请求上的当前图片为模型可消费的工作文件，并把产出
// 文件归属到事件（事件结束统一清理）。
// 覆盖 req.ImageURLs 与 req.ExtraUserContentParts 中的 image_url 块；引用/
// 转发图片已由 collectQuotedImageURLs 在构建请求时并入 req.ImageURLs。
// 对齐 py prepare_request_images。
func (s *ProcessStage) prepareRequestImages(ctx context.Context, event *core.Event, req *provider.ProviderRequest) error {
	settings := resolveModelImagePrepareSettings(s.providerSettingsMap())
	outputDir := utils.DataPath("temp")

	// ref -> 已准备路径（"" 表示失败）；同时登记 path -> path 以复用同一工作文件。
	prepared := make(map[string]string)
	req.ImageURLs = normalizeAndDedupeStrings(req.ImageURLs)
	refs := make([]string, 0, len(req.ImageURLs)+len(req.ExtraUserContentParts))
	refs = append(refs, req.ImageURLs...)
	for _, part := range req.ExtraUserContentParts {
		if ref := extraPartImageRef(part); ref != "" {
			refs = append(refs, ref)
		}
	}

	failed := false
	for _, ref := range normalizeAndDedupeStrings(refs) {
		if _, done := prepared[ref]; done {
			continue
		}
		path := ""
		if settings.enabled {
			preparedPath, err := utils.PrepareModelImage(ctx, ref, utils.ModelImageOptions{
				MaxSize:        settings.maxSize,
				OutputDir:      outputDir,
				Quality:        settings.quality,
				MontageMaxSize: settings.montageMaxSize,
			})
			if err != nil {
				if !utils.IsRecoverableImageError(err) {
					return err
				}
				logger.Warn("Model image preparation failed; skipping image (%T).", err)
			}
			path = preparedPath
			if path != "" {
				// 模型工作副本归事件所有，事件结束统一删除（对齐 py
				// prepare_request_images 的 track_temporary_local_file）。
				event.TrackTemporaryFile(path)
			}
		} else {
			// 关闭压缩时仍本地化图片引用（下载/解码落盘），但只追踪本次创建的
			// 文件，绝不接管用户文件（对齐 py image_input.py 的 disabled 分支）。
			localPath, owned, err := utils.MaterializeImageRef(ctx, ref, outputDir)
			if err != nil {
				if !utils.IsRecoverableImageError(err) {
					return err
				}
				logger.Warn("Image localization failed; skipping image (%T).", err)
			}
			path = localPath
			if owned && path != "" {
				event.TrackTemporaryFile(path)
			}
		}
		if path != "" {
			prepared[path] = path
		}
		prepared[ref] = path
		if path == "" {
			failed = true
		}
	}

	// 重写 req.ImageURLs：仅保留成功准备好的路径。
	rewrittenURLs := make([]string, 0, len(req.ImageURLs))
	for _, ref := range req.ImageURLs {
		if path := prepared[ref]; path != "" {
			rewrittenURLs = append(rewrittenURLs, path)
		}
	}
	req.ImageURLs = normalizeAndDedupeStrings(rewrittenURLs)

	// 重写 extra 内容块：失败的图片块整个跳过，成功的替换 url。
	rewrittenParts := make([]map[string]interface{}, 0, len(req.ExtraUserContentParts))
	for _, part := range req.ExtraUserContentParts {
		ref := extraPartImageRef(part)
		if ref == "" {
			rewrittenParts = append(rewrittenParts, part)
			continue
		}
		path := prepared[ref]
		if path == "" {
			continue
		}
		rewrittenParts = append(rewrittenParts, replaceExtraPartImageURL(part, path))
	}
	req.ExtraUserContentParts = rewrittenParts

	// 全部图片失败且消息中已无其它内容时给出占位提示（对齐 py 占位逻辑）。
	if failed && strings.TrimSpace(req.Prompt) == "" && len(req.ImageURLs) == 0 && len(req.AudioURLs) == 0 {
		hasOtherContent := false
		for _, part := range rewrittenParts {
			partType, _ := part["type"].(string)
			text, _ := part["text"].(string)
			if partType != "text" || strings.TrimSpace(text) != "" {
				hasOtherContent = true
				break
			}
		}
		if !hasOtherContent {
			req.Prompt = "[Image unavailable]"
		}
	}

	if settings.cuaPixelMode {
		warnOversizedCuaImages(prepared)
	}
	return nil
}

// providerSettingsMap 返回 provider_settings 的原始 map（不存在时返回空 map）。
func (s *ProcessStage) providerSettingsMap() map[string]interface{} {
	if ps, ok := s.config["provider_settings"].(map[string]interface{}); ok {
		return ps
	}
	return map[string]interface{}{}
}

// extraPartImageRef 提取 extra 内容块中的图片引用；非图片块返回空串。
func extraPartImageRef(part map[string]interface{}) string {
	if partType, _ := part["type"].(string); partType != "image_url" {
		return ""
	}
	imageURL, _ := part["image_url"].(map[string]interface{})
	ref, _ := imageURL["url"].(string)
	return ref
}

// replaceExtraPartImageURL 返回替换 url 后的新内容块（浅拷贝，不改写原块）。
func replaceExtraPartImageURL(part map[string]interface{}, url string) map[string]interface{} {
	imageURL, _ := part["image_url"].(map[string]interface{})
	copiedImageURL := make(map[string]interface{}, len(imageURL)+1)
	for key, value := range imageURL {
		copiedImageURL[key] = value
	}
	copiedImageURL["url"] = url
	copiedPart := make(map[string]interface{}, len(part)+1)
	for key, value := range part {
		copiedPart[key] = value
	}
	copiedPart["image_url"] = copiedImageURL
	return copiedPart
}

// collectQuotedImageURLs 收集引用消息/转发节点内嵌的图片引用（URL 优先，
// 其次本地路径 file URI，最后 base64 data URI），与 collectMediaURLs 的取值
// 顺序一致。py 对仅 id 的引用还会走平台 fallback 抓取；Go 未实现
// get_forward_msg，这里只覆盖消息链内嵌图片（刻意差异）。
func collectQuotedImageURLs(event *core.Event) []string {
	if event == nil || event.Message == nil {
		return nil
	}
	var refs []string
	var walk func(components []message.Component)
	walk = func(components []message.Component) {
		for _, component := range components {
			switch comp := component.(type) {
			case *message.Image:
				if ref := imageComponentRef(comp); ref != "" {
					refs = append(refs, ref)
				}
			case *message.Reply:
				walk(comp.Chain)
			case *message.Node:
				walk(comp.Content)
			case *message.Nodes:
				for _, node := range comp.Nodes {
					if node != nil {
						walk(node.Content)
					}
				}
			}
		}
	}
	for _, component := range event.Message.Chain {
		switch comp := component.(type) {
		case *message.Reply:
			walk(comp.Chain)
		case *message.Node:
			walk(comp.Content)
		case *message.Nodes:
			for _, node := range comp.Nodes {
				if node != nil {
					walk(node.Content)
				}
			}
		}
	}
	return refs
}

// imageComponentRef 返回 Image 组件的候选引用（与 collectMediaURLs 同序）。
func imageComponentRef(img *message.Image) string {
	switch {
	case img.URL != "":
		return img.URL
	case img.Path != "":
		return utils.PathToFileURI(img.Path)
	case img.Base64 != "":
		return "data:image/png;base64," + img.Base64
	}
	return ""
}

// warnOversizedCuaImages 对 CUA 会话中超过 5MB 的透传图片告警。
// 对齐 py internal.py 的 cua_pixel_mode 告警块。
func warnOversizedCuaImages(prepared map[string]string) {
	oversized := make([]int64, 0)
	seen := make(map[string]struct{})
	for _, path := range prepared {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.Size() > cuaImageWarnBytes {
			oversized = append(oversized, info.Size())
		}
	}
	if len(oversized) == 0 {
		return
	}
	largest := oversized[0]
	for _, size := range oversized[1:] {
		if size > largest {
			largest = size
		}
	}
	logger.Warn("CUA session sends %d image(s) larger than %d MB (largest %.1f MB) without resize; this may exceed provider image upload limits.",
		len(oversized), cuaImageWarnBytes/1048576, float64(largest)/1048576)
}
