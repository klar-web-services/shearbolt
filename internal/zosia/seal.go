package zosia

import (
	"bytes"
	"compress/gzip"
	"crypto/ecdh"
	"crypto/hpke"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const infoLabel = "zosia/report/v1"

var (
	kem  = hpke.DHKEM(ecdh.X25519())
	kdf  = hpke.HKDFSHA256()
	aead = hpke.ChaCha20Poly1305()
)

type Key struct {
	ID     string
	Public hpke.PublicKey
}

// ParseKey reads "<kid>:<base64url X25519 public key>".
func ParseKey(s string) (Key, error) {
	kid, b64, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || kid == "" || b64 == "" {
		return Key{}, errors.New("a Zosia key has the form <key id>:<base64url public key>")
	}
	raw, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil || len(raw) != 32 {
		return Key{}, errors.New("a Zosia public key is 32 bytes in unpadded base64url")
	}
	pk, err := kem.NewPublicKey(raw)
	if err != nil {
		return Key{}, fmt.Errorf("Zosia public key: %v", err)
	}
	return Key{ID: kid, Public: pk}, nil
}

// Info binds a sealed report to the secret that sends it: Zosia opens it only with the
// same secret's hash, so a sealed report replayed under another account does not open.
func Info(secret string) []byte {
	sum := sha256.Sum256([]byte(secret))
	return append([]byte(infoLabel+"\x00"), hex.EncodeToString(sum[:])...)
}

type Envelope struct {
	V   int    `json:"v"`
	Kid string `json:"kid"`
	Enc string `json:"enc"`
	Ct  string `json:"ct"`
}

// Seal gzips payload and seals it to key, bound to secret, returning the envelope JSON.
func Seal(key Key, secret string, payload []byte) ([]byte, error) {
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	if _, err := zw.Write(payload); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	enc, sender, err := hpke.NewSender(key.Public, kdf, aead, Info(secret))
	if err != nil {
		return nil, err
	}
	ct, err := sender.Seal(nil, zipped.Bytes())
	if err != nil {
		return nil, err
	}
	b64 := base64.RawURLEncoding.EncodeToString
	return json.Marshal(Envelope{V: 1, Kid: key.ID, Enc: b64(enc), Ct: b64(ct)})
}
