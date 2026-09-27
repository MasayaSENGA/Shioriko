package menubar

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"shioriko/internal/mtpdev"
	"shioriko/internal/service"
)

func TestFormatBytes(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0, "0 B"},
		{999, "999 B"},
		{1000, "1 KB"},
		{1500, "1.5 KB"},
		{9.96e6, "10 MB"},
		{23.4e6, "23 MB"},
		{128e9, "128 GB"},
		{3.14e9, "3.1 GB"},
		{2e12, "2 TB"},
		{5e15, "5000 TB"},
	} {
		if got := formatBytes(tc.in); got != tc.want {
			t.Errorf("formatBytes(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestMakeStorage(t *testing.T) {
	s := makeStorage(mtpdev.Storage{Description: "内部共有ストレージ", Capacity: 100e9, Free: 5e9})
	if s.Fraction != 0.95 || !s.Warn {
		t.Errorf("fraction/warn = %v/%v, want 0.95/true", s.Fraction, s.Warn)
	}
	if s.Detail != "空き 5 GB / 100 GB" {
		t.Errorf("detail = %q", s.Detail)
	}
	// Free larger than capacity (seen on some firmwares) must not underflow.
	s = makeStorage(mtpdev.Storage{Capacity: 10, Free: 20, ReadOnly: true})
	if s.Fraction != 0 || s.Warn || !strings.HasSuffix(s.Detail, "（読み取り専用）") {
		t.Errorf("got %+v", s)
	}
	s = makeStorage(mtpdev.Storage{})
	if s.Detail != "容量不明" || s.Fraction != 0 {
		t.Errorf("unknown capacity: %+v", s)
	}
}

func TestActivity(t *testing.T) {
	if got := activity(1000, 0); got != "" {
		t.Errorf("tiny rates should be hidden, got %q", got)
	}
	got := activity(12.3e6, 45e3)
	if !strings.Contains(got, "↓ 読み込み 12 MB/s") || !strings.Contains(got, "↑ 書き込み 45 KB/s") {
		t.Errorf("activity = %q", got)
	}
}

func TestMakeViewActions(t *testing.T) {
	type flags struct{ reveal, eject, mount bool }
	for _, tc := range []struct {
		st   service.Status
		want flags
		tone string
	}{
		{service.Status{Phase: service.Waiting}, flags{}, "idle"},
		{service.Status{Phase: service.Paused, Paused: true}, flags{}, "idle"},
		{service.Status{Phase: service.Busy}, flags{mount: true}, "warning"},
		{service.Status{Phase: service.Connecting, Device: "Pixel"}, flags{}, "idle"},
		{service.Status{Phase: service.Mounted, Device: "Pixel", MountPoint: "/Volumes/Pixel"}, flags{reveal: true, eject: true}, "active"},
		{service.Status{Phase: service.Ejecting, Device: "Pixel", MountPoint: "/Volumes/Pixel"}, flags{reveal: true}, "active"},
		{service.Status{Phase: service.Ejected, Device: "Pixel"}, flags{mount: true}, "idle"},
		{service.Status{Phase: service.Failed, Err: errors.New("x")}, flags{mount: true}, "warning"},
	} {
		v := makeView(tc.st)
		got := flags{v.CanReveal, v.CanEject, v.CanMount}
		if got != tc.want || v.Tone != tc.tone {
			t.Errorf("%v: flags %+v tone %q, want %+v %q", tc.st.Phase, got, v.Tone, tc.want, tc.tone)
		}
		if v.Title == "" || v.Phase != tc.st.Phase.String() {
			t.Errorf("%v: title %q phase %q", tc.st.Phase, v.Title, v.Phase)
		}
		if tc.want.mount && v.MountTitle == "" {
			t.Errorf("%v: missing mount title", tc.st.Phase)
		}
	}
}

func TestMakeViewMountedDetails(t *testing.T) {
	st := service.Status{Phase: service.Mounted, Device: "Pixel", MountPoint: "/Volumes/Pixel", StoragesKnown: true}
	v := makeView(st)
	if len(v.Notes) != 1 || !strings.Contains(v.Notes[0], "ロックを解除") {
		t.Errorf("no storages should show the unlock hint, notes = %q", v.Notes)
	}
	if v.Active {
		t.Error("idle volume marked active")
	}

	st.Storages = []mtpdev.Storage{{Description: "内部", Capacity: 10e9, Free: 5e9}}
	st.WriteRate = 5e6
	v = makeView(st)
	if len(v.Storages) != 1 || len(v.Notes) != 1 || !v.Active {
		t.Errorf("storages %d notes %q active %v", len(v.Storages), v.Notes, v.Active)
	}

	// Before the first storage query the hint must not flash up.
	st = service.Status{Phase: service.Mounted, Device: "Pixel", MountPoint: "/Volumes/Pixel"}
	if v := makeView(st); len(v.Notes) != 0 {
		t.Errorf("unexpected notes %q", v.Notes)
	}
}

func TestDescribeError(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "自動的に再試行します"},
		{errors.New("mtp: more than 1 device: a,b"), "複数"},
		{fmt.Errorf("wrap: %w", mtpdev.ErrDeviceLost), "途切れ"},
		{errors.New("device not ready: timeout"), "応答しません"},
		{errors.New("mtp: open session: RetCode 2019"), "通信を開始できません"},
		{errors.New("mount: osascript: exit status 1"), "ボリューム"},
		{errors.New("something else"), "ログ"},
	} {
		if got := describeError(tc.err); !strings.Contains(got, tc.want) {
			t.Errorf("describeError(%v) = %q, want it to mention %q", tc.err, got, tc.want)
		}
	}
}
