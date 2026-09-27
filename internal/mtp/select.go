package mtp

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unsafe"

	"github.com/hanwen/usb"
)

func candidateFromDeviceDescriptor(d *usb.Device) *Device {
	dd, err := d.GetDeviceDescriptor()
	if err != nil {
		return nil
	}
	for i := byte(0); i < dd.NumConfigurations; i++ {
		cdecs, err := d.GetConfigDescriptor(i)
		if err != nil {
			return nil
		}
		for _, iface := range cdecs.Interfaces {
			for _, a := range iface.AltSetting {
				if len(a.EndPoints) != 3 {
					continue
				}
				m := Device{}
				for _, s := range a.EndPoints {
					switch {
					case s.Direction() == usb.ENDPOINT_IN && s.TransferType() == usb.TRANSFER_TYPE_INTERRUPT:
						m.eventEP = s.EndpointAddress
					case s.Direction() == usb.ENDPOINT_IN && s.TransferType() == usb.TRANSFER_TYPE_BULK:
						m.fetchEP = s.EndpointAddress
					case s.Direction() == usb.ENDPOINT_OUT && s.TransferType() == usb.TRANSFER_TYPE_BULK:
						m.sendEP = s.EndpointAddress
					}
				}
				if m.sendEP > 0 && m.fetchEP > 0 && m.eventEP > 0 {
					m.devDescr = *dd
					m.ifaceDescr = a
					m.dev = d.Ref()
					m.configValue = cdecs.ConfigurationValue
					return &m
				}
			}
		}
	}

	return nil
}

// FindDevices finds likely MTP devices without opening them.
func FindDevices(c *usb.Context) ([]*Device, error) {
	l, err := c.GetDeviceList()
	if err != nil {
		return nil, err
	}

	var cands []*Device
	for _, d := range l {
		cand := candidateFromDeviceDescriptor(d)
		if cand != nil {
			cands = append(cands, cand)
		}
	}
	freeDeviceList(l)

	return cands, nil
}

// freeDeviceList frees a list from GetDeviceList. DeviceList.Done takes
// &d[0], so it panics on an empty list (nothing plugged in, the usual idle
// state), yet libusb still allocated the NULL-terminated array. Free it
// through a one-element view of that terminator, which unreferences nothing.
func freeDeviceList(l usb.DeviceList) {
	if len(l) > 0 {
		l.Done()
		return
	}
	// Read the data pointer from the slice header: unsafe.SliceData is
	// unspecified for a zero-capacity slice.
	if p := *(*unsafe.Pointer)(unsafe.Pointer(&l)); p != nil {
		usb.DeviceList(unsafe.Slice((**usb.Device)(p), 1)).Done()
	}
}

// release closes and unreferences devices. It is safe on devices that were
// never opened or were already released.
func release(ds []*Device) {
	for _, d := range ds {
		d.Close()
		d.Done()
	}
}

// selectDevice finds a device that matches given pattern. It owns cands:
// every candidate except the one returned is closed and released, on success
// and on error, so a failed poll leaves nothing open or claimed.
func selectDevice(cands []*Device, pattern string) (*Device, error) {
	re, err := regexp.Compile(pattern)
	if err != nil {
		release(cands)
		return nil, err
	}

	var found []*Device
	var lastErr, claimErr error
	for _, cand := range cands {
		if err := cand.Open(); err != nil {
			// Open closes the handle itself when it fails.
			lastErr = err
			if claimErr == nil && (errors.Is(err, usb.ERROR_ACCESS) || errors.Is(err, usb.ERROR_BUSY)) {
				claimErr = err
			}
			cand.Done()
			continue
		}

		found = append(found, cand)
	}

	// An MTP device held by another program matters more than whatever
	// other candidate (a USB Ethernet adapter, say) happened to fail last.
	if len(found) == 0 {
		switch {
		case claimErr != nil:
			return nil, fmt.Errorf("no MTP devices found (claim failed: %w)", claimErr)
		case lastErr != nil:
			return nil, fmt.Errorf("no MTP devices found (last error: %w)", lastErr)
		}
		return nil, fmt.Errorf("no MTP devices found")
	}

	cands = found
	found = nil
	var ids []string
	for i, cand := range cands {
		id, err := cand.ID()
		if err != nil {
			release(cands)
			return nil, fmt.Errorf("Id dev %d: %v", i, err)
		}

		if pattern == "" || re.FindString(id) != "" {
			found = append(found, cand)
			ids = append(ids, id)
		} else {
			cand.Close()
			cand.Done()
		}
	}

	if len(found) == 0 {
		if claimErr != nil {
			return nil, fmt.Errorf("no device matched (claim failed: %w)", claimErr)
		}
		return nil, fmt.Errorf("no device matched")
	}

	if len(found) > 1 {
		release(found)
		return nil, fmt.Errorf("mtp: more than 1 device: %s", strings.Join(ids, ","))
	}

	cand := found[0]
	config, err := cand.h.GetConfiguration()
	if err != nil {
		release(found)
		return nil, fmt.Errorf("could not get configuration of %v: %v",
			ids[0], err)
	}
	if config != cand.configValue {

		if err := cand.h.SetConfiguration(cand.configValue); err != nil {
			release(found)
			return nil, fmt.Errorf("could not set configuration of %v: %v",
				ids[0], err)
		}
	}
	return found[0], nil
}

// SelectDevice returns opened MTP device that matches the given pattern.
// Call Close and then Done on it to release it (Done also frees the libusb
// context it was found in).
func SelectDevice(pattern string) (*Device, error) {
	c := usb.NewContext()

	devs, err := FindDevices(c)
	if err != nil {
		c.Exit()
		return nil, err
	}
	if len(devs) == 0 {
		// Nothing references the context; release it so polling callers do
		// not leak one libusb context per attempt.
		c.Exit()
		return nil, fmt.Errorf("no MTP devices found")
	}

	d, err := selectDevice(devs, pattern)
	if err != nil {
		// selectDevice released every candidate, so the context is unused.
		c.Exit()
		return nil, err
	}
	d.ctx = c
	return d, nil
}

// Location identifies an attached USB device by where it sits on the bus. It
// can be read without opening the device, and the address changes whenever
// the device re-enumerates (for example after being unplugged).
type Location struct {
	Bus       uint8
	Address   uint8
	VendorID  uint16
	ProductID uint16
}

func (l Location) String() string {
	return fmt.Sprintf("bus %d addr %d (%04x:%04x)", l.Bus, l.Address, l.VendorID, l.ProductID)
}

// Location reports where the device is attached. It is only valid until Done.
func (d *Device) Location() Location {
	if d.dev == nil {
		return Location{}
	}
	return Location{
		Bus:       d.dev.GetBusNumber(),
		Address:   d.dev.GetDeviceAddress(),
		VendorID:  d.devDescr.IdVendor,
		ProductID: d.devDescr.IdProduct,
	}
}

// AttachedLocations lists the likely MTP devices currently attached, without
// opening or claiming any of them.
func AttachedLocations() ([]Location, error) {
	c := usb.NewContext()
	defer c.Exit()
	devs, err := FindDevices(c)
	if err != nil {
		return nil, err
	}
	locs := make([]Location, 0, len(devs))
	for _, d := range devs {
		locs = append(locs, d.Location())
		d.Done()
	}
	return locs, nil
}
