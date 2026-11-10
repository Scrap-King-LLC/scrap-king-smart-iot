// Command scrap-king-smart-iot streams truck-scale weights and camera images
// from a yard's local network to the Scrap King Smart web app. See README.md.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"image"
	_ "image/jpeg" // registers the JPEG decoder for `check`
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Scrap-King-LLC/scrap-king-smart-iot/internal/scale"
	"github.com/Scrap-King-LLC/scrap-king-smart-iot/internal/signing"
)

const (
	appName        = "scrap-king-smart-iot"
	defaultKeyFile = "agent.key"
)

// version is set at build time: -ldflags "-X main.version=1.2.3".
var version = "dev"

const usage = `scrap-king-smart-iot %s: streams truck-scale weights and camera images to the web app.

Usage:
  scrap-king-smart-iot keygen  [-key path] [-force]      make this PC's key pair and print the public key
  scrap-king-smart-iot pubkey  [-key path]               print the public key again
  scrap-king-smart-iot check   [-config path]            read each scale and camera once, send one report
  scrap-king-smart-iot run     [-config path] [-dry-run] run in this window (Ctrl+C stops it)
  scrap-king-smart-iot install [-config path]            install the Windows service (starts with Windows)
  scrap-king-smart-iot start | stop | status             control the Windows service
  scrap-king-smart-iot uninstall                         remove the Windows service
  scrap-king-smart-iot version

-config defaults to config.json and -key to agent.key, both next to the program.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Printf(usage, version)
		os.Exit(2)
	}
	cmd := os.Args[1]
	flags := flag.NewFlagSet(appName+" "+cmd, flag.ExitOnError)
	var configPath, keyPath string
	var dryRun, replace bool
	switch cmd {
	case "run", "check", "install":
		flags.StringVar(&configPath, "config", besideProgram("config.json"), "path to config.json")
	case "keygen", "pubkey":
		flags.StringVar(&keyPath, "key", besideProgram(defaultKeyFile), "path to the private key file")
	}
	switch cmd {
	case "run":
		flags.BoolVar(&dryRun, "dry-run", false, "print what would be sent instead of sending it")
	case "keygen":
		flags.BoolVar(&replace, "force", false, "replace an existing key (the web app then needs the new public key)")
	}
	flags.Parse(os.Args[2:])
	if flags.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "%s %s: unexpected %q\n", appName, cmd, flags.Arg(0))
		os.Exit(2)
	}

	var err error
	switch cmd {
	case "keygen":
		err = keygen(keyPath, replace)
	case "pubkey":
		err = pubkey(keyPath)
	case "run":
		if runningAsService() {
			err = runAsService(configPath)
		} else {
			err = runConsole(configPath, dryRun)
		}
	case "check":
		err = check(configPath)
	case "install":
		err = installService(configPath)
	case "uninstall":
		err = uninstallService()
	case "start":
		err = startService()
	case "stop":
		err = stopService()
	case "status":
		var s string
		if s, err = serviceStatus(); err == nil {
			fmt.Println(s)
		}
	case "version":
		fmt.Println(version)
	case "help", "-h", "-help", "--help":
		fmt.Printf(usage, version)
	default:
		fmt.Printf(usage, version)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, appName+":", err)
		os.Exit(1)
	}
}

func besideProgram(name string) string {
	exe, err := os.Executable()
	if err != nil {
		return name
	}
	return filepath.Join(filepath.Dir(exe), name)
}

var errKeyExists = errors.New("the key file already exists")

func keygen(path string, replace bool) error {
	key, err := signing.GenerateKey()
	if err != nil {
		return err
	}
	pemBytes, err := signing.EncodePrivateKey(key)
	if err != nil {
		return err
	}
	if err := writeKeyFile(path, pemBytes, replace); err != nil {
		if errors.Is(err, errKeyExists) {
			return fmt.Errorf("%s already exists: run `%s pubkey` to see its public key, or `%s keygen -force` to replace it (the web app then needs the new public key)", path, appName, appName)
		}
		return err
	}
	pub, err := signing.PublicKeyString(key)
	if err != nil {
		return err
	}
	fmt.Printf("Private key written to %s. It stays on this PC: never copy or share it.\n\nPublic key, for the web app:\n\n%s\n", path, pub)
	return nil
}

func pubkey(path string) error {
	key, err := loadKey(path)
	if err != nil {
		return err
	}
	pub, err := signing.PublicKeyString(key)
	if err != nil {
		return err
	}
	fmt.Println(pub)
	return nil
}

// loadKey reads the private key made by keygen.
func loadKey(path string) (ed25519.PrivateKey, error) {
	key, err := signing.LoadPrivateKey(path)
	if errors.Is(err, fs.ErrPermission) {
		return nil, fmt.Errorf("%w (only Administrators and the service can read the key: run this from an Administrator command prompt)", err)
	}
	return key, err
}

// load reads the config and, unless this is a dry run, the key it names.
func load(configPath string, dryRun bool) (*Config, ed25519.PrivateKey, error) {
	cfg, err := loadConfig(configPath, dryRun)
	if err != nil || dryRun {
		return cfg, nil, err
	}
	key, err := loadKey(cfg.KeyFile)
	if err != nil {
		return nil, nil, err
	}
	return cfg, key, nil
}

func runConsole(configPath string, dryRun bool) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, key, err := load(configPath, dryRun)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	newAgent(cfg, newUplink(cfg, key, dryRun), log).Run(ctx)
	return nil
}

// check reads each scale and camera once, prints what it found, and sends one
// report to the endpoint. It exits non-zero if anything failed.
//
// Reading a scale opens its line for a few seconds, so on a box that allows one
// connection per port (the UDS2100), run it only while nothing else holds the line.
func check(configPath string) error {
	cfg, key, err := load(configPath, false)
	if err != nil {
		return err
	}
	pub, err := signing.PublicKeyString(key)
	if err != nil {
		return err
	}
	a := newAgent(cfg, newUplink(cfg, key, false), slog.New(slog.DiscardHandler))
	a.captures = false
	failed := false
	fmt.Printf("%s %s, agent %q\npublic key %s\n\n", appName, version, cfg.AgentID, pub)

	for _, st := range a.scales {
		label := fmt.Sprintf("scale %-12s %s", st.cfg.ID, st.cfg.source())
		if !st.cfg.enabled() {
			fmt.Printf("  skip  %s (disabled)\n", label)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		var result string
		onEvent := a.onScale(st)
		scale.Run(ctx, st.cfg.source(), st.cfg.options(), func(e scale.Event) {
			onEvent(e)
			if result != "" {
				return
			}
			if e.Frame != nil {
				result = "  ok    " + label + ": " + describe(*e.Frame)
			} else {
				result = "  FAIL  " + label + ": " + e.Err.Error()
				failed = true
			}
			cancel()
		})
		cancel()
		if result == "" {
			result = "  FAIL  " + label + ": nothing within 8s"
			failed = true
		}
		fmt.Println(result)
	}

	for _, cs := range a.camList {
		for _, c := range []struct{ what, url string }{{"live", cs.cfg.SnapshotURL}, {"capture", cs.cfg.captureURL()}} {
			if c.what == "capture" && c.url == cs.cfg.SnapshotURL {
				continue
			}
			cam := cs.live
			if c.what == "capture" {
				cam = cs.still
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			img, err := cam.Snapshot(ctx)
			cancel()
			a.noteCamera(cs, err)
			label := fmt.Sprintf("camera %-11s %s", cs.cfg.ID, c.what)
			if err != nil {
				fmt.Printf("  FAIL  %s: %v\n", label, err)
				failed = true
				continue
			}
			size := ""
			if ic, _, err := image.DecodeConfig(bytes.NewReader(img)); err == nil {
				size = fmt.Sprintf("%dx%d, ", ic.Width, ic.Height)
			}
			fmt.Printf("  ok    %s: %s%d KB\n", label, size, len(img)/1024)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.up.PostJSON(ctx, "scales", a.report()); err != nil {
		fmt.Printf("  FAIL  endpoint: %v\n", err)
		failed = true
	} else {
		fmt.Printf("  ok    endpoint: report accepted\n")
	}

	if failed {
		return errors.New("some checks failed")
	}
	return nil
}
