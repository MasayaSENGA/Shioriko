package service

import (
	"testing"
	"time"
)

func TestBackoff(t *testing.T) {
	s := New(Options{Poll: 2 * time.Second})
	for _, tc := range []struct {
		n    int
		want time.Duration
	}{
		{0, 2 * time.Second},
		{1, 2 * time.Second},
		{openFailThreshold, 2 * time.Second},
		{openFailThreshold + 1, 4 * time.Second},
		{openFailThreshold + 2, 8 * time.Second},
		{openFailThreshold + 10, 30 * time.Second},
	} {
		if got := s.backoff(tc.n); got != tc.want {
			t.Errorf("backoff(%d) = %v, want %v", tc.n, got, tc.want)
		}
	}
}

func TestNewDefaults(t *testing.T) {
	s := New(Options{})
	if s.opts.Poll != 2*time.Second {
		t.Errorf("default poll = %v", s.opts.Poll)
	}
	if s.opts.Reveal == nil || s.opts.Reveal() {
		t.Error("default Reveal should exist and return false")
	}
}

func TestPhaseString(t *testing.T) {
	if Mounted.String() != "mounted" || Stopped.String() != "stopped" || Phase(99).String() != "phase(99)" {
		t.Error("unexpected phase names")
	}
}

func TestCommandsWithoutSession(t *testing.T) {
	s := New(Options{})
	var got []Status
	s.opts.OnStatus = func(st Status) { got = append(got, st) }

	// Eject with nothing mounted must not queue a request that would eject
	// the next session.
	s.Eject(false)
	select {
	case <-s.eject:
		t.Fatal("eject queued while not mounted")
	default:
	}

	s.SetPaused(true)
	if !s.Status().Paused || len(got) == 0 || !got[len(got)-1].Paused {
		t.Fatalf("pause not published: %+v", got)
	}
	s.SetPaused(true) // no change: no extra publish
	n := len(got)
	s.SetPaused(true)
	if len(got) != n {
		t.Error("unchanged pause republished")
	}

	s.Mount()
	if !s.takeRemount() || s.takeRemount() {
		t.Error("remount flag should be set once")
	}
}
