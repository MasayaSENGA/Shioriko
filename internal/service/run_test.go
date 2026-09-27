package service

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shioriko/internal/mtp"
	"shioriko/internal/mtpdev"
)

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard)
	os.Exit(m.Run())
}

// harness runs a Service with a fake device, session, and USB bus.
type harness struct {
	t   *testing.T
	s   *Service
	loc mtp.Location

	opens    atomic.Int32
	sessions atomic.Int32
	plugged  atomic.Bool

	mu     sync.Mutex
	phases []Phase
	events []EventKind

	cancel context.CancelFunc
	done   chan struct{}

	// ejecting, if set, runs while the fake session handles an eject.
	ejecting func()
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, loc: mtp.Location{Bus: 1, Address: 7, VendorID: 0x22d9}}
	h.plugged.Store(true)
	h.s = New(Options{
		OnStatus: func(st Status) {
			h.mu.Lock()
			if len(h.phases) == 0 || h.phases[len(h.phases)-1] != st.Phase {
				h.phases = append(h.phases, st.Phase)
			}
			h.mu.Unlock()
		},
		OnEvent: func(e Event) {
			h.mu.Lock()
			h.events = append(h.events, e.Kind)
			h.mu.Unlock()
		},
	})
	h.s.opts.Poll = 5 * time.Millisecond
	h.s.openDevice = func(string) (*mtpdev.Device, error) {
		h.opens.Add(1)
		return &mtpdev.Device{Name: "Fake", Location: h.loc}, nil
	}
	h.s.attached = func() ([]mtp.Location, error) {
		if h.plugged.Load() {
			return []mtp.Location{h.loc}, nil
		}
		return nil, nil
	}
	h.s.runSession = h.mountedSession
	return h
}

// mountedSession mimics session: it publishes Mounted and runs until ejected
// or stopped.
func (h *harness) mountedSession(ctx context.Context, dev *mtpdev.Device) (sessionEnd, error) {
	h.sessions.Add(1)
	s := h.s
	s.update(func(st *Status) { *st = Status{Phase: Connecting, Device: dev.Name} })
	select {
	case <-s.eject:
	default:
	}
	s.mu.Lock()
	s.pauseEject = false
	s.mu.Unlock()
	if !s.enterMounted(Status{Phase: Mounted, Device: dev.Name, MountPoint: "/Volumes/" + dev.Name}) {
		return endPaused, nil
	}
	s.emit(Event{Kind: EventMounted, Device: dev.Name})
	select {
	case <-ctx.Done():
		return endShutdown, nil
	case <-s.eject:
		if h.ejecting != nil {
			h.ejecting() // simulates a slow unmount
		}
		s.emit(Event{Kind: EventEjected, Device: dev.Name})
		return s.ejectEnd(), nil
	}
}

func (h *harness) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	go func() {
		h.s.Run(ctx)
		close(h.done)
	}()
	h.t.Cleanup(h.stop)
}

func (h *harness) stop() {
	if h.cancel == nil {
		return
	}
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		h.t.Error("Run did not return after cancel")
	}
	h.cancel = nil
}

// waitPhase waits until the service reports phase p.
func (h *harness) waitPhase(p Phase) {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.s.Status().Phase == p {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatalf("phase %v not reached; now %v, history %v", p, h.s.Status().Phase, h.history())
}

func (h *harness) history() []Phase {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Phase(nil), h.phases...)
}

func (h *harness) countEvents(k EventKind) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, e := range h.events {
		if e == k {
			n++
		}
	}
	return n
}

// settle lets the loop run several poll intervals.
func settle() { time.Sleep(60 * time.Millisecond) }

func TestEjectedPhoneStaysUnmountedUntilReplugOrMount(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.waitPhase(Mounted)

	h.s.Eject(false)
	h.waitPhase(Ejected)
	settle()
	if n := h.opens.Load(); n != 1 {
		t.Fatalf("ejected phone was reopened: %d opens", n)
	}

	// Mount() lifts the gate while the phone stays plugged in.
	h.s.Mount()
	h.waitPhase(Mounted)
	if n := h.opens.Load(); n != 2 {
		t.Fatalf("opens after Mount = %d, want 2", n)
	}

	// Unplugging lifts it too; the phone then comes back at a new address.
	h.s.Eject(false)
	h.waitPhase(Ejected)
	h.plugged.Store(false)
	h.waitPhase(Mounted)
	if n := h.opens.Load(); n != 3 {
		t.Fatalf("opens after replug = %d, want 3", n)
	}

	h.stop()
	if p := h.s.Status().Phase; p != Stopped {
		t.Errorf("phase after stop = %v", p)
	}
}

func TestPauseDuringOpenLeavesPhoneAlone(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	opening := make(chan struct{}, 1)
	h.s.openDevice = func(string) (*mtpdev.Device, error) {
		if h.opens.Add(1) == 1 {
			opening <- struct{}{}
			<-release
		}
		return &mtpdev.Device{Name: "Fake", Location: h.loc}, nil
	}
	h.start()
	<-opening
	h.s.SetPaused(true)
	close(release)
	h.waitPhase(Paused)
	settle()
	if n := h.sessions.Load(); n != 0 {
		t.Fatalf("paused during open, but %d sessions ran", n)
	}

	h.s.SetPaused(false)
	h.waitPhase(Mounted)
}

func TestPauseWhileConnectingIsNotOverwrittenByMounted(t *testing.T) {
	h := newHarness(t)
	h.s.runSession = func(ctx context.Context, dev *mtpdev.Device) (sessionEnd, error) {
		// The user pauses while the volume is being mounted.
		h.s.SetPaused(true)
		return h.mountedSession(ctx, dev)
	}
	h.start()
	h.waitPhase(Paused)
	settle()
	for _, p := range h.history() {
		if p == Mounted {
			t.Fatalf("Mounted published after pausing: %v", h.history())
		}
	}
	if h.countEvents(EventMounted) != 0 {
		t.Error("EventMounted emitted after pausing")
	}
}

func TestResumeAfterPauseEjectMountsAgain(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.waitPhase(Mounted)

	h.s.SetPaused(true) // ejects the mounted volume
	h.waitPhase(Paused)
	h.s.SetPaused(false)
	h.waitPhase(Mounted) // not Ejected, although the phone never left
	if n := h.opens.Load(); n != 2 {
		t.Errorf("opens = %d, want 2", n)
	}
}

func TestRetryDuringConnectDoesNotLiftLaterEject(t *testing.T) {
	h := newHarness(t)
	release := make(chan struct{})
	opening := make(chan struct{}, 1)
	h.s.openDevice = func(string) (*mtpdev.Device, error) {
		if h.opens.Add(1) == 1 {
			opening <- struct{}{}
			<-release
		}
		return &mtpdev.Device{Name: "Fake", Location: h.loc}, nil
	}
	h.start()
	<-opening
	h.s.Mount() // the user clicks 今すぐ再試行 while the attempt is in flight
	close(release)
	h.waitPhase(Mounted)

	h.s.Eject(false)
	h.waitPhase(Ejected)
	settle()
	if n := h.opens.Load(); n != 1 {
		t.Fatalf("stale retry request remounted the ejected phone (%d opens)", n)
	}
}

func TestTransientOpenFailuresAreNotReported(t *testing.T) {
	h := newHarness(t)
	var mode atomic.Int32 // 0: fail, 1: no device
	h.s.openDevice = func(string) (*mtpdev.Device, error) {
		h.opens.Add(1)
		if mode.Load() == 1 {
			return nil, mtpdev.ErrNoDevice
		}
		return nil, errors.New("mtp: open session: timeout")
	}
	h.start()
	h.waitPhase(Failed)
	if n := h.opens.Load(); n < openFailThreshold {
		t.Fatalf("Failed after %d attempts, want at least %d", n, openFailThreshold)
	}
	settle()
	if n := h.countEvents(EventFailed); n != 1 {
		t.Fatalf("EventFailed reported %d times, want once per streak", n)
	}
	if hist := h.history(); len(hist) < 2 || hist[0] != Connecting {
		t.Errorf("first failures should show Connecting, history %v", hist)
	}

	// Unplugging resets the streak: the next failure is transient again.
	mode.Store(1)
	h.waitPhase(Waiting)
	mode.Store(0)
	h.waitPhase(Connecting)
}

func TestBusyReportedOnce(t *testing.T) {
	h := newHarness(t)
	h.s.openDevice = func(string) (*mtpdev.Device, error) {
		h.opens.Add(1)
		return nil, mtpdev.ErrBusy
	}
	h.start()
	h.waitPhase(Busy)
	settle()
	if h.opens.Load() < 3 {
		t.Fatal("busy device not retried")
	}
	if n := h.countEvents(EventBusy); n != 1 {
		t.Errorf("EventBusy reported %d times, want 1", n)
	}
}

func TestOnceStopsAfterFirstSession(t *testing.T) {
	h := newHarness(t)
	h.s.opts.Once = true
	h.start()
	h.waitPhase(Mounted)
	h.s.Eject(false)
	select {
	case <-h.done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the first session in Once mode")
	}
	if p := h.s.Status().Phase; p != Stopped {
		t.Errorf("phase = %v, want Stopped", p)
	}
}

func TestResumeDuringPauseEjectMountsAgain(t *testing.T) {
	h := newHarness(t)
	h.ejecting = func() {
		// The user resumes while the pause's unmount is still running.
		h.s.SetPaused(false)
	}
	h.start()
	h.waitPhase(Mounted)
	h.s.SetPaused(true)
	settle()
	h.waitPhase(Mounted)
	if n := h.opens.Load(); n != 2 {
		t.Fatalf("opens = %d, want 2 (history %v)", n, h.history())
	}
	for _, p := range h.history() {
		if p == Ejected {
			t.Fatalf("pause-driven eject gated the phone: %v", h.history())
		}
	}
}

func TestUserEjectStillGatesAfterEarlierPause(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.waitPhase(Mounted)
	h.s.SetPaused(true)
	h.waitPhase(Paused)
	h.s.SetPaused(false)
	h.waitPhase(Mounted)
	// A later eject by the user must gate as usual.
	h.s.Eject(false)
	h.waitPhase(Ejected)
	settle()
	if n := h.opens.Load(); n != 2 {
		t.Fatalf("opens = %d, want 2", n)
	}
}
