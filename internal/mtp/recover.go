package mtp

import (
	"encoding/binary"
	"fmt"
	"log"
	"strings"
	"time"
)

// PTP/MTP class-specific control requests (PIMA 15740 / MTP spec).
const (
	reqCancel          = 0x64
	reqGetDeviceStatus = 0x67
	reqDeviceReset     = 0x66

	cancelCode = 0x4001
)

// DrainIn reads and discards data pending on the bulk-IN endpoint until a
// timeout occurs. It returns the number of bytes discarded. Use it to
// recover a device whose previous host process died mid-transaction.
func (d *Device) DrainIn(timeoutMs int) (int, error) {
	if d.h == nil {
		return 0, fmt.Errorf("mtp: DrainIn: device not open")
	}
	buf := make([]byte, rwBufSize)
	total := 0
	for i := 0; i < 100000; i++ {
		n, err := d.h.BulkTransfer(d.fetchEP, buf, timeoutMs)
		total += n
		if err != nil {
			if strings.Contains(err.Error(), "TIMEOUT") {
				return total, nil
			}
			return total, err
		}
		if n == 0 {
			return total, nil
		}
	}
	return total, nil
}

// DeviceStatus issues the PTP Get_Device_Status control request and returns
// the status code (0x2001 = OK, 0x2019 = Device_Busy, ...).
func (d *Device) DeviceStatus() (uint16, error) {
	if d.h == nil {
		return 0, fmt.Errorf("mtp: DeviceStatus: device not open")
	}
	buf := make([]byte, 64)
	err := d.h.ControlTransfer(0xA1, reqGetDeviceStatus, 0, uint16(d.ifaceDescr.InterfaceNumber), buf, d.Timeout)
	if err != nil {
		return 0, err
	}
	// wLength (u16), Code (u16), params...
	return binary.LittleEndian.Uint16(buf[2:4]), nil
}

// CancelPending sends the PTP Cancel_Request control request so the device
// aborts whatever transaction it is stuck in, then waits until it reports OK.
func (d *Device) CancelPending() error {
	if d.h == nil {
		return fmt.Errorf("mtp: CancelPending: device not open")
	}
	data := make([]byte, 6)
	binary.LittleEndian.PutUint16(data[0:2], cancelCode)
	tid := uint32(0)
	if d.session != nil {
		tid = d.session.tid
	}
	binary.LittleEndian.PutUint32(data[2:6], tid)
	if err := d.h.ControlTransfer(0x21, reqCancel, 0, uint16(d.ifaceDescr.InterfaceNumber), data, d.Timeout); err != nil {
		return fmt.Errorf("cancel request: %w", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st, err := d.DeviceStatus()
		if err != nil {
			return fmt.Errorf("device status: %w", err)
		}
		if d.MTPDebug {
			log.Printf("MTP device status 0x%04x", st)
		}
		if st == RC_OK {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("device did not become ready after cancel")
}

// DeviceReset issues the PTP Device_Reset class request (not a USB port reset).
func (d *Device) DeviceReset() error {
	if d.h == nil {
		return fmt.Errorf("mtp: DeviceReset: device not open")
	}
	return d.h.ControlTransfer(0x21, reqDeviceReset, 0, uint16(d.ifaceDescr.InterfaceNumber), nil, d.Timeout)
}

// ClearHalts clears stall conditions on both bulk endpoints.
func (d *Device) ClearHalts() {
	if d.h == nil {
		return
	}
	_ = d.h.ClearHalt(d.fetchEP)
	_ = d.h.ClearHalt(d.sendEP)
}

// IsOpen reports whether the USB handle is still open.
func (d *Device) IsOpen() bool { return d.h != nil }
