package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	environmentKey = "TOKEN_ENCRYPTION_KEY"
	prefix         = "enc:v1:"
	keySize        = 32
)

var ErrKeyNotConfigured = errors.New("TOKEN_ENCRYPTION_KEY is not configured")

func ValidateKey() error {
	_, err := encryptionKey()
	return err
}

func IsEncrypted(value string) bool {
	return strings.HasPrefix(value, prefix)
}

func Encrypt(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if IsEncrypted(value) {
		if _, err := Decrypt(value); err != nil {
			return "", err
		}
		return value, nil
	}
	key, err := encryptionKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("generate token nonce: %w", err)
	}
	sealed := gcm.Seal(nonce, nonce, []byte(value), nil)
	return prefix + base64.RawStdEncoding.EncodeToString(sealed), nil
}

func Decrypt(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !IsEncrypted(value) {
		return value, nil
	}
	key, err := encryptionKey()
	if err != nil {
		return "", err
	}
	payload, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(value, prefix))
	if err != nil {
		return "", fmt.Errorf("decode encrypted token: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(payload) < gcm.NonceSize() {
		return "", errors.New("encrypted token payload is truncated")
	}
	nonce, ciphertext := payload[:gcm.NonceSize()], payload[gcm.NonceSize():]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", errors.New("decrypt token: invalid encryption key or corrupted data")
	}
	return string(plaintext), nil
}

// BlindIndex derives a deterministic, non-reversible lookup value for a
// secret. It is used for credential de-duplication without exposing a raw
// hash that would let a database reader verify guessed API keys offline.
func BlindIndex(purpose, value string) (string, error) {
	key, err := encryptionKey()
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte("zencoder2api/blind-index/v1\x00" + purpose + "\x00" + value))
	return "v1:" + hex.EncodeToString(mac.Sum(nil)), nil
}

func encryptionKey() ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv(environmentKey))
	if raw == "" {
		return nil, ErrKeyNotConfigured
	}
	encodings := []*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	for _, encoding := range encodings {
		decoded, err := encoding.DecodeString(raw)
		if err == nil && len(decoded) == keySize {
			return decoded, nil
		}
	}
	return nil, fmt.Errorf("%s must be base64-encoded %d bytes", environmentKey, keySize)
}
