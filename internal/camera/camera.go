// Package camera fetches still JPEGs from an IP camera's snapshot URL.
package camera

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// maxImage bounds a snapshot so a misbehaving camera cannot exhaust memory.
const maxImage = 20 << 20

// Camera is one snapshot URL with optional credentials. Credentials are sent
// only once the camera asks, in the form it asks for (basic or digest), so a
// password never crosses the network in the clear to a camera that wants digest.
type Camera struct {
	URL      string
	Username string
	Password string
	Client   *http.Client

	mu     sync.Mutex
	basic  bool             // the camera asked for basic authentication
	digest *digestChallenge // reused across snapshots until the camera rejects it
}

// Snapshot fetches one JPEG.
func (c *Camera) Snapshot(ctx context.Context) ([]byte, error) {
	resp, err := c.get(ctx, c.authorization)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && c.Username != "" {
		challenge := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		if err := c.accept(challenge); err != nil {
			return nil, err
		}
		if resp, err = c.get(ctx, c.authorization); err != nil {
			return nil, err
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("camera answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImage+1))
	if err != nil {
		return nil, fmt.Errorf("reading snapshot: %w", err)
	}
	if len(body) > maxImage {
		return nil, fmt.Errorf("snapshot larger than %d MB", maxImage>>20)
	}
	if !bytes.HasPrefix(body, []byte{0xFF, 0xD8}) {
		return nil, fmt.Errorf("camera did not send a JPEG (Content-Type %q)", resp.Header.Get("Content-Type"))
	}
	return body, nil
}

func (c *Camera) get(ctx context.Context, auth func(*http.Request)) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("snapshot URL: %w", unwrapURL(err))
	}
	auth(req)
	client := c.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		// Name the camera by host and path only: some cameras take the
		// password in the query string, and this text reaches logs and reports.
		return nil, fmt.Errorf("GET %s://%s%s: %w", req.URL.Scheme, req.URL.Host, req.URL.Path, unwrapURL(err))
	}
	return resp, nil
}

// unwrapURL drops url.Error's copy of the whole URL, query string included.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// accept records how the camera wants to be authenticated.
func (c *Camera) accept(header string) error {
	scheme, params, _ := strings.Cut(header, " ")
	switch strings.ToLower(scheme) {
	case "basic":
		c.mu.Lock()
		c.basic, c.digest = true, nil
		c.mu.Unlock()
		return nil
	case "digest":
		ch, err := parseDigest(params)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.basic, c.digest = false, ch
		c.mu.Unlock()
		return nil
	default:
		return fmt.Errorf("camera asked for unsupported authentication %q", scheme)
	}
}

// authorization adds credentials, once the camera has said which kind.
func (c *Camera) authorization(req *http.Request) {
	if c.Username == "" {
		return
	}
	c.mu.Lock()
	basic, ch := c.basic, c.digest
	c.mu.Unlock()
	switch {
	case ch != nil:
		req.Header.Set("Authorization", ch.authorize(req.Method, req.URL.RequestURI(), c.Username, c.Password))
	case basic:
		req.SetBasicAuth(c.Username, c.Password)
	}
}
