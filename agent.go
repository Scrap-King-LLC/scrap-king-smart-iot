package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Scrap-King-LLC/scrap-king-smart-iot/internal/camera"
	"github.com/Scrap-King-LLC/scrap-king-smart-iot/internal/scale"
)

// Agent reads every configured scale and camera and keeps the web app told.
type Agent struct {
	cfg *Config
	up  *Uplink
	log *slog.Logger

	ctx      context.Context // set by Run; captures outlive the event that starts them
	captures bool            // false during `check`, which must not upload photos

	mu      sync.Mutex
	scales  []*scaleState
	cameras map[string]*cameraState
	camList []*cameraState // config order, for reports
	changed chan struct{}

	uploads *captureQueue
}

type scaleState struct {
	cfg     ScaleConfig
	online  bool
	err     string
	frame   *scale.Frame
	readAt  time.Time
	steady  bool
	since   time.Time
	tracker scale.Steady
	// photographed is the start of the steady run already sent to the cameras,
	// so a truck parked for ten minutes is photographed once.
	photographed time.Time
}

type cameraState struct {
	cfg         CameraConfig
	live        *camera.Camera
	still       *camera.Camera
	online      *bool
	err         string
	lastImageAt time.Time
	fails       int // snapshots failed in a row
	liveFails   int // live uploads failed in a row; liveLoop's alone
}

// failuresToReport is how many failures in a row turn a camera offline, or
// get an upload problem logged. One slow answer is not an outage, and a link
// that fails now and then must not fill the event log.
const failuresToReport = 3

func newAgent(cfg *Config, up *Uplink, log *slog.Logger) *Agent {
	a := &Agent{
		cfg:      cfg,
		up:       up,
		log:      log,
		ctx:      context.Background(),
		captures: true,
		cameras:  map[string]*cameraState{},
		changed:  make(chan struct{}, 1),
		uploads:  newCaptureQueue(captureQueueSize),
	}
	settle := seconds(cfg.Steady.SettleSeconds)
	for _, sc := range cfg.Scales {
		st := &scaleState{cfg: sc, err: "not read yet"}
		if !sc.enabled() {
			st.err = "disabled in the agent's config"
		}
		st.tracker = scale.Steady{Settle: settle, Tolerance: cfg.Steady.Tolerance}
		a.scales = append(a.scales, st)
	}
	for _, cc := range cfg.Cameras {
		client := &http.Client{Timeout: 10 * time.Second}
		cs := &cameraState{
			cfg:   cc,
			live:  &camera.Camera{URL: cc.SnapshotURL, Username: cc.Username, Password: cc.Password, Client: client},
			still: &camera.Camera{URL: cc.captureURL(), Username: cc.Username, Password: cc.Password, Client: client},
		}
		a.cameras[cc.ID] = cs
		a.camList = append(a.camList, cs)
	}
	return a
}

// Run works until ctx ends.
func (a *Agent) Run(ctx context.Context) {
	a.ctx = ctx
	a.log.Info("agent.start", "version", version, "agent", a.cfg.AgentID, "scales", len(a.cfg.Scales), "cameras", len(a.cfg.Cameras))

	var wg sync.WaitGroup
	for _, st := range a.scales {
		if !st.cfg.enabled() {
			a.log.Warn("scale.disabled", "scale", st.cfg.ID)
			continue
		}
		wg.Go(func() { scale.Run(ctx, st.cfg.source(), st.cfg.options(), a.onScale(st)) })
	}
	for _, cs := range a.camList {
		if cs.cfg.LiveIntervalSeconds > 0 {
			wg.Go(func() { a.liveLoop(ctx, cs) })
		}
	}
	wg.Go(func() { a.sendLoop(ctx) })
	wg.Go(func() { a.uploadLoop(ctx) })
	wg.Wait()

	if n := a.uploads.len(); n > 0 {
		a.log.Warn("agent.stop", "unsent_photos", n)
	} else {
		a.log.Info("agent.stop")
	}
}

func (a *Agent) signal() {
	select {
	case a.changed <- struct{}{}:
	default:
	}
}

// onScale folds one event from a scale into its state. Called from that
// scale's reader goroutine, up to twenty times a second.
func (a *Agent) onScale(st *scaleState) func(scale.Event) {
	return func(e scale.Event) {
		a.mu.Lock()
		defer a.mu.Unlock()

		if e.Frame == nil {
			msg := "unknown error"
			if e.Err != nil {
				msg = e.Err.Error()
			}
			if msg != st.err || st.online {
				a.log.Warn("scale.unavailable", "scale", st.cfg.ID, "error", msg)
			}
			changed := st.online || msg != st.err
			st.online, st.err, st.frame, st.steady, st.since = false, msg, nil, false, time.Time{}
			st.tracker.Reset()
			if changed {
				a.signal()
			}
			return
		}

		f := *e.Frame
		if !st.online {
			a.log.Info("scale.online", "scale", st.cfg.ID, "source", st.cfg.source().String())
		}
		steady, since := st.tracker.Observe(f, e.At)
		changed := !st.online || st.frame == nil || *st.frame != f || steady != st.steady
		st.online, st.err, st.frame, st.readAt, st.steady = true, "", &f, e.At, steady
		st.since = time.Time{}
		if steady {
			st.since = since
		}

		if steady && since != st.photographed && f.Weight >= a.cfg.Steady.MinCaptureWeight && a.captures {
			st.photographed = since
			for _, id := range st.cfg.Cameras {
				go a.capture(st.cfg.ID, a.cameras[id], f, since)
			}
		}
		if changed {
			a.signal()
		}
	}
}

// sendLoop posts the full state of every scale and camera shortly after
// anything changes, and every heartbeat regardless. Each post replaces the last,
// so a lost one costs nothing and nothing ever queues up.
func (a *Agent) sendLoop(ctx context.Context) {
	every := time.Duration(a.cfg.SendEveryMs) * time.Millisecond
	heartbeat := time.NewTimer(0)
	defer heartbeat.Stop()
	var coalesce <-chan time.Time
	fails, connected := 0, false

	send := func() {
		sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := a.up.PostJSON(sctx, "scales", a.report())
		cancel()
		switch {
		case ctx.Err() != nil:
		case err != nil:
			fails++
			if fails == failuresToReport {
				a.log.Warn("uplink.failed", "error", err.Error())
			}
		case !connected:
			a.log.Info("uplink.connected")
			fails, connected = 0, true
		default:
			if fails >= failuresToReport {
				a.log.Info("uplink.restored")
			}
			fails = 0
		}
		heartbeat.Reset(seconds(a.cfg.HeartbeatSeconds))
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-a.changed:
			if coalesce == nil {
				coalesce = time.After(every)
			}
		case <-coalesce:
			coalesce = nil
			send()
		case <-heartbeat.C:
			send()
		}
	}
}

// report is the body of POST {endpoint}/scales.
type report struct {
	AgentID string         `json:"agentId"`
	Version string         `json:"version"`
	SentAt  time.Time      `json:"sentAt"`
	Scales  []scaleReport  `json:"scales"`
	Cameras []cameraReport `json:"cameras"`
}

type scaleReport struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Online      bool       `json:"online"`
	Error       string     `json:"error,omitempty"`
	Weight      *float64   `json:"weight"`
	Unit        string     `json:"unit,omitempty"`
	Mode        string     `json:"mode,omitempty"`
	Status      string     `json:"status,omitempty"`
	Steady      bool       `json:"steady"`
	SteadySince *time.Time `json:"steadySince,omitempty"`
	ReadAt      *time.Time `json:"readAt,omitempty"`
}

type cameraReport struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Online      *bool      `json:"online"`
	Error       string     `json:"error,omitempty"`
	LastImageAt *time.Time `json:"lastImageAt,omitempty"`
}

func (a *Agent) report() report {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := report{AgentID: a.cfg.AgentID, Version: version, SentAt: stamp(time.Now()), Scales: []scaleReport{}, Cameras: []cameraReport{}}
	for _, st := range a.scales {
		sr := scaleReport{ID: st.cfg.ID, Name: st.cfg.Name, Online: st.online, Error: st.err}
		if st.online && st.frame != nil {
			w := st.frame.Weight
			sr.Weight, sr.Unit, sr.Mode, sr.Status, sr.Steady = &w, st.frame.Unit, st.frame.Mode, string(st.frame.Status), st.steady
			sr.ReadAt = utc(st.readAt)
			if st.steady {
				sr.SteadySince = utc(st.since)
			}
		}
		r.Scales = append(r.Scales, sr)
	}
	for _, cs := range a.camList {
		r.Cameras = append(r.Cameras, cameraReport{ID: cs.cfg.ID, Name: cs.cfg.Name, Online: cs.online, Error: cs.err, LastImageAt: utc(cs.lastImageAt)})
	}
	return r
}

// stamp is how every time is sent: UTC, to the millisecond, which is all a
// JavaScript Date keeps. A steadySince then compares equal in the report and
// on its photo, however the web app parses them.
func stamp(t time.Time) time.Time { return t.UTC().Truncate(time.Millisecond) }

// utc is stamp for an optional time: nil when unset.
func utc(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := stamp(t)
	return &u
}

// noteCamera records whether the camera just answered. A camera that has
// answered before goes offline only after failuresToReport failures in a row.
func (a *Agent) noteCamera(cs *cameraState, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	was := cs.online
	if err == nil {
		cs.fails, cs.err, cs.lastImageAt = 0, "", time.Now()
		if was == nil || !*was {
			a.log.Info("camera.online", "camera", cs.cfg.ID)
			cs.online = ptr(true)
			a.signal()
		}
		return
	}
	cs.fails++
	if was != nil && *was && cs.fails < failuresToReport {
		return
	}
	if was == nil || *was {
		a.log.Warn("camera.unavailable", "camera", cs.cfg.ID, "error", err.Error())
		cs.online = ptr(false)
		a.signal()
	}
	if cs.err != err.Error() {
		cs.err = err.Error()
		a.signal()
	}
}

// ptr returns a pointer to a fresh copy of v. Reports keep the pointer after
// the lock is released, so the value behind it must never change.
func ptr[T any](v T) *T { return &v }

// liveLoop sends one frame per interval. A slow camera or receiver skips
// frames rather than queueing them: live video is only worth its newest frame.
func (a *Agent) liveLoop(ctx context.Context, cs *cameraState) {
	t := time.NewTicker(seconds(cs.cfg.LiveIntervalSeconds))
	defer t.Stop()
	path := "cameras/" + cs.cfg.ID + "/live"
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		at := time.Now()
		img, err := cs.live.Snapshot(fctx)
		if ctx.Err() != nil {
			cancel()
			return
		}
		a.noteCamera(cs, err)
		if err == nil {
			err = a.up.PostImage(fctx, path, img, liveMeta{CameraID: cs.cfg.ID, CapturedAt: stamp(at)})
			switch {
			case ctx.Err() != nil:
			case err != nil:
				cs.liveFails++
				if cs.liveFails == failuresToReport {
					a.log.Warn("live.upload_failed", "camera", cs.cfg.ID, "error", err.Error())
				}
			default:
				if cs.liveFails >= failuresToReport {
					a.log.Info("live.upload_restored", "camera", cs.cfg.ID)
				}
				cs.liveFails = 0
			}
		}
		cancel()
	}
}

// liveMeta is the X-Meta of a live frame.
type liveMeta struct {
	CameraID   string    `json:"cameraId"`
	CapturedAt time.Time `json:"capturedAt"`
}

// captureMeta is the X-Meta of a photo taken when a scale's weight turned steady.
type captureMeta struct {
	CaptureID   string    `json:"captureId"` // the same on every retry
	ScaleID     string    `json:"scaleId"`
	CameraID    string    `json:"cameraId"`
	Weight      float64   `json:"weight"`
	Unit        string    `json:"unit"`
	Mode        string    `json:"mode"`
	SteadySince time.Time `json:"steadySince"`
	CapturedAt  time.Time `json:"capturedAt"`
}

// pendingCapture is a photo waiting to upload.
type pendingCapture struct {
	meta    captureMeta
	jpeg    []byte
	takenAt time.Time // with its monotonic reading, so a clock correction cannot age it
}

// captureAttempts is how many times a photo is tried, a second apart, before
// it is given up on: the truck is usually still on the scale.
const captureAttempts = 3

// capture takes the photo now, while the truck is still on the scale, and
// leaves the upload to uploadLoop, which may have to wait for the internet.
func (a *Agent) capture(scaleID string, cs *cameraState, f scale.Frame, since time.Time) {
	var img []byte
	var err error
	var takenAt time.Time
	for attempt := range captureAttempts {
		if attempt > 0 {
			sleepCtx(a.ctx, time.Second)
		}
		ctx, cancel := context.WithTimeout(a.ctx, 10*time.Second)
		takenAt = time.Now()
		img, err = cs.still.Snapshot(ctx)
		cancel()
		if a.ctx.Err() != nil {
			return
		}
		a.noteCamera(cs, err)
		if err == nil {
			break
		}
	}
	if err != nil {
		a.log.Warn("capture.failed", "scale", scaleID, "camera", cs.cfg.ID, "attempts", captureAttempts, "error", err.Error())
		return
	}
	meta := captureMeta{
		CaptureID: rand.Text(), ScaleID: scaleID, CameraID: cs.cfg.ID,
		Weight: f.Weight, Unit: f.Unit, Mode: f.Mode, SteadySince: stamp(since), CapturedAt: stamp(takenAt),
	}
	if dropped := a.uploads.push(pendingCapture{meta: meta, jpeg: img, takenAt: takenAt}); dropped != nil {
		a.log.Error("capture.dropped", "capture", dropped.meta.CaptureID, "scale", dropped.meta.ScaleID, "reason", "queue full")
	}
}

const (
	captureQueueSize = 100
	captureRetryFor  = 30 * time.Minute
)

var retryBackoff = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second, time.Minute}

func (a *Agent) uploadLoop(ctx context.Context) {
	for {
		c, ok := a.uploads.pop(ctx)
		if !ok {
			return
		}
		m := c.meta
		for attempt := 0; ; attempt++ {
			uctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := a.up.PostImage(uctx, "captures", c.jpeg, m)
			cancel()
			if err == nil {
				a.log.Info("capture.sent", "capture", m.CaptureID, "scale", m.ScaleID, "camera", m.CameraID, "weight", m.Weight)
				break
			}
			if ctx.Err() != nil {
				a.uploads.pushFront(c)
				return
			}
			if permanent(err) {
				a.log.Error("capture.dropped", "capture", m.CaptureID, "scale", m.ScaleID, "reason", "rejected by the web app", "error", err.Error())
				break
			}
			if time.Since(c.takenAt) > captureRetryFor {
				a.log.Error("capture.dropped", "capture", m.CaptureID, "scale", m.ScaleID, "reason", "not accepted for "+captureRetryFor.String(), "error", err.Error())
				break
			}
			if attempt == 0 {
				a.log.Warn("capture.retrying", "capture", m.CaptureID, "error", err.Error())
			}
			sleepCtx(ctx, retryBackoff[min(attempt, len(retryBackoff)-1)])
		}
	}
}

// captureQueue holds photos waiting to upload. It is bounded: when full, the
// oldest photo goes, because a stale photo is worth less than a fresh one.
type captureQueue struct {
	mu     sync.Mutex
	items  []pendingCapture
	max    int
	notify chan struct{}
}

func newCaptureQueue(max int) *captureQueue {
	return &captureQueue{max: max, notify: make(chan struct{}, 1)}
}

func (q *captureQueue) push(c pendingCapture) (dropped *pendingCapture) {
	q.mu.Lock()
	if len(q.items) >= q.max {
		d := q.items[0]
		dropped = &d
		q.items = q.items[1:]
	}
	q.items = append(q.items, c)
	q.mu.Unlock()
	select {
	case q.notify <- struct{}{}:
	default:
	}
	return dropped
}

func (q *captureQueue) pushFront(c pendingCapture) {
	q.mu.Lock()
	q.items = append([]pendingCapture{c}, q.items...)
	q.mu.Unlock()
}

func (q *captureQueue) pop(ctx context.Context) (pendingCapture, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			c := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return c, true
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return pendingCapture{}, false
		case <-q.notify:
		}
	}
}

func (q *captureQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// describe renders a frame for `check` and logs.
func describe(f scale.Frame) string {
	return fmt.Sprintf("%s %s %s, %s", strconv.FormatFloat(f.Weight, 'f', -1, 64), f.Unit, f.Mode, f.Status)
}
