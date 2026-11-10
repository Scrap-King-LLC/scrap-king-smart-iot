//go:build windows

package main

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName        = "ScrapKingSmartIoT"
	serviceDisplayName = "Scrap King Smart IoT"
	serviceDescription = "Streams truck-scale weights and camera images from this yard to the Scrap King Smart web app."
	// serviceAccount is the account the service runs as: it can reach the
	// network and little else. keyFileSDDL grants it read access to the key.
	serviceAccount = `NT AUTHORITY\LocalService`
)

func runningAsService() bool {
	ok, err := svc.IsWindowsService()
	return err == nil && ok
}

// runAsService is what the Service Control Manager starts:
// `scrap-king-smart-iot.exe run -config <path>`, registered by installService.
func runAsService(configPath string) error {
	elog, err := eventlog.Open(serviceName)
	if err != nil {
		return err
	}
	defer elog.Close()
	log := slog.New(&eventLogHandler{el: elog})
	return svc.Run(serviceName, &service{configPath: configPath, log: log})
}

type service struct {
	configPath string
	log        *slog.Logger
}

func (s *service) Execute(_ []string, r <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if cfg, key := s.loadUntilValid(ctx); cfg != nil {
			newAgent(cfg, newUplink(cfg, key, false), s.log).Run(ctx)
		}
	}()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for c := range r {
		switch c.Cmd {
		case svc.Interrogate:
			status <- c.CurrentStatus
		case svc.Stop, svc.Shutdown:
			status <- svc.Status{State: svc.StopPending}
			cancel()
			select {
			case <-done:
			case <-time.After(15 * time.Second):
				s.log.Warn("agent.stop_timeout")
			}
			return false, 0
		}
	}
	return false, 0
}

// loadUntilValid loads the config and key, and while that fails, logs why and
// tries again every minute, so a file fixed later is picked up without anyone
// restarting the service. It returns nil only when ctx ends.
func (s *service) loadUntilValid(ctx context.Context) (*Config, ed25519.PrivateKey) {
	last := ""
	for {
		cfg, key, err := load(s.configPath, false)
		if err == nil {
			if last != "" {
				s.log.Info("config.loaded", "path", s.configPath)
			}
			return cfg, key
		}
		if err.Error() != last {
			last = err.Error()
			s.log.Error("config.invalid", "path", s.configPath, "error", last, "retry", "every minute")
		}
		select {
		case <-ctx.Done():
			return nil, nil
		case <-time.After(time.Minute):
		}
	}
}

func installService(configPath string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if configPath, err = filepath.Abs(configPath); err != nil {
		return err
	}
	if _, _, err := load(configPath, false); err != nil {
		return fmt.Errorf("not installing until this is fixed: %w", err)
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("%w (run this from an Administrator command prompt)", err)
	}
	defer m.Disconnect()
	if s, err := m.OpenService(serviceName); err == nil {
		s.Close()
		return fmt.Errorf("service %s is already installed; run uninstall first", serviceName)
	}

	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName:      serviceDisplayName,
		Description:      serviceDescription,
		StartType:        mgr.StartAutomatic,
		ServiceStartName: serviceAccount,
	}, "run", "-config", configPath)
	if err != nil {
		return err
	}
	defer s.Close()

	// Restart after 5 s, then 30 s, then every minute; forget past failures after a day.
	actions := []mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
		{Type: mgr.ServiceRestart, Delay: time.Minute},
	}
	if err := s.SetRecoveryActions(actions, uint32((24 * time.Hour).Seconds())); err != nil {
		s.Delete()
		return fmt.Errorf("setting restart-on-failure: %w", err)
	}
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		s.Delete()
		return fmt.Errorf("setting restart-on-failure: %w", err)
	}
	if err := eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil &&
		!strings.Contains(err.Error(), "exists") {
		s.Delete()
		return fmt.Errorf("registering the event log source: %w", err)
	}
	fmt.Printf("Installed the %s service (starts with Windows, runs as %s).\nConfig: %s\nStart it now with: %s start\n", serviceDisplayName, serviceAccount, configPath, appName)
	return nil
}

func uninstallService() error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("%w (run this from an Administrator command prompt)", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %s is not installed", serviceName)
	}
	defer s.Close()
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		if err := control(s, svc.Stop, svc.Stopped); err != nil {
			return err
		}
	}
	if err := s.Delete(); err != nil {
		return err
	}
	eventlog.Remove(serviceName)
	fmt.Printf("Removed %s.\n", serviceName)
	return nil
}

func startService() error {
	return withService(func(s *mgr.Service) error {
		if err := s.Start(); err != nil {
			return err
		}
		return waitFor(s, svc.Running)
	})
}

func stopService() error {
	return withService(func(s *mgr.Service) error { return control(s, svc.Stop, svc.Stopped) })
}

func serviceStatus() (string, error) {
	var out string
	err := withService(func(s *mgr.Service) error {
		st, err := s.Query()
		if err != nil {
			return err
		}
		out = stateName(st.State)
		return nil
	})
	return out, err
}

func withService(f func(*mgr.Service) error) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("%w (run this from an Administrator command prompt)", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %s is not installed", serviceName)
	}
	defer s.Close()
	return f(s)
}

func control(s *mgr.Service, c svc.Cmd, want svc.State) error {
	if _, err := s.Control(c); err != nil {
		return err
	}
	return waitFor(s, want)
}

func waitFor(s *mgr.Service, want svc.State) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		st, err := s.Query()
		if err != nil {
			return err
		}
		if st.State == want {
			return nil
		}
		if want == svc.Running && st.State == svc.Stopped {
			return errors.New("the service stopped as it started: see the Application log in Event Viewer")
		}
		if time.Now().After(deadline) {
			return errors.New("service is still " + stateName(st.State) + " after 20s")
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func stateName(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "resuming"
	case svc.PausePending:
		return "pausing"
	case svc.Paused:
		return "paused"
	default:
		return fmt.Sprintf("state %d", s)
	}
}

// eventLogHandler writes slog records to the Windows Event Log (Application),
// as `event key=value ...`, at the matching level.
type eventLogHandler struct {
	el    *eventlog.Log
	attrs []slog.Attr
}

func (h *eventLogHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }

func (h *eventLogHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	write := func(a slog.Attr) bool {
		v := a.Value.String()
		if strings.ContainsAny(v, " \"=") {
			v = fmt.Sprintf("%q", v)
		}
		fmt.Fprintf(&b, " %s=%s", a.Key, v)
		return true
	}
	for _, a := range h.attrs {
		write(a)
	}
	r.Attrs(write)
	msg := b.String()
	switch {
	case r.Level >= slog.LevelError:
		return h.el.Error(1, msg)
	case r.Level >= slog.LevelWarn:
		return h.el.Warning(1, msg)
	default:
		return h.el.Info(1, msg)
	}
}

func (h *eventLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &eventLogHandler{el: h.el, attrs: append(append([]slog.Attr{}, h.attrs...), attrs...)}
}

func (h *eventLogHandler) WithGroup(string) slog.Handler { return h }
