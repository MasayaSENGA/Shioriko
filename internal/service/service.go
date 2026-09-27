// Package service runs Shioriko's device lifecycle: it watches USB for an
// Android (MTP) device, serves it over loopback WebDAV, mounts it as a Finder
// volume, and tears everything down again when the device goes away or the
// user ejects it. Front ends (the menu bar app, the headless CLI) observe it
// through Status snapshots and Events and drive it with a few commands.
package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"shioriko/internal/davfs"
	"shioriko/internal/mount"
	"shioriko/internal/mtp"
	"shioriko/internal/mtpdev"
)

// Phase is what the service is currently doing.
type Phase int

const (
	// Waiting: no MTP device is attached.
	Waiting Phase = iota
	// Paused: the user turned automatic mounting off.
	Paused
	// Busy: a device is attached but another program holds it.
	Busy
	// Connecting: a device was opened and is being mounted.
	Connecting
	// Mounted: the device is available as a Finder volume.
	Mounted
	// Ejecting: an unmount requested by the user is in progress.
	Ejecting
	// Ejected: the user ejected the volume; the device is still plugged in
	// and is left alone until it is reconnected or Mount is called.
	Ejected
	// Failed: the last attempt failed; it is retried automatically.
	Failed
	// Stopped: Run has returned.
	Stopped
)

var phaseNames = [...]string{"waiting", "paused", "busy", "connecting", "mounted", "ejecting", "ejected", "failed", "stopped"}

func (p Phase) String() string {
	if int(p) < len(phaseNames) {
		return phaseNames[p]
	}
	return fmt.Sprintf("phase(%d)", int(p))
}

// Status is a snapshot of the service state.
type Status struct {
	Phase Phase
	// Paused is set when automatic mounting is off. The phase can still be
	// Mounted while a volume mounted before pausing is in use.
	Paused bool
	// Device is the device's friendly name (Connecting, Mounted, Ejecting,
	// Ejected).
	Device     string
	MountPoint string
	// Storages is the latest storage list; StoragesKnown is false until the
	// first successful query of a session.
	Storages      []mtpdev.Storage
	StoragesKnown bool
	// ReadRate and WriteRate are recent file transfer speeds in bytes/s.
	ReadRate, WriteRate float64
	// Err explains Failed.
	Err error
}

// EventKind identifies a notable one-off occurrence.
type EventKind int

const (
	// EventMounted: a device was mounted.
	EventMounted EventKind = iota + 1
	// EventDisconnected: a mounted device was unplugged or stopped answering.
	EventDisconnected
	// EventEjected: the volume was ejected (from Shioriko or from Finder).
	EventEjected
	// EventEjectFailed: an eject without force failed, typically because files
	// on the volume are in use. Err has the details.
	EventEjectFailed
	// EventFailed: connecting or mounting failed. Only the first failure of a
	// streak is reported.
	EventFailed
	// EventBusy: a device is attached but held by another program. Only
	// reported when entering that state.
	EventBusy
)

// Event describes something that happened.
type Event struct {
	Kind       EventKind
	Device     string
	MountPoint string
	Err        error
}

// Options configure a Service.
type Options struct {
	// Pattern selects a device when several are attached (see mtp.SelectDevice).
	Pattern string
	// Poll is how often to look for a device and check that it is alive.
	Poll time.Duration
	// Verbose enables MTP diagnostics; DavLog logs WebDAV errors.
	Verbose, DavLog bool
	// Once stops Run after the first session ends.
	Once bool
	// SpoolDir holds upload spool files.
	SpoolDir string
	// StateDir, if set, records the mounted volume so a later run can clean
	// it up after a crash.
	StateDir string
	// Reveal reports whether to open a freshly mounted volume in Finder.
	Reveal func() bool
	// OnStatus and OnEvent are called from the service goroutine, in order.
	// They must not block or call back into the Service synchronously.
	OnStatus func(Status)
	OnEvent  func(Event)
}

// Service is the device lifecycle controller. Create it with New, then call Run.
type Service struct {
	opts Options

	wake  chan struct{} // re-evaluate now (pause, resume, mount)
	eject chan bool     // eject the mounted volume; the value is force

	mu      sync.Mutex
	st      Status
	paused  bool
	remount bool // Mount was called: override the eject gate and retry now
	// pauseEject marks the queued eject as coming from SetPaused, so it
	// must not gate the phone (resuming should mount it again).
	pauseEject bool

	pubMu sync.Mutex // serialises observer callbacks

	// Seams for tests; New sets the real implementations.
	openDevice func(pattern string) (*mtpdev.Device, error)
	runSession func(ctx context.Context, dev *mtpdev.Device) (sessionEnd, error)
	attached   func() ([]mtp.Location, error)
}

// New creates a service. It does nothing until Run is called.
func New(opts Options) *Service {
	if opts.Poll < time.Second {
		opts.Poll = 2 * time.Second
	}
	if opts.Reveal == nil {
		opts.Reveal = func() bool { return false }
	}
	s := &Service{
		opts:       opts,
		wake:       make(chan struct{}, 1),
		eject:      make(chan bool, 1),
		openDevice: mtpdev.Open,
		attached:   mtp.AttachedLocations,
	}
	s.runSession = s.session
	return s
}

// Status returns the current state.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot()
}

func (s *Service) snapshot() Status {
	st := s.st
	st.Paused = s.paused
	st.Storages = slices.Clone(s.st.Storages)
	return st
}

// SetPaused turns automatic mounting off or back on. Pausing while a volume
// is mounted also ejects it (without force).
func (s *Service) SetPaused(p bool) {
	s.mu.Lock()
	changed := s.paused != p
	s.paused = p
	mounted := s.st.Phase == Mounted
	if changed && p && mounted {
		s.pauseEject = true
	}
	s.mu.Unlock()
	if !changed {
		return
	}
	if p {
		log.Printf("automatic mounting paused")
	} else {
		log.Printf("automatic mounting resumed")
	}
	if p && mounted {
		s.Eject(false)
	}
	s.poke()
	s.publish()
}

// Eject unmounts the current volume and releases the device. Without force
// it fails (reported as EventEjectFailed) if files on the volume are in use.
// The device is not mounted again until it is reconnected or Mount is called.
func (s *Service) Eject(force bool) {
	s.mu.Lock()
	ok := s.st.Phase == Mounted
	s.mu.Unlock()
	if !ok {
		return
	}
	select {
	case s.eject <- force:
	default:
	}
}

// Mount retries immediately: it mounts an ejected device that is still
// plugged in, or retries after a failure.
func (s *Service) Mount() {
	s.mu.Lock()
	s.remount = true
	s.mu.Unlock()
	s.poke()
}

func (s *Service) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// update mutates the status under the lock and publishes it.
func (s *Service) update(fn func(st *Status)) {
	s.mu.Lock()
	fn(&s.st)
	s.mu.Unlock()
	s.publish()
}

func (s *Service) publish() {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	if s.opts.OnStatus != nil {
		s.opts.OnStatus(s.Status())
	}
}

func (s *Service) emit(e Event) {
	s.pubMu.Lock()
	defer s.pubMu.Unlock()
	if s.opts.OnEvent != nil {
		s.opts.OnEvent(e)
	}
}

// setIdle records a state with no active session.
func (s *Service) setIdle(p Phase, device string, err error) {
	s.update(func(st *Status) {
		*st = Status{Phase: p, Device: device, Err: err}
	})
}

func (s *Service) isPaused() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

func (s *Service) takeRemount() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.remount
	s.remount = false
	return r
}

// wait sleeps for d (forever if d <= 0) or until poked. It returns false if
// ctx is done.
func (s *Service) wait(ctx context.Context, d time.Duration) bool {
	var timeout <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case <-ctx.Done():
		return false
	case <-s.wake:
		return true
	case <-timeout:
		return true
	}
}

// ejectGate remembers a device the user ejected while it is still plugged in,
// so it is not immediately mounted again.
type ejectGate struct {
	loc  mtp.Location
	name string
}

// present reports whether the gated device is still attached at the same bus
// address. Re-plugging assigns a new address, which lifts the gate.
func (g *ejectGate) present(attached func() ([]mtp.Location, error)) bool {
	locs, err := attached()
	if err != nil {
		return true // cannot tell; stay put
	}
	return slices.Contains(locs, g.loc)
}

// Run drives the lifecycle until ctx is cancelled (or, with Once, until the
// first session ends). A mounted volume is unmounted before Run returns.
func (s *Service) Run(ctx context.Context) {
	defer s.update(func(st *Status) { *st = Status{Phase: Stopped} })

	if mp, err := mount.CleanupStale(s.opts.StateDir); mp != "" {
		if err != nil {
			log.Printf("could not remove stale volume %s left by a previous run: %v", mp, err)
		} else {
			log.Printf("removed stale volume %s left by a previous run", mp)
		}
	}

	log.Printf("waiting for an Android device (MTP). Plug in your phone and choose \"File transfer\" as the USB mode.")
	var gate *ejectGate
	var fails failStreak
	lastPhase := Phase(-1)
	for ctx.Err() == nil {
		if s.isPaused() {
			// Resuming means "mount automatically again", including a
			// phone that was ejected (possibly by the pause itself).
			gate = nil
			s.setIdle(Paused, "", nil)
			lastPhase = Paused
			if !s.wait(ctx, 0) {
				return
			}
			continue
		}
		remount := s.takeRemount()
		if gate != nil {
			if remount || !gate.present(s.attached) {
				gate = nil
			} else {
				s.setIdle(Ejected, gate.name, nil)
				lastPhase = Ejected
				if !s.wait(ctx, s.opts.Poll) {
					return
				}
				continue
			}
		}

		dev, err := s.openDevice(s.opts.Pattern)
		if err != nil {
			var phase Phase
			switch {
			case errors.Is(err, mtpdev.ErrNoDevice):
				phase = Waiting
				fails.reset()
				err = nil
				if lastPhase != Waiting && lastPhase != Phase(-1) {
					log.Printf("waiting for a device...")
				}
			case errors.Is(err, mtpdev.ErrBusy):
				phase = Busy
				err = nil
				if lastPhase != Busy {
					log.Printf("a device is attached but busy (another program is using it); retrying...")
					s.emit(Event{Kind: EventBusy})
				}
			default:
				// Right after plugging in, the device is often still busy
				// with the previous host and the first attempts fail. Only
				// report a failure once it persists.
				fails.n++
				if fails.n < openFailThreshold {
					phase = Connecting
					if s.opts.Verbose {
						log.Printf("open failed (will retry): %v", err)
					}
					err = nil
				} else {
					phase = Failed
					if !fails.reported {
						fails.reported = true
						log.Printf("open failed: %v", err)
						s.emit(Event{Kind: EventFailed, Err: err})
					} else if s.opts.Verbose {
						log.Printf("open failed: %v", err)
					}
				}
			}
			s.setIdle(phase, "", err)
			lastPhase = phase
			if !s.wait(ctx, s.backoff(fails.n)) {
				return
			}
			continue
		}

		if s.isPaused() {
			// Paused while Open was running: leave the phone alone.
			dev.Close()
			continue
		}
		dev.Verbose = s.opts.Verbose
		log.Printf("connected: %s", dev.Name)
		end, err := s.runSession(ctx, dev)
		loc, name := dev.Location, dev.Name
		closeDevice(dev)
		lastPhase = Connecting

		switch end {
		case endShutdown:
			return
		case endEjected:
			fails.reset()
			gate = &ejectGate{loc: loc, name: name}
			log.Printf("released %s; it is mounted again when reconnected", name)
		case endLost:
			fails.reset()
			if err != nil {
				log.Printf("device disconnected while connecting: %v", err)
			}
		case endPaused:
			fails.reset()
			log.Printf("released %s: automatic mounting is paused", name)
		case endFailed:
			// The device opened, so this is not the transient plug-in race:
			// report it right away.
			fails.n = max(fails.n+1, openFailThreshold)
			log.Printf("could not mount %s: %v", name, err)
			if !fails.reported {
				fails.reported = true
				s.emit(Event{Kind: EventFailed, Device: name, Err: err})
			}
			s.setIdle(Failed, name, err)
			lastPhase = Failed
		}
		if s.opts.Once {
			return
		}
		if end == endFailed && !s.wait(ctx, s.backoff(fails.n)) {
			return
		}
	}
}

// openFailThreshold is how many consecutive failed attempts count as a real
// failure rather than the device settling after being plugged in.
const openFailThreshold = 3

// failStreak tracks consecutive failures and whether they were reported.
type failStreak struct {
	n        int
	reported bool
}

func (f *failStreak) reset() { *f = failStreak{} }

// backoff is the delay before the next attempt after n consecutive failures:
// the poll interval at first, then doubling up to 30s.
func (s *Service) backoff(n int) time.Duration {
	d := s.opts.Poll
	for i := openFailThreshold; i < n && d < 30*time.Second; i++ {
		d *= 2
	}
	return min(d, 30*time.Second)
}

// enterMounted publishes a freshly mounted volume, unless automatic mounting
// was paused while connecting, in which case it returns false and the caller
// should unmount. It checks and publishes under the lock SetPaused reads the
// phase with, so a pause either sees Mounted (and queues an eject) or is seen
// here. A Mount/retry request made while connecting has now been satisfied, so
// it is cleared and cannot lift the eject gate after a later eject.
func (s *Service) enterMounted(st Status) bool {
	s.mu.Lock()
	if s.paused {
		s.mu.Unlock()
		return false
	}
	s.st = st
	s.remount = false
	s.mu.Unlock()
	select {
	case <-s.wake:
	default:
	}
	s.publish()
	return true
}

// ejectEnd is how a session ends after its volume was ejected on request:
// endPaused if the eject came from pausing (so the phone is not gated and
// resuming mounts it again, even if resumed while the eject was running),
// endEjected otherwise.
func (s *Service) ejectEnd() sessionEnd {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pauseEject {
		s.pauseEject = false
		return endPaused
	}
	return endEjected
}

type sessionEnd int

const (
	endShutdown sessionEnd = iota // ctx cancelled
	endEjected                    // unmounted by the user (Shioriko or Finder)
	endLost                       // device unplugged or unresponsive
	endFailed                     // could not get as far as a mounted volume
	endPaused                     // paused before the volume was published
)

// storageRefresh is how often storage capacity is re-read while mounted.
const storageRefresh = 5 * time.Second

// session serves and mounts a connected device until it disconnects, is
// ejected, or ctx is cancelled.
func (s *Service) session(ctx context.Context, dev *mtpdev.Device) (sessionEnd, error) {
	name := dev.Name
	s.update(func(st *Status) { *st = Status{Phase: Connecting, Device: name} })

	fs, err := davfs.New(dev, s.opts.SpoolDir)
	if err != nil {
		return endFailed, fmt.Errorf("filesystem init: %w", err)
	}
	// Sanity check that we can actually talk to the device before mounting.
	if _, err := fs.Stat(ctx, "/"); err != nil {
		if dev.Lost() {
			return endLost, err
		}
		return endFailed, fmt.Errorf("device not ready: %w", err)
	}

	var logger func(*http.Request, error)
	if s.opts.DavLog {
		logger = func(r *http.Request, err error) {
			if err != nil {
				log.Printf("dav %s %s: %v", r.Method, r.URL.Path, err)
			}
		}
	}
	vol, err := mount.Serve(fs, name, logger)
	if err != nil {
		return endFailed, fmt.Errorf("serve: %w", err)
	}
	defer func() {
		vol.Close()
		mount.Forget(s.opts.StateDir)
	}()

	mctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	err = vol.Mount(mctx)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return endShutdown, nil
		}
		return endFailed, err
	}
	if err := vol.Record(s.opts.StateDir); err != nil && s.opts.Verbose {
		log.Printf("could not record mount: %v", err)
	}
	mp := vol.MountPoint
	log.Printf("mounted at %s", mp)

	// Drop an eject request left over from an earlier session.
	select {
	case <-s.eject:
	default:
	}
	s.mu.Lock()
	s.pauseEject = false
	s.mu.Unlock()

	storages, ok, _ := dev.TryStorages()
	if !s.enterMounted(Status{Phase: Mounted, Device: name, MountPoint: mp, Storages: storages, StoragesKnown: ok}) {
		return endPaused, nil // the deferred Close unmounts
	}
	s.emit(Event{Kind: EventMounted, Device: name, MountPoint: mp})
	if s.opts.Reveal() {
		vol.Reveal()
	}

	// Device round trips (liveness pings, storage queries) can block for a
	// long time when the phone stops answering, so they run in probe and
	// this loop stays free to handle quit and eject at once.
	probes := make(chan probeResult, 1)
	stopProbe := make(chan struct{})
	defer close(stopProbe)
	go s.probe(dev, probes, stopProbe)

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	lastRead, lastWritten := dev.Transferred()
	lastSample := time.Now()
	lost := func() (sessionEnd, error) {
		log.Printf("device disconnected; unmounting %s", mp)
		s.emit(Event{Kind: EventDisconnected, Device: name, MountPoint: mp})
		return endLost, nil
	}
	for {
		select {
		case <-ctx.Done():
			log.Printf("shutting down; unmounting %s", mp)
			return endShutdown, nil

		case force := <-s.eject:
			s.update(func(st *Status) { st.Phase = Ejecting })
			log.Printf("ejecting %s", mp)
			if err := vol.Unmount(force); err != nil {
				log.Printf("eject failed: %v", err)
				s.update(func(st *Status) { st.Phase = Mounted; s.pauseEject = false })
				s.emit(Event{Kind: EventEjectFailed, Device: name, MountPoint: mp, Err: err})
				continue
			}
			s.emit(Event{Kind: EventEjected, Device: name, MountPoint: mp})
			return s.ejectEnd(), nil

		case p := <-probes:
			if p.lost {
				return lost()
			}
			if p.storagesOK {
				s.update(func(st *Status) { st.Storages, st.StoragesKnown = p.storages, true })
			}

		case now := <-tick.C:
			if dev.Lost() {
				return lost()
			}
			// If the user ejected the volume in Finder, stop this session.
			if !vol.Mounted() {
				log.Printf("volume ejected; releasing device")
				s.emit(Event{Kind: EventEjected, Device: name, MountPoint: mp})
				return endEjected, nil
			}
			read, written := dev.Transferred()
			secs := now.Sub(lastSample).Seconds()
			rr, wr := float64(read-lastRead)/secs, float64(written-lastWritten)/secs
			lastRead, lastWritten, lastSample = read, written, now
			s.update(func(st *Status) { st.ReadRate, st.WriteRate = rr, wr })
		}
	}
}

// probeResult is one round of probe.
type probeResult struct {
	lost       bool
	storages   []mtpdev.Storage
	storagesOK bool
}

// probe pings dev every poll interval and re-reads its storages every
// storageRefresh, sending results until stop is closed. It may outlive the
// session while a round trip to an unresponsive device is still pending.
func (s *Service) probe(dev *mtpdev.Device, out chan<- probeResult, stop <-chan struct{}) {
	t := time.NewTicker(s.opts.Poll)
	defer t.Stop()
	lastStorage := time.Now()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		var r probeResult
		if err := dev.Ping(); err != nil {
			if isLost(err) {
				r.lost = true
			} else if s.opts.Verbose {
				log.Printf("ping: %v", err)
			}
		}
		if !r.lost && time.Since(lastStorage) >= storageRefresh {
			if st, ok, err := dev.TryStorages(); ok && err == nil {
				r.storages, r.storagesOK = st, true
				lastStorage = time.Now()
			}
		}
		if !r.lost && !r.storagesOK {
			continue
		}
		select {
		case out <- r:
		case <-stop:
			return
		}
	}
}

// closeTimeout bounds how long releasing a device may hold up the service.
const closeTimeout = 5 * time.Second

// closeDevice releases dev, giving up waiting after closeTimeout: an
// unresponsive phone can keep a transaction (and the device lock) busy for a
// minute, and neither quitting nor the next connection should wait for it.
func closeDevice(dev *mtpdev.Device) {
	done := make(chan struct{})
	go func() {
		dev.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeTimeout):
		log.Printf("%s is not responding; releasing it in the background", dev.Name)
	}
}

func isLost(err error) bool {
	return errors.Is(err, mtpdev.ErrDeviceLost) || (err != nil && strings.Contains(err.Error(), "disconnect"))
}
