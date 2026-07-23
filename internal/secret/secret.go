package secret

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

const legacyEncryptedPrefix = "enc:v1:"

var ErrLegacyEncryptedValue = errors.New("legacy encrypted credential is no longer supported; recreate the account")

// Plaintext keeps credential storage explicit while rejecting values written
// by older encrypted releases. Treating legacy ciphertext as a usable token
// would produce misleading upstream authentication failures.
func Plaintext(value string) (string, error) {
	if strings.HasPrefix(value, legacyEncryptedPrefix) {
		return "", ErrLegacyEncryptedValue
	}
	return value, nil
}

// BlindIndex creates a stable identifier without storing the credential in an
// indexed column. Credentials themselves are intentionally stored as plaintext.
func BlindIndex(purpose, value string) (string, error) {
	digest := sha256.Sum256([]byte("zencoder2api/plaintext-index/v1\x00" + purpose + "\x00" + value))
	return "v1:" + hex.EncodeToString(digest[:]), nil
}
