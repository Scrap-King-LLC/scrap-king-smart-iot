package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Scrap-King-LLC/scrap-king-smart-iot/internal/signing"
)

var testJPEG = []byte{0xFF, 0xD8, 0xFF, 0xE0, 't', 'e', 's', 't'}

type received struct {
	path   string
	header http.Header
	body   []byte
	scales *report
}

// receiver stands in for the web app, and checks each request the way
// PROTOCOL.md tells the web app to. A request that fails is recorded in bad.
type receiver struct {
	pub ed25519.PublicKey

	mu     sync.Mutex
	reqs   []received
	nonces map[string]bool
	bad    []string
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if problem := rc.verify(r, body); problem != "" {
		rc.bad = append(rc.bad, r.URL.Path+": "+problem)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	got := received{path: r.URL.Path, header: r.Header.Clone(), body: body}
	if strings.HasSuffix(r.URL.Path, "/scales") {
		var rep report
		if err := json.Unmarshal(body, &rep); err == nil {
			got.scales = &rep
		}
	}
	rc.reqs = append(rc.reqs, got)
	w.WriteHeader(http.StatusNoContent)
}

func (rc *receiver) verify(r *http.Request, body []byte) string {
	ts, err := strconv.ParseInt(r.Header.Get(signing.HeaderTimestamp), 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)).Abs() > 5*time.Minute {
		return "timestamp missing or out of range"
	}
	nonce := r.Header.Get(signing.HeaderNonce)
	if nonce == "" || rc.nonces[nonce] {
		return "nonce missing or reused"
	}
	req := signing.Request{
		Path: r.URL.EscapedPath(), AgentID: r.Header.Get(signing.HeaderAgentID), Timestamp: ts,
		Nonce: nonce, Meta: r.Header.Get(signing.HeaderMeta), Body: body,
	}
	if !signing.Verify(rc.pub, req, r.Header.Get(signing.HeaderSignature)) {
		return "bad signature"
	}
	if rc.nonces == nil {
		rc.nonces = map[string]bool{}
	}
	rc.nonces[nonce] = true
	return ""
}

func (rc *receiver) all(suffix string) []received {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	var out []received
	for _, r := range rc.reqs {
		if strings.HasSuffix(r.path, suffix) {
			out = append(out, r)
		}
	}
	return out
}

// fakeScale streams the way the yard's indicators do: zero, a truck rolling on,
// a steady weight with one-graduation flicker, then rolling off.
func fakeScale(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		send := func(line string, n int) {
			for range n {
				if _, err := c.Write([]byte(line)); err != nil {
					return
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
		send("\x02      00LGZ\r\n", 4)
		send("\x02      60LG \r\n", 1) // the lagging flag: called still while rolling on
		send("\x02    1200LGM\r\n", 2)
		send("\x02    2840LGM\r\n", 2)
		for range 6 {
			send("\x02    2840LG \r\n", 2)
			send("\x02    2820LG \r\n", 1)
		}
		send("\x02    1200LGM\r\n", 2)
		send("\x02      00LGZ\r\n", 200)
	}()
	return ln.Addr().String()
}

func TestAgentEndToEnd(t *testing.T) {
	key, err := signing.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	rc := &receiver{pub: key.Public().(ed25519.PublicKey)}
	endpoint := httptest.NewServer(rc)
	defer endpoint.Close()
	cam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(testJPEG) }))
	defer cam.Close()

	off := false
	cfg := &Config{
		AgentID:          "test-yard",
		Endpoint:         endpoint.URL + "/api/iot/",
		KeyFile:          "unused.key",
		HeartbeatSeconds: 1,
		SendEveryMs:      50,
		Steady:           SteadyConfig{SettleSeconds: 0.3, Tolerance: 20, MinCaptureWeight: 500},
		Scales: []ScaleConfig{
			{ID: "inbound-1", Name: "Inbound Scale 1", Address: fakeScale(t), Cameras: []string{"scale-camera"}},
			{ID: "outbound-2", Name: "Outbound Scale 2", Address: "127.0.0.1:1", Enabled: &off},
		},
		Cameras: []CameraConfig{{ID: "scale-camera", Name: "Scale camera", SnapshotURL: cam.URL + "/snap", LiveIntervalSeconds: 0.2}},
	}
	cfg.applyDefaults()
	if err := cfg.validate(false); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	newAgent(cfg, newUplink(cfg, key, false), slog.New(slog.DiscardHandler)).Run(ctx)

	if len(rc.bad) > 0 {
		t.Fatalf("requests the web app would refuse: %v", rc.bad)
	}
	reports := rc.all("/api/iot/scales")
	if len(reports) == 0 {
		t.Fatal("no reports reached the receiver")
	}
	for _, r := range rc.reqs {
		if r.header.Get("X-Agent-Id") != "test-yard" || r.header.Get("Authorization") != "" {
			t.Fatalf("%s sent with the wrong identity: %v", r.path, r.header)
		}
	}

	var sawSteady, sawMotion bool
	for _, r := range reports {
		if r.scales == nil || len(r.scales.Scales) != 2 {
			t.Fatalf("malformed report: %s", r.body)
		}
		in, out := r.scales.Scales[0], r.scales.Scales[1]
		if out.Online || out.Error != "disabled in the agent's config" || out.Weight != nil {
			t.Fatalf("disabled scale reported as %+v", out)
		}
		if in.Online && in.Weight != nil && *in.Weight == 2840 && in.Steady && in.SteadySince != nil {
			sawSteady = true
		}
		if in.Online && in.Status == "motion" && !in.Steady {
			sawMotion = true
		}
		if in.Online && in.Weight != nil && *in.Weight == 60 && in.Steady {
			t.Fatal("the lagging still flag while rolling on must not count as steady")
		}
	}
	if !sawSteady || !sawMotion {
		t.Fatalf("expected both a moving and a steady 2,840 lb report (steady=%v motion=%v)", sawSteady, sawMotion)
	}

	captures := rc.all("/api/iot/captures")
	if len(captures) != 1 {
		t.Fatalf("expected one photo for one steady weight, got %d", len(captures))
	}
	var m captureMeta
	h := captures[0].header
	if err := json.Unmarshal([]byte(h.Get(signing.HeaderMeta)), &m); err != nil {
		t.Fatalf("capture meta %q: %v", h.Get(signing.HeaderMeta), err)
	}
	if m.ScaleID != "inbound-1" || m.CameraID != "scale-camera" || m.Weight != 2840 || m.Unit != "lb" || m.Mode != "gross" ||
		m.CaptureID == "" || m.SteadySince.IsZero() || m.CapturedAt.Before(m.SteadySince) ||
		h.Get("Content-Type") != "image/jpeg" || string(captures[0].body) != string(testJPEG) {
		t.Fatalf("capture sent as %+v %v", m, h)
	}

	live := rc.all("/api/iot/cameras/scale-camera/live")
	if len(live) < 3 {
		t.Fatalf("expected live frames every 0.2s, got %d", len(live))
	}
	var lm liveMeta
	if err := json.Unmarshal([]byte(live[0].header.Get(signing.HeaderMeta)), &lm); err != nil || lm.CameraID != "scale-camera" || lm.CapturedAt.IsZero() {
		t.Fatalf("live frame meta %q: %v", live[0].header.Get(signing.HeaderMeta), err)
	}
}

// The web app refuses a request signed by another key, and the agent says why.
func TestUplinkRefusedSignature(t *testing.T) {
	right, _ := signing.GenerateKey()
	wrong, _ := signing.GenerateKey()
	rc := &receiver{pub: right.Public().(ed25519.PublicKey)}
	srv := httptest.NewServer(rc)
	defer srv.Close()

	cfg := &Config{AgentID: "yard-1", Endpoint: srv.URL} // no path: requests go to /scales
	err := newUplink(cfg, wrong, false).PostJSON(context.Background(), "scales", map[string]int{"n": 1})
	var re *rejectedError
	if !errors.As(err, &re) || re.code != http.StatusUnauthorized || !strings.Contains(err.Error(), "pubkey") || permanent(err) {
		t.Fatalf("wrong key: %v", err)
	}
	if len(rc.bad) != 1 || !strings.HasPrefix(rc.bad[0], "/scales: bad signature") {
		t.Fatalf("receiver saw %v", rc.bad)
	}

	if err := newUplink(cfg, right, false).PostJSON(context.Background(), "scales", map[string]int{"n": 1}); err != nil {
		t.Fatalf("right key, endpoint with no path: %v", err)
	}
}

func TestRejectedErrorNamesClockSkew(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", time.Now().Add(-10*time.Minute).UTC().Format(http.TimeFormat))
		http.Error(w, "stale timestamp", http.StatusUnauthorized)
	}))
	defer srv.Close()
	key, _ := signing.GenerateKey()
	err := newUplink(&Config{AgentID: "yard-1", Endpoint: srv.URL + "/api/iot"}, key, false).PostJSON(context.Background(), "scales", 1)
	// Date has whole seconds only, so the skew reads 10m0s or 10m1s.
	if err == nil || !strings.Contains(err.Error(), "clock is 10m") || !strings.Contains(err.Error(), "ahead of the web app's") ||
		!strings.Contains(err.Error(), "stale timestamp") {
		t.Fatalf("got %v", err)
	}
}

// A redirect is reported with where it points, never followed.
func TestUplinkReportsRedirects(t *testing.T) {
	login := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			login = true
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer srv.Close()
	key, _ := signing.GenerateKey()
	err := newUplink(&Config{AgentID: "yard-1", Endpoint: srv.URL + "/api/iot"}, key, false).PostJSON(context.Background(), "scales", 1)
	if err == nil || login || !strings.Contains(err.Error(), `302 Found to "/login"`) {
		t.Fatalf("redirect: followed=%v, %v", login, err)
	}
}

// Header values are read back as Latin-1 by Node, so X-Meta must be ASCII.
func TestASCIIJSON(t *testing.T) {
	in := map[string]string{"name": "Báscula 1 🚛"}
	b, _ := json.Marshal(in)
	got := asciiJSON(b)
	for i := range len(got) {
		if got[i] >= 0x80 {
			t.Fatalf("non-ASCII byte in %q", got)
		}
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(got), &out); err != nil || out["name"] != in["name"] {
		t.Fatalf("%q decodes to %v, %v", got, out, err)
	}
}

// One failed snapshot does not take a camera offline; three in a row do.
func TestCameraGoesOfflineAfterRepeatedFailures(t *testing.T) {
	cfg := &Config{AgentID: "yard-1", Cameras: []CameraConfig{{ID: "cam", SnapshotURL: "http://127.0.0.1:1/snap"}}}
	cfg.applyDefaults()
	a := newAgent(cfg, nil, slog.New(slog.DiscardHandler))
	cs := a.cameras["cam"]
	online := func() string {
		r := a.report().Cameras[0]
		if r.Online == nil {
			return "unknown"
		}
		return strconv.FormatBool(*r.Online) + " " + r.Error
	}
	fail := errors.New("timeout")
	a.noteCamera(cs, nil)
	for i, want := range []string{"true ", "true ", "false timeout"} {
		a.noteCamera(cs, fail)
		if got := online(); got != want {
			t.Fatalf("after %d failures: %q, want %q", i+1, got, want)
		}
	}
	if a.noteCamera(cs, nil); online() != "true " {
		t.Fatalf("a success did not bring it back: %q", online())
	}
}

func TestPermanentRejections(t *testing.T) {
	for code, want := range map[int]bool{400: true, 413: true, 415: true, 422: true, 401: false, 404: false, 429: false, 500: false, 503: false} {
		if got := permanent(&rejectedError{code: code}); got != want {
			t.Errorf("%d: permanent = %v", code, got)
		}
	}
	if permanent(errors.New("dial tcp: no route to host")) {
		t.Error("a network error is not permanent")
	}
}

func TestKeygenWritesAKeyTheAgentLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.key")
	if err := keygen(path, false); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" { // Windows reports 0666 whatever the ACL says
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("key file %v, %v", fi.Mode(), err)
		}
	}
	first, err := loadKey(path)
	if errors.Is(err, fs.ErrPermission) && runtime.GOOS == "windows" {
		t.Skip("the key is readable only by Administrators and the service: run the tests elevated")
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := keygen(path, false); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("keygen over an existing key: %v", err)
	}
	if err := keygen(path, true); err != nil {
		t.Fatal(err)
	}
	if second, _ := loadKey(path); second.Equal(first) {
		t.Fatal("keygen -force kept the old key")
	}
}

func TestConfigExampleLoads(t *testing.T) {
	cfg, err := loadConfig("config.example.json", false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Scales[0].enabled() || cfg.Scales[0].options().Stale != 3*time.Second {
		t.Fatalf("example config defaults wrong: %+v", cfg.Scales[0])
	}
	wd, _ := os.Getwd()
	if cfg.KeyFile != filepath.Join(wd, "agent.key") {
		t.Fatalf("keyFile resolved to %q, want agent.key beside the config", cfg.KeyFile)
	}
}

func TestConfigRejects(t *testing.T) {
	base := func() *Config {
		return &Config{
			AgentID: "yard-1", Endpoint: "https://example.com/x",
			Scales:  []ScaleConfig{{ID: "a", Address: "10.0.0.1:10001", Cameras: []string{"cam"}}},
			Cameras: []CameraConfig{{ID: "cam", SnapshotURL: "http://10.0.0.2/snap"}},
		}
	}
	cases := map[string]func(*Config){
		"agentId":        func(c *Config) { c.AgentID = "Yard 1" },
		"endpoint":       func(c *Config) { c.Endpoint = "ftp://example.com" },
		"only scheme":    func(c *Config) { c.Endpoint = "https://example.com/x?key=1" },
		"must use https": func(c *Config) { c.Endpoint = "http://example.com/x" },
		"negative":       func(c *Config) { c.Steady.Tolerance = -1 },
		"exactly one of": func(c *Config) { c.Scales[0].Address = "" },
		"not in cameras": func(c *Config) { c.Scales[0].Cameras = []string{"nope"} },
		"used twice":     func(c *Config) { c.Scales = append(c.Scales, c.Scales[0]) },
		"snapshotUrl":    func(c *Config) { c.Cameras[0].SnapshotURL = "" },
		"parity": func(c *Config) {
			c.Scales[0].Address, c.Scales[0].Serial = "", &SerialConfig{Port: "COM5", Parity: "mark"}
		},
	}
	for want, mutate := range cases {
		c := base()
		mutate(c)
		c.applyDefaults()
		if err := c.validate(false); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v", want, err)
		}
	}
	c := base()
	c.applyDefaults()
	if err := c.validate(false); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	c.Endpoint = "http://127.0.0.1:3000/api/iot"
	if err := c.validate(false); err != nil {
		t.Fatalf("http to this PC rejected: %v", err)
	}

	// A dry run needs no endpoint, but one that is set must still parse.
	c.Endpoint = ""
	if err := c.validate(true); err != nil {
		t.Fatalf("dry run without an endpoint: %v", err)
	}
	c.Endpoint = "https://your app/api/iot"
	if err := c.validate(true); err == nil {
		t.Fatal("dry run accepted a malformed endpoint")
	}
}
