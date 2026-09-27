package mtpdev

import (
	"errors"
	"fmt"
	"testing"

	"github.com/hanwen/usb"
)

func TestClassifySelectError(t *testing.T) {
	for _, tc := range []struct {
		msg  string
		want error
	}{
		{"no MTP devices found", ErrNoDevice},
		{"no device matched", ErrNoDevice},
		{"no MTP devices found (last error: has no MTP in interface string)", ErrNoDevice},
		// A phone held by another program: every claim failed.
		{"no MTP devices found (last error: mtp: claim interface: LIBUSB_ERROR_ACCESS)", ErrBusy},
		{"no MTP devices found (last error: LIBUSB_ERROR_BUSY)", ErrBusy},
	} {
		if got := classifySelectError(errors.New(tc.msg)); got != tc.want {
			t.Errorf("classifySelectError(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
	// The real chain wraps the libusb error value.
	wrapped := fmt.Errorf("no MTP devices found (claim failed: %w)", fmt.Errorf("mtp: claim interface: %w", usb.ERROR_ACCESS))
	if got := classifySelectError(wrapped); got != ErrBusy {
		t.Errorf("wrapped claim error classified as %v", got)
	}
	other := errors.New("mtp: more than 1 device: a,b")
	if got := classifySelectError(other); got != other {
		t.Errorf("unrelated error rewritten to %v", got)
	}
}

func TestCountingReaderWriter(t *testing.T) {
	d := &Device{}
	w := &countingWriter{w: discard{}, n: &d.bytesRead}
	w.Write(make([]byte, 100))
	r := &countingReader{r: zeros{}, n: &d.bytesWritten}
	r.Read(make([]byte, 42))
	if rd, wr := d.Transferred(); rd != 100 || wr != 42 {
		t.Errorf("Transferred() = %d, %d", rd, wr)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { return len(p), nil }
