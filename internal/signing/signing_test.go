package signing

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignVerifyAndTamper(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub := key.Public().(ed25519.PublicKey)
	r := Request{Path: "/api/iot/captures", AgentID: "yard-1", Timestamp: 1794340000, Nonce: "00ff", Meta: `{"weight":2840}`, Body: []byte("jpeg")}
	sig := Sign(key, r)
	if !Verify(pub, r, sig) {
		t.Fatal("a fresh signature does not verify")
	}

	tampered := []func(*Request){
		func(r *Request) { r.Path = "/api/iot/scales" },
		func(r *Request) { r.AgentID = "yard-2" },
		func(r *Request) { r.Timestamp++ },
		func(r *Request) { r.Nonce = "00fe" },
		func(r *Request) { r.Meta = `{"weight":2860}` },
		func(r *Request) { r.Body = []byte("jpeh") },
	}
	for i, change := range tampered {
		c := r
		change(&c)
		if Verify(pub, c, sig) {
			t.Errorf("change %d still verifies", i)
		}
	}

	other, _ := GenerateKey()
	if Verify(other.Public().(ed25519.PublicKey), r, sig) {
		t.Error("another key's public half verified the signature")
	}
}

func TestMessageLayout(t *testing.T) {
	r := Request{Path: "/api/iot/scales", AgentID: "yard-1", Timestamp: 42, Nonce: "ab", Body: []byte("")}
	want := "scrap-king-smart-iot-v1\n/api/iot/scales\nyard-1\n42\nab\n\n" +
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" // sha256 of nothing
	if got := string(r.Message()); got != want {
		t.Fatalf("message\n%q\nwant\n%q", got, want)
	}
}

func TestKeyFileRoundTrip(t *testing.T) {
	key, _ := GenerateKey()
	pemBytes, err := EncodePrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "agent.key")
	os.WriteFile(path, pemBytes, 0o600)

	loaded, err := LoadPrivateKey(path)
	if err != nil || !loaded.Equal(key) {
		t.Fatalf("LoadPrivateKey = %v, %v", loaded, err)
	}

	s, err := PublicKeyString(key)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := base64.StdEncoding.DecodeString(s)
	pub, err := x509.ParsePKIXPublicKey(der)
	if err != nil || !pub.(ed25519.PublicKey).Equal(key.Public()) {
		t.Fatalf("public key string does not round-trip: %v", err)
	}
	if !strings.HasPrefix(s, "MCowBQYDK2VwAyEA") { // the fixed SPKI header for Ed25519
		t.Errorf("unexpected public key encoding %q", s)
	}
}

func TestLoadPrivateKeyErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadPrivateKey(filepath.Join(dir, "missing.key")); err == nil || !strings.Contains(err.Error(), "keygen") {
		t.Errorf("missing key: %v", err)
	}
	bad := filepath.Join(dir, "bad.key")
	os.WriteFile(bad, []byte("not a key"), 0o600)
	if _, err := LoadPrivateKey(bad); err == nil {
		t.Error("garbage loaded as a key")
	}
}
