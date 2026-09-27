package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// errAlreadyRunning means another Shioriko process holds the instance lock.
var errAlreadyRunning = errors.New("another Shioriko is already running")

// instanceLock is held for the life of the process so that only one Shioriko
// (menu bar app or headless) drives the USB device at a time. It is kept in
// a package variable so the file, and with it the lock, is never collected.
var instanceLock *os.File

// acquireInstanceLock takes the single-instance lock without blocking.
func acquireInstanceLock(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "shioriko.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errAlreadyRunning
		}
		return err
	}
	// Record our pid for diagnostics.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	instanceLock = f
	return nil
}

// lockHolder returns the pid recorded by the process holding the lock, or 0.
func lockHolder(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "shioriko.lock"))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// legacyAgentLabel is the LaunchAgent installed by scripts/install-agent.sh
// (and by versions of Shioriko before the menu bar app).
const legacyAgentLabel = "com.love-live.jp.shioriko"

func legacyAgentPlist() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents", legacyAgentLabel+".plist")
}

// launchedByLegacyAgent reports whether launchd started this process for the
// LaunchAgent. Plists written before the menu bar app existed run the binary
// without -headless; such a process must stay headless (a menu bar app with
// KeepAlive could never be quit) and must not offer to remove its own job.
func launchedByLegacyAgent() bool {
	return os.Getenv("XPC_SERVICE_NAME") == legacyAgentLabel
}

func legacyAgentInstalled() bool {
	_, err := os.Stat(legacyAgentPlist())
	return err == nil
}

// removeLegacyAgent stops the headless LaunchAgent and deletes its plist, so
// it neither runs now nor at the next login.
func removeLegacyAgent() error {
	target := fmt.Sprintf("gui/%d/%s", os.Getuid(), legacyAgentLabel)
	_ = exec.Command("/bin/launchctl", "bootout", target).Run()
	if err := os.Remove(legacyAgentPlist()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	log.Printf("removed LaunchAgent %s", legacyAgentLabel)
	return nil
}

// maxLogSize is the size at which the log is rotated to <name>.1 at startup.
const maxLogSize = 4 << 20

// openLogFile opens path for appending, rotating it first if it is large.
func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() > maxLogSize {
		_ = os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// logTo sends the standard logger to f, and also to stderr when that is a
// terminal (running the app binary by hand).
func logTo(f *os.File) {
	var w io.Writer = f
	if fi, err := os.Stderr.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		w = io.MultiWriter(f, os.Stderr)
	}
	log.SetOutput(w)
}
