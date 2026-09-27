package main

import (
	"errors"
	"reflect"
	"testing"
)

func TestLaunchArgs(t *testing.T) {
	got := launchArgs([]string{"-psn_0_12345", "-v", "-device", "Pixel"})
	if want := []string{"-v", "-device", "Pixel"}; !reflect.DeepEqual(got, want) {
		t.Errorf("launchArgs = %q, want %q", got, want)
	}
	if got := launchArgs(nil); len(got) != 0 {
		t.Errorf("launchArgs(nil) = %q", got)
	}
}

func TestInstanceLock(t *testing.T) {
	dir := t.TempDir()
	if err := acquireInstanceLock(dir); err != nil {
		t.Fatal(err)
	}
	first := instanceLock
	defer func() {
		first.Close()
		instanceLock = nil
	}()
	// flock locks belong to the open file, so a second open conflicts even
	// within this process.
	if err := acquireInstanceLock(dir); !errors.Is(err, errAlreadyRunning) {
		t.Fatalf("second lock: %v, want errAlreadyRunning", err)
	}
	if instanceLock != first {
		t.Error("failed attempt replaced the held lock")
	}
	if pid := lockHolder(dir); pid <= 0 {
		t.Errorf("lockHolder = %d", pid)
	}
}
