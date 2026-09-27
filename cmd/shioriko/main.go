// Command shioriko mounts an attached Android (MTP) device as a Finder volume
// on macOS, giving a Windows-like "the phone shows up as a drive" experience.
//
// It watches USB for an MTP device, and when one appears it opens an MTP
// session, serves the device over loopback WebDAV, and mounts it under
// /Volumes so it appears in Finder. When the device is unplugged the volume is
// unmounted and shioriko goes back to waiting.
//
// By default it runs as a menu bar app (normally launched as Shioriko.app).
// With -headless it runs in the terminal and logs to stderr instead.
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"shioriko/internal/service"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func init() {
	// AppKit must run on the main thread, which is the one main starts on.
	runtime.LockOSThread()
}

type config struct {
	verbose  bool
	reveal   bool
	once     bool
	poll     time.Duration
	pattern  string
	davDebug bool
}

func main() {
	var (
		verbose  = flag.Bool("v", false, "verbose logging")
		headless = flag.Bool("headless", false, "run without the menu bar UI, logging to stderr")
		reveal   = flag.Bool("reveal", true, "with -headless: open the volume in Finder after mounting")
		once     = flag.Bool("once", false, "mount the current device and exit after it is unplugged (implies -headless)")
		pollStr  = flag.String("poll", "2s", "how often to poll for a device / check liveness")
		pattern  = flag.String("device", "", "regexp to select a device when several are attached")
		davDebug = flag.Bool("davlog", false, "log WebDAV errors")
		showVer  = flag.Bool("version", false, "print the version and exit")
	)
	flag.CommandLine.Parse(launchArgs(os.Args[1:]))

	if *showVer {
		fmt.Println("shioriko", version)
		return
	}

	poll, err := time.ParseDuration(*pollStr)
	if err != nil || poll < time.Second {
		poll = 2 * time.Second
	}
	cfg := config{
		verbose:  *verbose,
		reveal:   *reveal,
		once:     *once,
		poll:     poll,
		pattern:  *pattern,
		davDebug: *davDebug,
	}
	if *headless || *once || launchedByLegacyAgent() {
		runHeadless(cfg)
		return
	}
	runGUI(cfg)
}

// launchArgs drops arguments macOS may add when launching an app bundle
// (such as -psn_0_12345 from older Finder versions), which flag would reject.
func launchArgs(args []string) []string {
	out := args[:0:0]
	for _, a := range args {
		if strings.HasPrefix(a, "-psn_") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// serviceOptions returns the options shared by both front ends.
func (c config) serviceOptions() service.Options {
	return service.Options{
		Pattern:  c.pattern,
		Poll:     c.poll,
		Verbose:  c.verbose,
		DavLog:   c.davDebug,
		Once:     c.once,
		SpoolDir: filepath.Join(os.TempDir(), "shioriko-spool"),
		StateDir: stateDir(),
	}
}

// stateDir holds the single-instance lock and the mounted-volume record.
func stateDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "shioriko-state")
	}
	return filepath.Join(home, "Library", "Application Support", "Shioriko")
}

// logPath is where the menu bar app writes its log (and where the LaunchAgent
// installed by scripts/install-agent.sh sends the headless log).
func logPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "shioriko.log")
	}
	return filepath.Join(home, "Library", "Logs", "shioriko.log")
}
