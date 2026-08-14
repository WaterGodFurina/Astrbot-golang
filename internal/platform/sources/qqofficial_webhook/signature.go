package qqofficial_webhook

// QQ 官方开放平台 Webhook 回调的 Ed25519 签名工具。
// 1:1 移植自 qo_webhook_server.py 的 _build_ed25519_seed/_sign_qq_webhook_payload/
// _verify_qq_webhook_signature。

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
)

const (
	// signatureHeader 回调签名字段
	signatureHeader = "X-Signature-Ed25519"
	// signatureTimestampHeader 回调时间戳字段
	signatureTimestampHeader = "X-Signature-Timestamp"
	// ed25519SeedSize Ed25519 私钥种子长度（32 字节）
	ed25519SeedSize = 32
	// ed25519SignatureSize Ed25519 签名长度（64 字节）
	ed25519SignatureSize = 64
)

// buildEd25519Seed 从 secret 构建 ed25519 私钥种子（对齐 Python _build_ed25519_seed）。
// secret 不足 32 字节时不断自加倍长后截断。
func buildEd25519Seed(secret string) ([]byte, error) {
	if secret == "" {
		return nil, errors.New("QQ official bot secret is empty")
	}
	seed := []byte(secret)
	for len(seed) < ed25519SeedSize {
		seed = append(seed, seed...)
	}
	return seed[:ed25519SeedSize], nil
}

// signQQWebhookPayload 对回调载荷进行 ed25519 签名（对齐 Python _sign_qq_webhook_payload）。
// 签名内容为 timestamp + payload。
func signQQWebhookPayload(secret, timestamp string, payload []byte) (string, error) {
	seed, err := buildEd25519Seed(secret)
	if err != nil {
		return "", err
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	signed := ed25519.Sign(privateKey, append([]byte(timestamp), payload...))
	return hex.EncodeToString(signed), nil
}

// verifyQQWebhookSignature 校验 QQ 官方 webhook 回调签名
// （对齐 Python _verify_qq_webhook_signature）。
func verifyQQWebhookSignature(secret, timestamp, signature string, body []byte) bool {
	if timestamp == "" || signature == "" {
		return false
	}

	signatureBuffer, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	// 签名长度与规范形式检查（对齐 Python）
	if len(signatureBuffer) != ed25519SignatureSize || signatureBuffer[63]&224 != 0 {
		return false
	}

	seed, err := buildEd25519Seed(secret)
	if err != nil {
		return false
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return ed25519.Verify(publicKey, append([]byte(timestamp), body...), signatureBuffer)
}
