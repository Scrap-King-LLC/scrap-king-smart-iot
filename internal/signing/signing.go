// Package signing proves to the web app that a request came from this agent.
//
// The agent holds an Ed25519 private key that never leaves the yard PC; the web
// app holds only the matching public key, which can check a signature but not
// make one. Every request is signed over its URL path, the agent's id, a
// timestamp, a one-off nonce, its metadata and a hash of its body, so a request
// can be neither altered nor replayed later. PROTOCOL.md is the receiver's side.
package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Scheme is the first line of every signed message. A future change to what is
// signed gets a new scheme, so the two can never be confused.
const Scheme = "scrap-king-smart-iot-v1"

// Headers carrying the signature and what it covers.
const (
	HeaderAgentID   = "X-Agent-Id"
	HeaderTimestamp = "X-Timestamp"
	HeaderNonce     = "X-Nonce"
	HeaderMeta      = "X-Meta"
	HeaderSignature = "X-Signature"
)

// Request is everything a signature covers.
type Request struct {
	Path      string // the request's URL path as sent, e.g. "/api/iot/scales"
	AgentID   string
	Timestamp int64 // Unix seconds
	Nonce     string
	Meta      string // the X-Meta header, verbatim; empty when there is none
	Body      []byte
}

// Message is the exact bytes signed: seven lines joined by "\n", no trailing newline.
func (r Request) Message() []byte {
	sum := sha256.Sum256(r.Body)
	return []byte(strings.Join([]string{
		Scheme,
		r.Path,
		r.AgentID,
		strconv.FormatInt(r.Timestamp, 10),
		r.Nonce,
		r.Meta,
		hex.EncodeToString(sum[:]),
	}, "\n"))
}

// Sign returns the base64 signature for the X-Signature header.
func Sign(key ed25519.PrivateKey, r Request) string {
	return base64.StdEncoding.EncodeToString(ed25519.Sign(key, r.Message()))
}

// Verify reports whether signature is key's signature of r. The web app does
// the same check; this one exists for tests.
func Verify(key ed25519.PublicKey, r Request, signature string) bool {
	sig, err := base64.StdEncoding.DecodeString(signature)
	return err == nil && ed25519.Verify(key, r.Message(), sig)
}

// NewNonce returns 128 random bits as 26 base32 characters.
func NewNonce() string { return rand.Text() }

// Now is the timestamp to sign.
func Now() int64 { return time.Now().Unix() }

// GenerateKey makes a new key pair.
func GenerateKey() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	return priv, err
}

// EncodePrivateKey renders a private key as PEM (PKCS #8), the key file's format.
func EncodePrivateKey(key ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// LoadPrivateKey reads a key file written by EncodePrivateKey.
func LoadPrivateKey(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no key at %s: run `scrap-king-smart-iot keygen` first", path)
	}
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s is not a PEM private key", path)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s holds a %T, not an Ed25519 key", path, parsed)
	}
	return key, nil
}

// PublicKeyString is the public key as the web app's environment variable
// holds it: base64 of the DER-encoded SubjectPublicKeyInfo, one line. Node reads
// it with createPublicKey({ key: Buffer.from(value, "base64"), format: "der", type: "spki" }).
func PublicKeyString(key ed25519.PrivateKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(der), nil
}
