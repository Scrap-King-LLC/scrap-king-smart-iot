package camera

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"
	"sync"
)

// digestChallenge is a camera's HTTP digest challenge (RFC 7616), kept so the
// nonce can be reused with a rising count instead of a 401 round trip per photo.
type digestChallenge struct {
	realm, nonce, opaque, algorithm string
	qop                             string // "auth" or "" for the legacy RFC 2069 form
	newHash                         func() hash.Hash
	sess                            bool

	mu sync.Mutex
	nc uint32

	cnonce func() string // replaced in tests
}

func parseDigest(params string) (*digestChallenge, error) {
	p := parseParams(params)
	ch := &digestChallenge{realm: p["realm"], nonce: p["nonce"], opaque: p["opaque"], algorithm: p["algorithm"], cnonce: randomCnonce}
	if ch.nonce == "" {
		return nil, errors.New("camera's digest challenge has no nonce")
	}

	alg := strings.ToUpper(ch.algorithm)
	ch.sess = strings.HasSuffix(alg, "-SESS")
	switch strings.TrimSuffix(alg, "-SESS") {
	case "", "MD5":
		ch.newHash = md5.New
	case "SHA-256":
		ch.newHash = sha256.New
	default:
		return nil, fmt.Errorf("camera asked for unsupported digest algorithm %q", ch.algorithm)
	}

	if q := p["qop"]; q != "" {
		for _, opt := range strings.Split(q, ",") {
			if strings.TrimSpace(opt) == "auth" {
				ch.qop = "auth"
			}
		}
		if ch.qop == "" {
			return nil, fmt.Errorf("camera asked for unsupported digest qop %q", q)
		}
	}
	return ch, nil
}

func (ch *digestChallenge) h(parts ...string) string {
	h := ch.newHash()
	h.Write([]byte(strings.Join(parts, ":")))
	return hex.EncodeToString(h.Sum(nil))
}

func (ch *digestChallenge) authorize(method, uri, username, password string) string {
	ch.mu.Lock()
	ch.nc++
	nc := fmt.Sprintf("%08x", ch.nc)
	ch.mu.Unlock()
	cnonce := ch.cnonce()

	ha1 := ch.h(username, ch.realm, password)
	if ch.sess {
		ha1 = ch.h(ha1, ch.nonce, cnonce)
	}
	ha2 := ch.h(method, uri)

	var response string
	if ch.qop == "" {
		response = ch.h(ha1, ch.nonce, ha2)
	} else {
		response = ch.h(ha1, ch.nonce, nc, cnonce, ch.qop, ha2)
	}

	var b strings.Builder
	fmt.Fprintf(&b, `Digest username=%q, realm=%q, nonce=%q, uri=%q, response=%q`, username, ch.realm, ch.nonce, uri, response)
	if ch.algorithm != "" {
		fmt.Fprintf(&b, ", algorithm=%s", ch.algorithm)
	}
	if ch.qop != "" {
		fmt.Fprintf(&b, ", qop=%s, nc=%s, cnonce=%q", ch.qop, nc, cnonce)
	}
	if ch.opaque != "" {
		fmt.Fprintf(&b, ", opaque=%q", ch.opaque)
	}
	return b.String()
}

func randomCnonce() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// parseParams reads `key="value", key=value` pairs, where a quoted value may
// itself contain commas (qop="auth,auth-int").
func parseParams(s string) map[string]string {
	out := map[string]string{}
	for s = strings.TrimSpace(s); s != ""; s = strings.TrimSpace(s) {
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = strings.TrimSpace(s[eq+1:])
		var val string
		if strings.HasPrefix(s, `"`) {
			end := 1
			for end < len(s) && s[end] != '"' {
				if s[end] == '\\' {
					end++
				}
				end++
			}
			val = strings.ReplaceAll(s[1:min(end, len(s))], `\"`, `"`)
			s = s[min(end+1, len(s)):]
		} else {
			comma := strings.IndexByte(s, ',')
			if comma < 0 {
				comma = len(s)
			}
			val = strings.TrimSpace(s[:comma])
			s = s[comma:]
		}
		out[key] = val
		s = strings.TrimPrefix(strings.TrimSpace(s), ",")
	}
	return out
}
