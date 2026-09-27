package mtpdev

import (
	"log"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"shioriko/internal/mtp"
)

// On macOS the system daemon ptpcamerad (Image Capture) claims every
// PTP/MTP-class USB interface as soon as a device is plugged in, and libusb
// then fails with LIBUSB_ERROR_ACCESS. The daemon is relaunched by launchd
// within roughly a second, so the only reliable way to get the interface is
// to kill it and claim the interface immediately afterwards. Once we hold the
// interface, ptpcamerad's own open fails and it leaves the device alone.
func init() {
	mtp.ClaimHook = evictPTPCamera
}

func evictPTPCamera(_ *mtp.Device, err error) bool {
	msg := err.Error()
	if !strings.Contains(msg, "ACCESS") && !strings.Contains(msg, "BUSY") && !strings.Contains(msg, "NOT_FOUND") {
		return false
	}
	out, perr := exec.Command("/usr/bin/pgrep", "-x", "ptpcamerad").Output()
	if perr != nil || len(strings.TrimSpace(string(out))) == 0 {
		return false
	}
	killed := false
	for _, f := range strings.Fields(string(out)) {
		pid, aerr := strconv.Atoi(f)
		if aerr != nil {
			continue
		}
		if kerr := syscall.Kill(pid, syscall.SIGKILL); kerr == nil {
			killed = true
		}
	}
	if killed {
		log.Printf("mtp: evicted ptpcamerad (macOS Image Capture) to take over the MTP interface")
	}
	return killed
}
