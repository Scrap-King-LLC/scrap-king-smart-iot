package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.bug.st/serial"

	"github.com/Scrap-King-LLC/scrap-king-smart-iot/internal/scale"
)

// Config is config.json. See config.example.json and the README.
type Config struct {
	AgentID  string `json:"agentId"`
	Endpoint string `json:"endpoint"`
	// KeyFile is the private key made by `keygen`. A relative path is taken
	// from the folder holding config.json; loadConfig makes it absolute.
	KeyFile string `json:"keyFile"`

	HeartbeatSeconds float64 `json:"heartbeatSeconds"`
	SendEveryMs      int     `json:"sendEveryMs"`

	Steady  SteadyConfig   `json:"steady"`
	Scales  []ScaleConfig  `json:"scales"`
	Cameras []CameraConfig `json:"cameras"`
}

type SteadyConfig struct {
	SettleSeconds    float64 `json:"settleSeconds"`
	Tolerance        float64 `json:"tolerance"`
	MinCaptureWeight float64 `json:"minCaptureWeight"`
}

type ScaleConfig struct {
	ID      string        `json:"id"`
	Name    string        `json:"name"`
	Enabled *bool         `json:"enabled"`
	Address string        `json:"address"`
	Serial  *SerialConfig `json:"serial"`
	Cameras []string      `json:"cameras"`

	StaleSeconds     float64 `json:"staleSeconds"`
	ReconnectSeconds float64 `json:"reconnectSeconds"`
}

type SerialConfig struct {
	Port     string  `json:"port"`
	BaudRate int     `json:"baudRate"`
	DataBits int     `json:"dataBits"`
	Parity   string  `json:"parity"`
	StopBits float64 `json:"stopBits"`
}

type CameraConfig struct {
	ID                  string  `json:"id"`
	Name                string  `json:"name"`
	SnapshotURL         string  `json:"snapshotUrl"`
	CaptureURL          string  `json:"captureUrl"`
	Username            string  `json:"username"`
	Password            string  `json:"password"`
	LiveIntervalSeconds float64 `json:"liveIntervalSeconds"`
}

func (s ScaleConfig) enabled() bool { return s.Enabled == nil || *s.Enabled }

func (s ScaleConfig) source() scale.Source {
	if s.Serial == nil {
		return scale.TCPSource{Address: s.Address}
	}
	mode := serial.Mode{BaudRate: s.Serial.BaudRate, DataBits: s.Serial.DataBits}
	switch s.Serial.Parity {
	case "even":
		mode.Parity = serial.EvenParity
	case "odd":
		mode.Parity = serial.OddParity
	default:
		mode.Parity = serial.NoParity
	}
	switch s.Serial.StopBits {
	case 1.5:
		mode.StopBits = serial.OnePointFiveStopBits
	case 2:
		mode.StopBits = serial.TwoStopBits
	default:
		mode.StopBits = serial.OneStopBit
	}
	return scale.SerialSource{Port: s.Serial.Port, Mode: mode}
}

func (s ScaleConfig) options() scale.Options {
	return scale.Options{Stale: seconds(s.StaleSeconds), Reconnect: seconds(s.ReconnectSeconds)}
}

func (c CameraConfig) captureURL() string {
	if c.CaptureURL != "" {
		return c.CaptureURL
	}
	return c.SnapshotURL
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

// loadConfig reads, defaults and checks config.json. dryRun relaxes the
// endpoint, which a dry run never uses.
func loadConfig(path string, dryRun bool) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.applyDefaults()
	if !filepath.IsAbs(c.KeyFile) {
		dir, err := filepath.Abs(filepath.Dir(path))
		if err != nil {
			return nil, err
		}
		c.KeyFile = filepath.Join(dir, c.KeyFile)
	}
	if err := c.validate(dryRun); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	def := func(v *float64, d float64) {
		if *v == 0 {
			*v = d
		}
	}
	if c.KeyFile == "" {
		c.KeyFile = defaultKeyFile
	}
	def(&c.HeartbeatSeconds, 5)
	if c.SendEveryMs == 0 {
		c.SendEveryMs = 250
	}
	def(&c.Steady.SettleSeconds, 1)
	def(&c.Steady.Tolerance, 20)
	def(&c.Steady.MinCaptureWeight, 500)
	for i := range c.Scales {
		s := &c.Scales[i]
		def(&s.StaleSeconds, 3)
		def(&s.ReconnectSeconds, 60)
		if s.Name == "" {
			s.Name = s.ID
		}
		if s.Serial != nil {
			if s.Serial.BaudRate == 0 {
				s.Serial.BaudRate = 9600
			}
			if s.Serial.DataBits == 0 {
				s.Serial.DataBits = 8
			}
		}
	}
	for i := range c.Cameras {
		if c.Cameras[i].Name == "" {
			c.Cameras[i].Name = c.Cameras[i].ID
		}
	}
}

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (c *Config) validate(dryRun bool) error {
	var errs []error
	bad := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !idPattern.MatchString(c.AgentID) {
		bad("agentId %q: use lowercase letters, digits, - and _", c.AgentID)
	}
	if !dryRun || c.Endpoint != "" {
		if u, err := url.Parse(c.Endpoint); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			bad("endpoint %q is not an https URL", c.Endpoint)
		} else if u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			bad("endpoint %q: give only scheme, host and path", c.Endpoint)
		} else if u.Scheme == "http" && !isLoopback(u.Hostname()) {
			// The signature stops forgery, not reading: over http anyone on the
			// path sees every weight and photo, and can hold a request back.
			bad("endpoint %q must use https (http is allowed only for this PC itself, for testing)", c.Endpoint)
		}
	}
	if c.HeartbeatSeconds < 1 {
		bad("heartbeatSeconds must be at least 1")
	}
	if c.SendEveryMs < 50 {
		bad("sendEveryMs must be at least 50")
	}
	if c.Steady.SettleSeconds < 0 || c.Steady.Tolerance < 0 || c.Steady.MinCaptureWeight < 0 {
		bad("steady settings cannot be negative")
	}

	cameras := map[string]bool{}
	for _, cam := range c.Cameras {
		if !idPattern.MatchString(cam.ID) {
			bad("camera id %q: use lowercase letters, digits, - and _", cam.ID)
		}
		if cameras[cam.ID] {
			bad("camera id %q is used twice", cam.ID)
		}
		cameras[cam.ID] = true
		checkURL := func(field, raw string) {
			if u, err := url.Parse(raw); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				bad("camera %q: %s %q is not an http(s) URL", cam.ID, field, raw)
			}
		}
		checkURL("snapshotUrl", cam.SnapshotURL)
		if cam.CaptureURL != "" {
			checkURL("captureUrl", cam.CaptureURL)
		}
		if cam.LiveIntervalSeconds < 0 {
			bad("camera %q: liveIntervalSeconds cannot be negative", cam.ID)
		}
	}

	scales := map[string]bool{}
	for _, s := range c.Scales {
		if !idPattern.MatchString(s.ID) {
			bad("scale id %q: use lowercase letters, digits, - and _", s.ID)
		}
		if scales[s.ID] {
			bad("scale id %q is used twice", s.ID)
		}
		scales[s.ID] = true
		if (s.Address == "") == (s.Serial == nil) {
			bad("scale %q: set exactly one of address (host:port) or serial", s.ID)
		}
		if s.Serial != nil {
			if s.Serial.Port == "" {
				bad("scale %q: serial.port is empty", s.ID)
			}
			switch s.Serial.Parity {
			case "", "none", "even", "odd":
			default:
				bad("scale %q: serial.parity must be none, even or odd", s.ID)
			}
			switch s.Serial.StopBits {
			case 0, 1, 1.5, 2:
			default:
				bad("scale %q: serial.stopBits must be 1, 1.5 or 2", s.ID)
			}
		}
		for _, id := range s.Cameras {
			if !cameras[id] {
				bad("scale %q names camera %q, which is not in cameras", s.ID, id)
			}
		}
		if s.StaleSeconds < 0.5 {
			bad("scale %q: staleSeconds must be at least 0.5", s.ID)
		}
		if s.ReconnectSeconds <= s.StaleSeconds {
			bad("scale %q: reconnectSeconds must be longer than staleSeconds", s.ID)
		}
	}
	if len(c.Scales) == 0 && len(c.Cameras) == 0 {
		bad("no scales and no cameras: nothing to do")
	}
	return errors.Join(errs...)
}
