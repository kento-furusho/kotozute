package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestIdentitiesIncludeApple(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{`[{"providerName":"SignInWithApple","userId":"001.abc"}]`, true},
		{`[{"providerName":"Google","userId":"1"},{"providerName":"SignInWithApple","userId":"2"}]`, true},
		{`[{"providerName":"Google","userId":"1"}]`, false},
		{``, false},
		{`not json`, false},
	}
	for _, c := range cases {
		if got := identitiesIncludeApple(c.raw); got != c.want {
			t.Errorf("identitiesIncludeApple(%q) = %v, want %v", c.raw, got, c.want)
		}
	}
}

// client_secret が Apple の求める形（ES256・r||s・必要なクレーム）になっているか。
func TestAppleClientSecret(t *testing.T) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	pemKey := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))

	now := time.Unix(1_800_000_000, 0)
	jwt, err := appleClientSecret(appleKey{KeyID: "KEY123", PrivateKey: pemKey}, now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		t.Fatalf("want 3 parts, got %d", len(parts))
	}
	enc := base64.RawURLEncoding

	var header map[string]string
	b, _ := enc.DecodeString(parts[0])
	json.Unmarshal(b, &header)
	if header["alg"] != "ES256" || header["kid"] != "KEY123" {
		t.Errorf("header = %v", header)
	}

	var claims map[string]any
	b, _ = enc.DecodeString(parts[1])
	json.Unmarshal(b, &claims)
	if claims["iss"] != appleTeamID || claims["sub"] != appleClientID || claims["aud"] != "https://appleid.apple.com" {
		t.Errorf("claims = %v", claims)
	}
	if claims["iat"].(float64) != float64(now.Unix()) || claims["exp"].(float64) <= claims["iat"].(float64) {
		t.Errorf("iat/exp = %v/%v", claims["iat"], claims["exp"])
	}

	sig, _ := enc.DecodeString(parts[2])
	if len(sig) != 64 {
		t.Fatalf("signature length = %d, want 64", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(&priv.PublicKey, digest[:], r, s) {
		t.Error("signature does not verify")
	}
}

func TestAppleClientSecretRejectsBadKey(t *testing.T) {
	if _, err := appleClientSecret(appleKey{KeyID: "K", PrivateKey: "nope"}, time.Now()); err == nil {
		t.Error("want error for non-PEM key")
	}
}
