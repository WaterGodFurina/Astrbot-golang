package lark

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// encryptForTest AES-256-CBC encrypts plain with the lark key derivation
// (sha256 of the key, PKCS7 padding, random IV prefixed).
func encryptForTest(t *testing.T, key, plain string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		t.Fatal(err)
	}
	iv := bytes.Repeat([]byte{0x01}, aes.BlockSize)
	pt := []byte(plain)
	pad := aes.BlockSize - len(pt)%aes.BlockSize
	pt = append(pt, bytes.Repeat([]byte{byte(pad)}, pad)...)
	ct := make([]byte, len(pt))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, pt)
	return base64.StdEncoding.EncodeToString(append(iv, ct...))
}

// signatureForTest computes the X-Lark-Signature header value:
// sha256(timestamp + nonce + encrypt_key + body) hex-encoded.
func signatureForTest(t *testing.T, key, timestamp, nonce, body string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(timestamp + nonce + key + body))
	return hex.EncodeToString(sum[:])
}
