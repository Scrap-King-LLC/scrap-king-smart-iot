package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Scrap-King-LLC/scrap-king-smart-iot/internal/signing"
)

// Uplink posts to the web app at Config.Endpoint, signing every request with
// this PC's private key. PROTOCOL.md describes what the web app receives.
type Uplink struct {
	base    *url.URL
	key     ed25519.PrivateKey // nil only in a dry run, which sends nothing
	agentID string
	client  *http.Client
	dryRun  bool
}

func newUplink(c *Config, key ed25519.PrivateKey, dryRun bool) *Uplink {
	base, err := url.Parse(c.Endpoint) // validated by loadConfig, but a dry run may leave it empty
	if err != nil {
		base = &url.URL{}
	}
	if base.Path == "" {
		// Otherwise JoinPath yields "scales", signed as such, while "/scales" is sent.
		base.Path = "/"
	}
	return &Uplink{
		base:    base,
		key:     key,
		agentID: c.AgentID,
		client: &http.Client{
			Timeout: 30 * time.Second,
			// A redirect is reported, never followed: following one turns the
			// POST into a GET (301/302), or answers from a login page that says
			// 200 while the data goes nowhere, or changes the signed path.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		dryRun: dryRun,
	}
}

// PostJSON sends v as the body of {endpoint}/{route}.
func (u *Uplink) PostJSON(ctx context.Context, route string, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return u.post(ctx, route, "application/json", body, "")
}

// PostImage sends a JPEG to {endpoint}/{route}, with meta as JSON in X-Meta.
func (u *Uplink) PostImage(ctx context.Context, route string, jpeg []byte, meta any) error {
	m, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return u.post(ctx, route, "image/jpeg", jpeg, asciiJSON(m))
}

// asciiJSON escapes every non-ASCII character in JSON as \uXXXX. A header is
// read back as Latin-1 by Node, so a raw UTF-8 byte would change the signed
// text; the escaped JSON parses to the same value.
func asciiJSON(b []byte) string {
	var s strings.Builder
	for _, r := range string(b) {
		switch {
		case r < utf8.RuneSelf:
			s.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&s, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&s, `\u%04x`, r)
		}
	}
	return s.String()
}

func (u *Uplink) post(ctx context.Context, route, contentType string, body []byte, meta string) error {
	target := u.base.JoinPath(route)
	if u.dryRun {
		if contentType == "application/json" {
			fmt.Printf("POST %s %s\n", target.Path, body)
		} else {
			fmt.Printf("POST %s %s %d bytes %s\n", target.Path, contentType, len(body), meta)
		}
		return nil
	}

	// Signed per attempt, so a photo retried twenty minutes later still carries
	// a current timestamp and a nonce the web app has not seen.
	signed := signing.Request{
		Path:      target.EscapedPath(),
		AgentID:   u.agentID,
		Timestamp: signing.Now(),
		Nonce:     signing.NewNonce(),
		Meta:      meta,
		Body:      body,
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("User-Agent", appName+"/"+version)
	req.Header.Set(signing.HeaderAgentID, signed.AgentID)
	req.Header.Set(signing.HeaderTimestamp, strconv.FormatInt(signed.Timestamp, 10))
	req.Header.Set(signing.HeaderNonce, signed.Nonce)
	if meta != "" {
		req.Header.Set(signing.HeaderMeta, meta)
	}
	req.Header.Set(signing.HeaderSignature, signing.Sign(u.key, signed))

	resp, err := u.client.Do(req)
	if err != nil {
		// url.Error repeats the whole URL; the path is enough.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("POST %s: %w", target.Path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	return &rejectedError{
		path:     target.Path,
		code:     resp.StatusCode,
		status:   resp.Status,
		body:     strings.Join(strings.Fields(string(snippet)), " "),
		location: resp.Header.Get("Location"),
		skew:     clockSkew(resp.Header.Get("Date")),
	}
}

// rejectedError is a non-2xx answer from the web app.
type rejectedError struct {
	path, status, body, location string
	code                         int
	skew                         time.Duration // this PC's clock minus the web app's, when known
}

func (e *rejectedError) Error() string {
	msg := fmt.Sprintf("POST %s: the web app answered %s", e.path, e.status)
	if e.code/100 == 3 {
		return msg + fmt.Sprintf(" to %q: set endpoint to the address it redirects to", e.location)
	}
	if e.body != "" {
		msg += ": " + e.body
	}
	if e.code == http.StatusUnauthorized {
		if e.skew > time.Minute || e.skew < -time.Minute {
			ahead := "ahead of"
			if e.skew < 0 {
				ahead = "behind"
			}
			msg += fmt.Sprintf(" (this PC's clock is %s %s the web app's: correct the clock)", e.skew.Abs().Round(time.Second), ahead)
		} else {
			msg += " (is this PC's public key, from `" + appName + " pubkey`, set in the web app?)"
		}
	}
	return msg
}

// permanent reports whether sending the same request again cannot help: the
// web app understood it and refused what it says. Anything else (no network,
// 5xx, 401 while the public key is being set up, 404 before the web app is
// deployed) is worth retrying.
func permanent(err error) bool {
	var re *rejectedError
	if !errors.As(err, &re) {
		return false
	}
	switch re.code {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

func clockSkew(date string) time.Duration {
	t, err := http.ParseTime(date)
	if err != nil {
		return 0
	}
	return time.Since(t)
}
