package secret

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestEncryptDecrypt(t *testing.T) {
	t.Setenv(environmentKey, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", keySize))))
	encrypted, err := Encrypt("sensitive-token")
	if err != nil {
		t.Fatal(err)
	}
	if encrypted == "sensitive-token" || !IsEncrypted(encrypted) {
		t.Fatalf("token was not encrypted: %q", encrypted)
	}
	decrypted, err := Decrypt(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	if decrypted != "sensitive-token" {
		t.Fatalf("unexpected plaintext: %q", decrypted)
	}
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	t.Setenv(environmentKey, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", keySize))))
	encrypted, err := Encrypt("sensitive-token")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(environmentKey, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("b", keySize))))
	if _, err := Decrypt(encrypted); err == nil {
		t.Fatal("expected wrong-key error")
	}
}

func TestBlindIndexIsKeyedAndPurposeSeparated(t *testing.T) {
	keyA := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", keySize)))
	keyB := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("b", keySize)))
	t.Setenv(environmentKey, keyA)
	first, err := BlindIndex("api-key", "guessable-value")
	if err != nil {
		t.Fatal(err)
	}
	second, err := BlindIndex("api-key", "guessable-value")
	if err != nil {
		t.Fatal(err)
	}
	otherPurpose, err := BlindIndex("other", "guessable-value")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(environmentKey, keyB)
	otherKey, err := BlindIndex("api-key", "guessable-value")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || first == otherPurpose || first == otherKey || strings.Contains(first, "guessable-value") {
		t.Fatalf("blind index lacks deterministic key/purpose separation")
	}
}
