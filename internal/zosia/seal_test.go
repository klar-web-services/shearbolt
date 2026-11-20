package zosia

import (
	"bytes"
	"compress/gzip"
	"crypto/hpke"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"testing"
)

func open(t *testing.T, priv hpke.PrivateKey, secret string, env []byte) ([]byte, error) {
	t.Helper()
	var e Envelope
	if err := json.Unmarshal(env, &e); err != nil {
		t.Fatal(err)
	}
	enc, _ := base64.RawURLEncoding.DecodeString(e.Enc)
	ct, _ := base64.RawURLEncoding.DecodeString(e.Ct)
	r, err := hpke.NewRecipient(enc, priv, kdf, aead, Info(secret))
	if err != nil {
		return nil, err
	}
	z, err := r.Open(nil, ct)
	if err != nil {
		return nil, err
	}
	zr, err := gzip.NewReader(bytes.NewReader(z))
	if err != nil {
		t.Fatal(err)
	}
	return io.ReadAll(zr)
}

func testKey(t *testing.T) (Key, hpke.PrivateKey) {
	t.Helper()
	priv, err := kem.DeriveKeyPair(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParseKey("test:" + base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return k, priv
}

func TestSealOpensOnlyWithTheSameSecret(t *testing.T) {
	k, priv := testKey(t)
	env, err := Seal(k, "zosia_secret_a", []byte(`{"hello":"world"}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := open(t, priv, "zosia_secret_a", env)
	if err != nil || string(got) != `{"hello":"world"}` {
		t.Fatalf("open = %q, %v", got, err)
	}
	if _, err := open(t, priv, "zosia_secret_b", env); err == nil {
		t.Fatal("opened with another secret")
	}
}

func encodeKey(k Key) string { return base64.RawURLEncoding.EncodeToString(k.Public.Bytes()) }

// RFC 9180, Appendix A.2.1, base mode, first encryption (sequence number 0).
func TestRFC9180Vector(t *testing.T) {
	unhex := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	priv, err := kem.NewPrivateKey(unhex("8057991eef8f1f1af18f4a9491d16a1ce333f695d4db8e38da75975c4478e0fb"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := hpke.NewRecipient(unhex("1afa08d3dec047a643885163f1180476fa7ddb54c6a8029ea33f95796bf2ac4a"), priv, kdf, aead, unhex("4f6465206f6e2061204772656369616e2055726e"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Open(unhex("436f756e742d30"), unhex("1c5250d8034ec2b784ba2cfd69dbdb8af406cfe3ff938e131f0def8c8b60b4db21993c62ce81883d2dd1b51a28"))
	if err != nil || !bytes.Equal(got, unhex("4265617574792069732074727574682c20747275746820626561757479")) {
		t.Fatalf("open = %x, %v", got, err)
	}
}
