package mtp

import (
	"testing"

	"github.com/hanwen/usb"
)

// TestFindDevicesRepeatedly exercises enumeration and list freeing (including
// the empty-list path) many times; with a leak or double free libusb or the
// allocator would abort. It only enumerates: nothing is opened or claimed.
func TestFindDevicesRepeatedly(t *testing.T) {
	for i := 0; i < 200; i++ {
		c := usb.NewContext()
		devs, err := FindDevices(c)
		if err != nil {
			c.Exit()
			t.Fatal(err)
		}
		for _, d := range devs {
			d.Done()
			d.Done() // idempotent
		}
		c.Exit()
	}
	if _, err := AttachedLocations(); err != nil {
		t.Fatal(err)
	}
}

func TestFreeEmptyDeviceList(t *testing.T) {
	freeDeviceList(nil) // nil list: nothing to free, must not panic
}
