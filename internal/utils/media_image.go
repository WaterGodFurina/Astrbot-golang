// Package utils - 自适应模型输入图片准备链路。
//
// 移植自 astrbot/core/utils/media_utils.py（AstrBot Python v4.28.2，
// 提交 bd046ed2 + 02fef48c 引入的 adaptive model image preparation）：
//   - 静图归一化：EXIF 方向矫正、超限缩放、alpha→PNG（>1MB 压平白底转 JPEG）、
//     已是合规 JPEG/PNG 则原字节跳过（内容无损 passthrough）；
//   - 动图拼图：3x3 均匀采样帧拼 contact sheet，白底、最长边受 montage 上限约束；
//   - 内容寻址缓存：data/temp/media_convert_cache 下 sha256(source)+sha256(params)
//     命名，原子写 + 读回校验（损坏视为 miss）；
//   - 可恢复错误分类 IsRecoverableImageError（对齐 py 的异常白名单）。
//
// 与 Python 实现的刻意差异（均有原因）：
//   - 解码器：Go 标准库 image/jpeg|png|gif + x/image/webp|bmp|tiff。标准库不
//     支持 APNG / 动图 WebP 的多帧，这类输入会按静图处理或按可恢复错误跳过；
//     Pillow 的 n_frames 语义在 Go 侧仅 GIF 完整对齐。
//   - 缩放滤波：x/image/draw CatmullRom 近似 Pillow LANCZOS。
//   - ICC profile：Go 标准库 JPEG/PNG 编码器不支持内嵌 ICC，省略 icc_profile
//     透传（Pillow 会显式携带）。
//   - Pillow 的 reducing_gap / 特定 mode（"I;16"）预处理在 Go 中通过
//     Gray16 min-max 归一化等价实现。
package utils

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"image/color"
	stddraw "image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	xdraw "golang.org/x/image/draw"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// 常量对齐 py media_utils.py v4.28.2 顶部定义。
const (
	// ImageCompressDefaultMaxSize 是模型输入图片最长边的默认上限。
	ImageCompressDefaultMaxSize = 1280
	// ImageCompressDefaultQuality 是 JPEG 输出质量的默认值。
	ImageCompressDefaultQuality = 95
	// AnimatedMontageGrid 是动图拼图的网格边长（3x3）。
	AnimatedMontageGrid = 3
	// AnimatedMontageFrameCount 是拼图采样的帧数（3x3=9）。
	AnimatedMontageFrameCount = AnimatedMontageGrid * AnimatedMontageGrid
	// ConvertCacheDirName 是转换缓存目录名（位于 AstrBot temp 目录下）。
	ConvertCacheDirName = "media_convert_cache"
	// ModelImagePNGFallbackMaxBytes 之外的 PNG 输出会被压平到白底并转 JPEG。
	ModelImagePNGFallbackMaxBytes = 1024 * 1024
	// modelImageTempPrefix 是 prepare_model_image 产出的工作文件前缀。
	modelImageTempPrefix = "model_image_"
	// imageConvertCacheVersion 在转换输出语义变化时递增，避免旧缓存被复用。
	imageConvertCacheVersion = "v9-icc"
	// maxImageFetchBytes 限制单张图片读取/下载大小（与 provider 侧 20MB 对齐）。
	maxImageFetchBytes = 20 << 20
)

// ModelImageOptions carries the per-request knobs of PrepareModelImage.
type ModelImageOptions struct {
	// MaxSize 是静图最长边上限（像素）。CUA 会话会被调用方抬到 1_000_000。
	MaxSize int
	// OutputDir 是请求自有工作文件的输出目录（通常是 data/temp）。
	OutputDir string
	// Quality 是 JPEG 输出质量（1-100）。
	Quality int
	// MontageMaxSize 是动图拼图的最长边上限；<=0 时回退到 MaxSize。
	// CUA 会话抬高清图上限但拼图不用于坐标，故调用方单独传入配置值。
	MontageMaxSize int
}

// IsRecoverableImageError 判断图片处理中的普通输入/解码/网络/缓存错误。
// 资源耗尽（ENOMEM/EMFILE/ENFILE）与调用方取消必须向上传播，其余视为可跳过。
// 对齐 py is_recoverable_image_error。
func IsRecoverableImageError(err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, syscall.ENOMEM),
		errors.Is(err, syscall.EMFILE),
		errors.Is(err, syscall.ENFILE):
		return false
	case errors.Is(err, context.Canceled):
		// py 中 asyncio.CancelledError 属于 BaseException，不属于被白名单
		// 捕获的 Exception，会直接向上传播。
		return false
	}
	return true
}

// NormalizeModelImageMaxSize 归一化模型图片最长边上限。
// 接受整数、整数值浮点与整数字符串；bool、非有限数、不可解析值与小于
// 拼图最小网格（3）的值回退默认值并告警（None 表示未配置，静默回退）。
// 对齐 py normalize_model_image_max_size。
func NormalizeModelImageMaxSize(value interface{}) int {
	normalized := 0
	valid := false
	switch v := value.(type) {
	case nil:
		// 未配置：静默回退默认值。
	case bool:
		// 布尔值视为非法（0/1 不应当作尺寸）。
	case int:
		normalized, valid = v, true
	case int64:
		normalized, valid = int(v), true
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) && v == math.Trunc(v) && v >= -1e12 && v <= 1e12 {
			normalized, valid = int(v), true
		}
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			normalized, valid = n, true
		}
	}
	if !valid || normalized < AnimatedMontageGrid {
		if value != nil {
			logger.Warn("Invalid model image max size %v; falling back to %d.", value, ImageCompressDefaultMaxSize)
		}
		return ImageCompressDefaultMaxSize
	}
	return normalized
}

// DetectImageMimeType 通过真实解码识别图片 MIME（对齐 py detect_image_mime_type
// 的语义：能解码才返回；已识别格式无注册 MIME 时返回 application/octet-stream；
// 无法解码返回空串）。
func DetectImageMimeType(data []byte) string {
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return ""
	}
	switch format {
	case "jpeg":
		return "image/jpeg"
	case "png":
		return "image/png"
	case "gif":
		return "image/gif"
	case "webp":
		return "image/webp"
	case "bmp":
		return "image/bmp"
	case "tiff":
		return "image/tiff"
	}
	return "application/octet-stream"
}

// InspectImage 校验并统计图片帧数。静图会被完整解码验证像素，动图帧数在
// 拼图时再逐帧解码。对齐 py _inspect_image。
func InspectImage(data []byte) (int, error) {
	if _, format, err := image.DecodeConfig(bytes.NewReader(data)); err != nil {
		return 0, err
	} else if format == "gif" {
		decoded, err := gif.DecodeAll(bytes.NewReader(data))
		if err != nil {
			return 0, err
		}
		if len(decoded.Image) == 0 {
			return 0, fmt.Errorf("gif contains no frames")
		}
		return len(decoded.Image), nil
	}
	// verify() 只校验结构，静图必须重新打开并加载像素（对齐 py 注释语义）。
	if _, _, err := image.Decode(bytes.NewReader(data)); err != nil {
		return 0, err
	}
	return 1, nil
}

// PrepareModelImage 把单个图片引用准备成模型可消费的本地工作文件。
// 返回调用方持有的 JPEG/PNG 路径；可恢复失败返回 ("", nil)，非可恢复错误
// 原样返回。语义对齐 py prepare_model_image（缓存条目永不直接返回）。
func PrepareModelImage(ctx context.Context, imageRef string, opts ModelImageOptions) (string, error) {
	path, err := prepareModelImage(ctx, imageRef, opts)
	if err != nil {
		if !IsRecoverableImageError(err) {
			return "", err
		}
		logger.Warn("Model image preparation failed; skipping image (%v).", err)
		return "", nil
	}
	return path, nil
}

// prepareModelImage 是 PrepareModelImage 的错误直通实现。
func prepareModelImage(ctx context.Context, imageRef string, opts ModelImageOptions) (string, error) {
	maxSize := opts.MaxSize
	if maxSize <= 0 {
		maxSize = ImageCompressDefaultMaxSize
	}
	quality := opts.Quality
	if quality < 1 {
		quality = ImageCompressDefaultQuality
	}
	if quality > 100 {
		quality = 100
	}
	source, err := readImageRefBytes(ctx, imageRef)
	if err != nil {
		return "", err
	}
	frameCount, err := InspectImage(source)
	if err != nil {
		return "", err
	}
	var converted []byte
	if frameCount > 1 {
		montageMax := opts.MontageMaxSize
		if montageMax <= 0 {
			montageMax = maxSize
		}
		converted, _, err = extractAnimationMontageSync(source, montageMax, quality)
	} else {
		converted, err = convertImageBytesSync(source, maxSize, quality)
	}
	if err != nil {
		return "", err
	}
	// 编码完成后同步发布工作文件：即使随后被取消，也不会留下未归属的
	// 后台写入（对齐 py 注释）。
	if err := os.MkdirAll(opts.OutputDir, 0o750); err != nil {
		return "", err
	}
	suffix := ".png"
	if bytes.HasPrefix(converted, []byte{0xff, 0xd8}) {
		suffix = ".jpg"
	}
	// #nosec G304 -- OutputDir 为宿主受控的 data/temp 目录，文件名由 CreateTemp 生成。
	output, err := os.CreateTemp(opts.OutputDir, modelImageTempPrefix+"*"+suffix)
	if err != nil {
		return "", err
	}
	outputPath := output.Name()
	if _, err := output.Write(converted); err != nil {
		_ = output.Close()
		_ = os.Remove(outputPath)
		return "", err
	}
	if err := output.Close(); err != nil {
		_ = os.Remove(outputPath)
		return "", err
	}
	return outputPath, nil
}

// MaterializeImageRef 把图片引用落到本地路径。
// 返回 (path, owned, err)：owned 为 true 表示返回的是本次新建的临时文件
// （调用方负责追踪/清理）；本地已有文件直接返回其绝对路径且 owned=false。
// 供 provider_settings.image_compress_enabled=false 分支使用（对齐 py
// image_input.py 的 MediaResolver.as_path 本地化 + 仅追踪 resolver 自有文件）。
func MaterializeImageRef(ctx context.Context, imageRef, outputDir string) (string, bool, error) {
	trimmed := strings.TrimSpace(imageRef)
	if trimmed == "" {
		return "", false, fmt.Errorf("empty image reference")
	}
	if !strings.HasPrefix(trimmed, "data:") &&
		!strings.HasPrefix(trimmed, "base64://") &&
		!strings.HasPrefix(trimmed, "http://") &&
		!strings.HasPrefix(trimmed, "https://") {
		path := FileURIToPath(trimmed)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			if abs, absErr := filepath.Abs(path); absErr == nil {
				return abs, false, nil
			}
			return path, false, nil
		}
	}
	data, err := readImageRefBytes(ctx, imageRef)
	if err != nil {
		return "", false, err
	}
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		return "", false, err
	}
	output, err := os.CreateTemp(outputDir, "model_image_src_*")
	if err != nil {
		return "", false, err
	}
	name := output.Name()
	if _, err := output.Write(data); err != nil {
		_ = output.Close()
		_ = os.Remove(name)
		return "", false, err
	}
	if err := output.Close(); err != nil {
		_ = os.Remove(name)
		return "", false, err
	}
	return name, true, nil
}

// ResolveImageRefToDataURL 把图片引用无损序列化为 portable data URI。
// 只做本地化 + 解码识别 MIME，不做任何缩放/转码/拼图（对齐 py v4.28.2
// resolve_image_ref_to_base64_data 语义）。返回 ("", nil) 表示无法识别为
// 图片（调用方按非法图片跳过）；其余错误为可恢复/不可恢复的源读取失败。
func ResolveImageRefToDataURL(imageRef string) (string, error) {
	trimmed := strings.TrimSpace(imageRef)
	if trimmed == "" {
		return "", nil
	}
	var (
		data         []byte
		hintMime     string
		legacyBase64 bool
		err          error
	)
	switch {
	case strings.HasPrefix(trimmed, "data:"):
		data, hintMime, err = decodeDataURLBytes(trimmed)
	case strings.HasPrefix(trimmed, "base64://"):
		data, err = decodeBase64Payload(strings.Join(strings.Fields(strings.TrimPrefix(trimmed, "base64://")), ""))
	case strings.HasPrefix(trimmed, "http://"), strings.HasPrefix(trimmed, "https://"):
		data, err = downloadImageBytes(context.Background(), trimmed)
	default:
		path := FileURIToPath(trimmed)
		if info, statErr := os.Stat(path); statErr == nil && !info.IsDir() {
			data, err = os.ReadFile(path) // #nosec G304 -- 调用方提供的图片引用路径。
			break
		}
		legacyBase64 = true
		data, err = decodeBase64Payload(strings.Join(strings.Fields(trimmed), ""))
	}
	if err != nil {
		return "", err
	}
	if len(data) == 0 {
		return "", nil
	}
	mimeType := DetectImageMimeType(data)
	if mimeType == "" && strings.HasPrefix(hintMime, "image/") {
		// data URI 自带 MIME 且内容无法解码时沿用（对齐 py resolved.mime_type）。
		mimeType = hintMime
	}
	if mimeType == "" && legacyBase64 {
		// 传统裸 base64 无法识别时按 py 默认值 image/jpeg 处理。
		mimeType = "image/jpeg"
	}
	if mimeType == "" {
		return "", nil
	}
	return "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

// readImageRefBytes 读取图片引用的原始字节（data URI / base64:// / 本地路径 /
// file URI / http(s) / 裸 base64）。
func readImageRefBytes(ctx context.Context, imageRef string) ([]byte, error) {
	trimmed := strings.TrimSpace(imageRef)
	if trimmed == "" {
		return nil, fmt.Errorf("empty image reference")
	}
	switch {
	case strings.HasPrefix(trimmed, "data:"):
		data, _, err := decodeDataURLBytes(trimmed)
		return data, err
	case strings.HasPrefix(trimmed, "base64://"):
		return decodeBase64Payload(strings.Join(strings.Fields(strings.TrimPrefix(trimmed, "base64://")), ""))
	case strings.HasPrefix(trimmed, "http://"), strings.HasPrefix(trimmed, "https://"):
		return downloadImageBytes(ctx, trimmed)
	}
	path := FileURIToPath(trimmed)
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		// #nosec G304 -- 调用方提供的图片引用路径。
		return os.ReadFile(path)
	}
	if data, err := decodeBase64Payload(strings.Join(strings.Fields(trimmed), "")); err == nil && len(data) > 0 {
		return data, nil
	}
	return nil, fmt.Errorf("unsupported image reference: %s", trimmed)
}

// decodeDataURLBytes 解析 data:<mime>;base64,<payload>。
func decodeDataURLBytes(raw string) ([]byte, string, error) {
	rest := strings.TrimPrefix(raw, "data:")
	comma := strings.Index(rest, ",")
	if comma < 0 {
		return nil, "", fmt.Errorf("malformed data URL")
	}
	meta, payload := rest[:comma], rest[comma+1:]
	mimeType := ""
	for _, part := range strings.Split(meta, ";") {
		if strings.Contains(part, "/") {
			mimeType = part
		}
	}
	if len(payload) > base64.StdEncoding.EncodedLen(maxImageFetchBytes) {
		return nil, "", fmt.Errorf("media exceeds %d bytes", maxImageFetchBytes)
	}
	data, err := decodeBase64Payload(payload)
	if err != nil {
		return nil, "", err
	}
	return data, mimeType, nil
}

// decodeBase64Payload 解码 base64（容忍空白与缺失 padding，对齐 py
// _decode_base64_payload 的宽容行为）。
func decodeBase64Payload(payload string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(payload)
	if err == nil {
		return data, nil
	}
	if d, rawErr := base64.RawStdEncoding.DecodeString(payload); rawErr == nil {
		return d, nil
	}
	return nil, err
}

// downloadImageBytes 下载远程图片字节（整体超时沿用 downloadClient）。
func downloadImageBytes(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := downloadClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageFetchBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxImageFetchBytes {
		return nil, fmt.Errorf("download exceeds size limit of %d bytes", maxImageFetchBytes)
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// 转换缓存（内容寻址）
// ---------------------------------------------------------------------------

// imageConvertCacheDir 返回转换缓存目录。读取与编码必须能在缓存不可用时继续，
// 因此所有缓存失败都按 miss / 静默失败处理（对齐 py _image_convert_cache_dir）。
func imageConvertCacheDir() string {
	return DataPath("temp", ConvertCacheDirName)
}

// imageConvertCacheKey 构造内容 + 参数寻址键：sha256(source) 前 16 字节 +
// sha256(params) 前 4 字节，均为 hex（对齐 py _image_convert_cache_key）。
func imageConvertCacheKey(source []byte, params string) string {
	sourceDigest := sha256.Sum256(source)
	paramsDigest := sha256.Sum256([]byte(params))
	return hex.EncodeToString(sourceDigest[:16]) + "_" + hex.EncodeToString(paramsDigest[:4])
}

// readValidCachedImageBytes 读取缓存条目；缺失、不可读、空、不可解码、
// 非单帧 JPEG/PNG 或带非 1 方向 EXIF 一律视为 miss（临时清理器可能并发删除）。
// 对齐 py _read_valid_cached_image_bytes。
func readValidCachedImageBytes(outputPath string) []byte {
	data, err := os.ReadFile(outputPath) // #nosec G304 -- 缓存目录内部路径。
	if err != nil {
		return nil
	}
	if len(data) == 0 {
		return nil
	}
	frameCount, err := InspectImage(data)
	if err != nil || frameCount != 1 {
		return nil
	}
	_, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	if format != "jpeg" && format != "png" {
		return nil
	}
	if format == "jpeg" && jpegEXIFOrientation(data) != 1 {
		return nil
	}
	return data
}

// publishImageCacheAtomic 通过唯一临时文件 + 原子替换发布缓存。
// 并发读者永远不会看到半写状态；发布失败（如缓存目录被清理）不致命
// （对齐 py _publish_image_cache_atomic）。
func publishImageCacheAtomic(outputPath string, data []byte) {
	dir := filepath.Dir(outputPath)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		logger.Debug("Failed to create image cache dir %s: %v", dir, err)
		return
	}
	tmpFile, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		logger.Debug("Failed to create image cache temp file: %v", err)
		return
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		logger.Debug("Failed to publish image cache %s: %v", outputPath, err)
		return
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		logger.Debug("Failed to publish image cache %s: %v", outputPath, err)
		return
	}
	if err := os.Rename(tmpPath, outputPath); err != nil {
		_ = os.Remove(tmpPath)
		logger.Debug("Failed to publish image cache %s: %v", outputPath, err)
	}
}

// ---------------------------------------------------------------------------
// 编码
// ---------------------------------------------------------------------------

// encodeImageFrameBytes 把单帧画面编码为展示友好的 JPEG；带透明通道时输出
// PNG，PNG 超过 ModelImagePNGFallbackMaxBytes 则压平白底转 JPEG。
// 对齐 py _encode_image_frame_bytes（Pillow 相关 mode 细节见包注释）。
func encodeImageFrameBytes(img image.Image, orientation, maxSize, quality int) ([]byte, error) {
	if quality < 1 {
		quality = 1
	}
	if quality > 100 {
		quality = 100
	}
	oriented := applyEXIFOrientation(img, orientation)
	// alpha 语义与原图一致（Pillow 在 exif_transpose 后检查 band，band 不变）。
	hasAlpha := imageHasAlpha(img)
	prepared := oriented
	// 高比特深度：Pillow 的 I;I;16 分支做 min-max 归一化后再 JPEG。
	if gray16, ok := prepared.(*image.Gray16); ok {
		prepared = normalizeGray16(gray16)
	}
	if maxSize > 0 {
		prepared = scaleDown(prepared, maxSize)
	}
	buffer := new(bytes.Buffer)
	if hasAlpha {
		if err := png.Encode(buffer, prepared); err != nil {
			return nil, err
		}
		if buffer.Len() <= ModelImagePNGFallbackMaxBytes {
			return buffer.Bytes(), nil
		}
		// 超大 PNG 压平到白底后重新编码为 JPEG。
		buffer.Reset()
		flattened := flattenOntoWhite(prepared)
		if err := jpeg.Encode(buffer, flattened, &jpeg.Options{Quality: quality}); err != nil {
			return nil, err
		}
		return buffer.Bytes(), nil
	}
	if err := jpeg.Encode(buffer, prepared, &jpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// convertImageBytesSync 归一化已校验的静图，带可选内容寻址缓存。
// 方向正常且已合规的 JPEG/PNG 直接复用源字节；其余重编码。
// 对齐 py _convert_image_bytes_sync。
func convertImageBytesSync(source []byte, maxSize, quality int) ([]byte, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(source))
	if err != nil {
		return nil, err
	}
	orientation := 1
	if format == "jpeg" {
		orientation = jpegEXIFOrientation(source)
	}
	if (format == "jpeg" || format == "png") && orientation == 1 &&
		config.Width <= maxSize && config.Height <= maxSize {
		return source, nil
	}
	cacheKey := imageConvertCacheKey(source, fmt.Sprintf("%s|convert|s=%d|q=%d", imageConvertCacheVersion, maxSize, quality))
	outputPath := filepath.Join(imageConvertCacheDir(), cacheKey+".img")
	if cached := readValidCachedImageBytes(outputPath); cached != nil {
		return cached, nil
	}
	img, _, err := image.Decode(bytes.NewReader(source))
	if err != nil {
		return nil, err
	}
	encoded, err := encodeImageFrameBytes(img, orientation, maxSize, quality)
	if err != nil {
		return nil, err
	}
	publishImageCacheAtomic(outputPath, encoded)
	return encoded, nil
}

// evenFrameIndices 均匀选取动画帧下标（含首尾）。
// 对齐 py _even_frame_indices；Go math.Round 与 Python 银行家舍入在
// 恰好 .5 的场景可能有 1 帧差异（刻意差异，影响可忽略）。
func evenFrameIndices(totalFrames, maxFrames int) []int {
	if totalFrames <= 0 {
		return []int{0}
	}
	count := maxFrames
	if totalFrames < count {
		count = totalFrames
	}
	if count <= 1 {
		return []int{0}
	}
	unique := make(map[int]struct{}, count)
	for i := 0; i < count; i++ {
		idx := int(math.Round(float64(i) * float64(totalFrames-1) / float64(count-1)))
		unique[idx] = struct{}{}
	}
	out := make([]int, 0, len(unique))
	for idx := range unique {
		out = append(out, idx)
	}
	sort.Ints(out)
	return out
}

// extractAnimationMontageSync 把动图均匀抽帧拼成 3x3 网格 contact sheet。
// 返回 (montageBytes, ran, err)：ran=false 表示命中缓存未实际抽帧。
// 对齐 py _extract_animation_montage_sync。
func extractAnimationMontageSync(source []byte, maxSize, quality int) ([]byte, bool, error) {
	cacheKey := imageConvertCacheKey(source, fmt.Sprintf("%s|montage|s=%d|q=%d", imageConvertCacheVersion, maxSize, quality))
	outputPath := filepath.Join(imageConvertCacheDir(), cacheKey+".img")
	if cached := readValidCachedImageBytes(outputPath); cached != nil {
		return cached, false, nil
	}
	decoded, err := gif.DecodeAll(bytes.NewReader(source))
	if err != nil {
		return nil, false, err
	}
	totalFrames := len(decoded.Image)
	if totalFrames == 0 {
		return nil, false, fmt.Errorf("gif contains no frames")
	}
	// APNG 的 default_image 概念不适用于 Go 标准库（无多帧 APNG 解码），
	// firstFrame 恒为 0。
	firstFrame := 0
	indices := evenFrameIndices(totalFrames-firstFrame, AnimatedMontageFrameCount)
	for i := range indices {
		indices[i] += firstFrame
	}
	displayW, displayH := decoded.Config.Width, decoded.Config.Height
	if displayW <= 0 || displayH <= 0 {
		bounds := decoded.Image[0].Bounds()
		displayW, displayH = bounds.Dx(), bounds.Dy()
	}
	if displayW <= 0 || displayH <= 0 {
		return nil, false, fmt.Errorf("invalid animation size")
	}
	// 单元格尺寸向下取整，保证拼图最长边不超过 maxSize。
	longestEdge := max(displayW, displayH) * AnimatedMontageGrid
	scale := math.Min(1.0, float64(max(maxSize, 1))/float64(longestEdge))
	cellW := max(1, int(float64(displayW)*scale))
	cellH := max(1, int(float64(displayH)*scale))
	canvas := image.NewRGBA(image.Rect(0, 0, cellW*AnimatedMontageGrid, cellH*AnimatedMontageGrid))
	stddraw.Draw(canvas, canvas.Bounds(), image.NewUniform(color.White), image.Point{}, stddraw.Src)

	// Go 的 gif.DecodeAll 返回未合成的子矩形帧；这里按 GIF89a 的 disposal
	// 规则在逻辑屏幕上逐帧合成（Pillow 的帧已是合成结果，属实现差异）。
	work := image.NewRGBA(image.Rect(0, 0, displayW, displayH))
	selected := make(map[int]bool, len(indices))
	for _, idx := range indices {
		selected[idx] = true
	}
	lastNeeded := 0
	for _, idx := range indices {
		if idx > lastNeeded {
			lastNeeded = idx
		}
	}
	var backup *image.RGBA
	for frameIndex := 0; frameIndex <= lastNeeded && frameIndex < totalFrames; frameIndex++ {
		if frameIndex > 0 {
			switch gifDisposalAt(decoded, frameIndex-1) {
			case gif.DisposalBackground:
				stddraw.Draw(work, decoded.Image[frameIndex-1].Bounds(), image.Transparent, image.Point{}, stddraw.Src)
			case gif.DisposalPrevious:
				if backup != nil {
					saved := backup
					copy(work.Pix, saved.Pix)
				}
			}
		}
		if gifDisposalAt(decoded, frameIndex) == gif.DisposalPrevious {
			if backup == nil {
				backup = image.NewRGBA(work.Bounds())
			}
			copy(backup.Pix, work.Pix)
		}
		frame := decoded.Image[frameIndex]
		stddraw.Draw(work, frame.Bounds(), frame, frame.Bounds().Min, stddraw.Over)
		if !selected[frameIndex] {
			continue
		}
		cell := image.NewRGBA(image.Rect(0, 0, cellW, cellH))
		xdraw.CatmullRom.Scale(cell, cell.Bounds(), work, work.Bounds(), xdraw.Src, nil)
		outIndex := 0
		for i, idx := range indices {
			if idx == frameIndex {
				outIndex = i
				break
			}
		}
		position := image.Pt((outIndex%AnimatedMontageGrid)*cellW, (outIndex/AnimatedMontageGrid)*cellH)
		stddraw.Draw(canvas, image.Rectangle{Min: position, Max: position.Add(cell.Bounds().Size())}, cell, image.Point{}, stddraw.Over)
	}
	// 拼图画布是全不透明白底（py 用 RGB 画布），按 py 语义编码为 JPEG；
	// 不能用 encodeImageFrameBytes（它会因 RGBA 色彩模型判定"带 alpha"而输出 PNG）。
	jpegBuffer := new(bytes.Buffer)
	if err := jpeg.Encode(jpegBuffer, flattenOntoWhite(canvas), &jpeg.Options{Quality: quality}); err != nil {
		return nil, false, err
	}
	encoded := jpegBuffer.Bytes()
	publishImageCacheAtomic(outputPath, encoded)
	return encoded, true, nil
}

// gifDisposalAt 返回第 index 帧的 disposal 方法；缺失时按 0（不处理）对待。
func gifDisposalAt(decoded *gif.GIF, index int) byte {
	if decoded == nil || index < 0 || index >= len(decoded.Disposal) {
		return gif.DisposalNone
	}
	return decoded.Disposal[index]
}

// ---------------------------------------------------------------------------
// 像素工具
// ---------------------------------------------------------------------------

// imageHasAlpha 判断图片是否携带 alpha 通道（对齐 py _image_has_alpha 的
// band 语义：调色板含透明项或颜色模型带 A 即视为透明）。
func imageHasAlpha(img image.Image) bool {
	if paletted, ok := img.(*image.Paletted); ok {
		for _, entry := range paletted.Palette {
			if _, _, _, alpha := entry.RGBA(); alpha != 0xffff {
				return true
			}
		}
		return false
	}
	switch img.ColorModel() {
	case color.NRGBAModel, color.RGBAModel, color.NRGBA64Model, color.RGBA64Model,
		color.AlphaModel, color.Alpha16Model:
		return true
	}
	return false
}

// applyEXIFOrientation 按 EXIF 方向值旋转/镜像画面；方向为 1（或未知）时
// 原样返回，避免无谓拷贝（对齐 py ImageOps.exif_transpose）。
func applyEXIFOrientation(img image.Image, orientation int) image.Image {
	if orientation <= 1 || orientation > 8 {
		return img
	}
	source := toNRGBA(img)
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	var destination *image.NRGBA
	switch orientation {
	case 5, 6, 7, 8:
		destination = image.NewNRGBA(image.Rect(0, 0, height, width))
	default:
		destination = image.NewNRGBA(image.Rect(0, 0, width, height))
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			pixel := source.NRGBAAt(bounds.Min.X+x, bounds.Min.Y+y)
			var dx, dy int
			switch orientation {
			case 2:
				dx, dy = width-1-x, y
			case 3:
				dx, dy = width-1-x, height-1-y
			case 4:
				dx, dy = x, height-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = height-1-y, x
			case 7:
				dx, dy = height-1-y, width-1-x
			case 8:
				dx, dy = y, width-1-x
			}
			destination.SetNRGBA(dx, dy, pixel)
		}
	}
	return destination
}

// toNRGBA 把任意 image.Image 复制为 *image.NRGBA。
func toNRGBA(img image.Image) *image.NRGBA {
	if nrgba, ok := img.(*image.NRGBA); ok {
		return nrgba
	}
	bounds := img.Bounds()
	destination := image.NewNRGBA(bounds)
	stddraw.Draw(destination, bounds, img, bounds.Min, stddraw.Src)
	return destination
}

// scaleDown 按最长边上限等比缩小（不放大）。滤波用 CatmullRom 近似
// Pillow LANCZOS（刻意差异）。
func scaleDown(img image.Image, maxSize int) image.Image {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 || maxSize <= 0 || (width <= maxSize && height <= maxSize) {
		return img
	}
	scale := float64(maxSize) / float64(width)
	if height > width {
		scale = float64(maxSize) / float64(height)
	}
	newWidth := max(1, int(math.Floor(float64(width)*scale)))
	newHeight := max(1, int(math.Floor(float64(height)*scale)))
	destination := image.NewNRGBA(image.Rect(0, 0, newWidth, newHeight))
	xdraw.CatmullRom.Scale(destination, destination.Bounds(), img, bounds, xdraw.Over, nil)
	return destination
}

// normalizeGray16 把 16 位灰度 min-max 归一化到 8 位（对齐 py I;I;16 分支：
// convert() 会直接裁剪，故必须缩放以保留内容）。
func normalizeGray16(img *image.Gray16) *image.Gray {
	bounds := img.Bounds()
	destination := image.NewGray(bounds)
	low, high := uint16(0xffff), uint16(0)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			value := img.Gray16At(x, y).Y
			if value < low {
				low = value
			}
			if value > high {
				high = value
			}
		}
	}
	if high > low {
		span := float64(high - low)
		for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
			for x := bounds.Min.X; x < bounds.Max.X; x++ {
				value := img.Gray16At(x, y).Y
				destination.SetGray(x, y, color.Gray{Y: uint8(float64(value-low) * 255.0 / span)})
			}
		}
	}
	return destination
}

// flattenOntoWhite 把带透明通道的画面压平到白底（对齐 py 的
// PILImage.new("RGB", ..., white) + paste(mask=alpha)）。
func flattenOntoWhite(img image.Image) *image.RGBA {
	bounds := img.Bounds()
	destination := image.NewRGBA(bounds)
	stddraw.Draw(destination, bounds, image.NewUniform(color.White), image.Point{}, stddraw.Src)
	stddraw.Draw(destination, bounds, img, bounds.Min, stddraw.Over)
	return destination
}

// ---------------------------------------------------------------------------
// EXIF
// ---------------------------------------------------------------------------

// jpegEXIFOrientation 从 JPEG APP1(Exif) 段读取方向标签 274；缺失/异常时
// 返回 1（对齐 Pillow getexif().get(274, 1)）。PNG 的 eXIf chunk 未解析
// （刻意差异，Go 标准库无 EXIF 支持）。
func jpegEXIFOrientation(data []byte) int {
	const (
		markerPrefix = 0xff
		markerAPP1   = 0xe1
		markerSOS    = 0xda
	)
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 1
	}
	offset := 2
	for offset+4 <= len(data) {
		if data[offset] != markerPrefix {
			break
		}
		marker := data[offset+1]
		if marker == 0xd8 || marker == 0x01 || (marker >= 0xd0 && marker <= 0xd7) {
			offset += 2
			continue
		}
		segmentLength := int(data[offset+2])<<8 | int(data[offset+3])
		if segmentLength < 2 || offset+2+segmentLength > len(data) {
			break
		}
		if marker == markerAPP1 {
			payload := data[offset+4 : offset+2+segmentLength]
			if len(payload) >= 6 && string(payload[:6]) == "Exif\x00\x00" {
				return tiffOrientation(payload[6:])
			}
		}
		if marker == markerSOS {
			break
		}
		offset += 2 + segmentLength
	}
	return 1
}

// tiffOrientation 解析 TIFF 头内的 IFD0 方向标签 274。
func tiffOrientation(data []byte) int {
	if len(data) < 8 {
		return 1
	}
	var byteOrder binaryByteOrder
	switch {
	case data[0] == 'I' && data[1] == 'I':
		byteOrder = littleEndian{}
	case data[0] == 'M' && data[1] == 'M':
		byteOrder = bigEndian{}
	default:
		return 1
	}
	if byteOrder.uint16(data[2:4]) != 42 {
		return 1
	}
	ifdOffset := int(byteOrder.uint32(data[4:8]))
	if ifdOffset < 0 || ifdOffset+2 > len(data) {
		return 1
	}
	entryCount := int(byteOrder.uint16(data[ifdOffset : ifdOffset+2]))
	for i := 0; i < entryCount; i++ {
		entryOffset := ifdOffset + 2 + i*12
		if entryOffset+12 > len(data) {
			return 1
		}
		tag := byteOrder.uint16(data[entryOffset : entryOffset+2])
		if tag != 0x0112 {
			continue
		}
		fieldType := byteOrder.uint16(data[entryOffset+2 : entryOffset+4])
		count := int(byteOrder.uint32(data[entryOffset+4 : entryOffset+8]))
		if fieldType != 3 || count < 1 {
			return 1
		}
		value := int(byteOrder.uint16(data[entryOffset+8 : entryOffset+10]))
		if value < 1 || value > 8 {
			return 1
		}
		return value
	}
	return 1
}

// binaryByteOrder 抽象 TIFF 的端序读取（仅用于 EXIF 方向解析）。
type binaryByteOrder interface {
	uint16(b []byte) uint16
	uint32(b []byte) uint32
}

type littleEndian struct{}
type bigEndian struct{}

func (littleEndian) uint16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }
func (littleEndian) uint32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
func (bigEndian) uint16(b []byte) uint16 { return uint16(b[1]) | uint16(b[0])<<8 }
func (bigEndian) uint32(b []byte) uint32 {
	return uint32(b[3]) | uint32(b[2])<<8 | uint32(b[1])<<16 | uint32(b[0])<<24
}
