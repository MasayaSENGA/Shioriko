// Package mount serves a webdav.FileSystem over loopback HTTP and mounts it
// into the macOS filesystem as a Finder-visible volume under /Volumes.
//
// The mount is created with AppleScript's "mount volume", which routes through
// NetAuthAgent and therefore can place the volume under /Volumes (a
// root-owned directory) without the calling process being root. The volume is
// named after the last path component of the URL, so the WebDAV tree is served
// under a path equal to the desired volume name.
package mount

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/net/webdav"
)

// Volume is a served-and-mounted WebDAV volume.
type Volume struct {
	Name       string
	Addr       string // 127.0.0.1:port
	URL        string // http://127.0.0.1:port/<name>/
	MountPoint string // /Volumes/<name>

	srv *http.Server
	ln  net.Listener
}

// sanitizeName makes a string safe as a /Volumes entry and URL path segment.
func sanitizeName(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', ':', '\\':
			return '-'
		}
		return r
	}, name)
	if name == "" {
		name = "Android"
	}
	// Keep names short, cutting on a character boundary so multi-byte
	// (for example Japanese) names stay valid UTF-8.
	for len(name) > 60 {
		_, size := utf8.DecodeLastRuneInString(name)
		name = name[:len(name)-size]
	}
	return name
}

// Serve starts a loopback WebDAV server exposing fs under a path named after
// name. It does not mount anything yet.
func Serve(fs webdav.FileSystem, name string, logger func(*http.Request, error)) (*Volume, error) {
	name = sanitizeName(name)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("mount: listen: %w", err)
	}
	h := &webdav.Handler{
		Prefix:     "/" + name,
		FileSystem: fs,
		LockSystem: webdav.NewMemLS(),
		Logger:     logger,
	}
	v := &Volume{
		Name: name,
		Addr: ln.Addr().String(),
		URL:  fmt.Sprintf("http://%s/%s/", ln.Addr().String(), url.PathEscape(name)),
		srv:  &http.Server{Handler: h},
		ln:   ln,
	}
	go v.srv.Serve(ln)
	return v, nil
}

// mountTable returns the current mounts. It uses MNT_NOWAIT so it never
// blocks on a file system, including our own WebDAV volume while it is busy.
func mountTable() ([]syscall.Statfs_t, error) {
	n, err := syscall.Getfsstat(nil, 2 /* MNT_NOWAIT */)
	if err != nil {
		return nil, err
	}
	// Leave headroom in case mounts appear between the two calls.
	buf := make([]syscall.Statfs_t, n+8)
	n, err = syscall.Getfsstat(buf, 2)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}

func cstr(b []int8) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

// serverKey is the part of a mount's source URL that identifies our server:
// "//127.0.0.1:port/". The trailing slash keeps port 5001 from matching 50012.
func serverKey(addr string) string { return "//" + addr + "/" }

// findMountPoint scans the mount table for a WebDAV mount whose source URL
// contains match (see serverKey) and returns its mount point, or "" if not
// mounted.
func findMountPoint(match string) string {
	mounts, err := mountTable()
	if err != nil {
		return ""
	}
	for i := range mounts {
		m := &mounts[i]
		if cstr(m.Fstypename[:]) != "webdav" {
			continue
		}
		if strings.Contains(cstr(m.Mntfromname[:]), match) {
			return cstr(m.Mntonname[:])
		}
	}
	return ""
}

// Mounted reports whether the volume is currently in the mount table. It
// turns false when the user ejects the volume in Finder.
func (v *Volume) Mounted() bool {
	return findMountPoint(serverKey(v.Addr)) != ""
}

// Mount asks macOS to mount the served volume. It returns once the mount point
// is available. The context bounds how long it waits.
func (v *Volume) Mount(ctx context.Context) error {
	key := serverKey(v.Addr)
	if mp := findMountPoint(key); mp != "" {
		v.MountPoint = mp
		return nil
	}
	script := fmt.Sprintf("mount volume %q", v.URL)
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", script)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount: osascript: %w: %s", err, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if mp := findMountPoint(key); mp != "" {
			v.MountPoint = mp
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("mount: volume did not appear under /Volumes")
}

// Reveal opens the mounted volume in Finder.
func (v *Volume) Reveal() {
	if v.MountPoint == "" {
		return
	}
	_ = exec.Command("/usr/bin/open", v.MountPoint).Run()
}

// Unmount unmounts the volume if mounted. It is safe to call more than once.
//
// Without force it only asks politely, and fails if files on the volume are
// in use. With force it falls back to a forced unmount, so a vanished device
// does not leave a wedged mount behind.
func (v *Volume) Unmount(force bool) error {
	mp := findMountPoint(serverKey(v.Addr))
	if mp == "" {
		v.MountPoint = ""
		return nil
	}
	if err := unmount(mp, force); err != nil {
		return err
	}
	v.MountPoint = ""
	return nil
}

// Unmount time limits. A polite unmount can hang when the volume's server or
// the phone behind it stops answering; the forced one should not, but is
// bounded too so quitting always finishes.
const (
	politeUnmountTimeout = 5 * time.Second
	forceUnmountTimeout  = 5 * time.Second
)

func unmount(mp string, force bool) error {
	err := diskutil(politeUnmountTimeout, "unmount", mp)
	if err == nil || !force {
		return err
	}
	return forceUnmount(mp)
}

func forceUnmount(mp string) error {
	return diskutil(forceUnmountTimeout, "unmount", "force", mp)
}

// diskutil runs /usr/sbin/diskutil with args, killing it after timeout.
func diskutil(timeout time.Duration, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/diskutil", args...).CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("mount: diskutil %s: timed out after %v", strings.Join(args, " "), timeout)
	}
	if err != nil {
		return fmt.Errorf("mount: diskutil %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Close unmounts the volume (forcibly if needed) and shuts down the loopback
// server.
func (v *Volume) Close() error {
	uerr := v.Unmount(true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = v.srv.Shutdown(ctx)
	_ = v.ln.Close()
	return uerr
}

// recordFile holds the URL of the volume currently mounted by this process,
// so a later run can clean it up if this one dies without unmounting.
const recordFile = "mounted-volume"

// Record notes in dir that v is mounted. Call Forget after unmounting.
func (v *Volume) Record(dir string) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, recordFile), []byte(v.URL+"\n"), 0o600)
}

// Forget removes the record written by Record.
func Forget(dir string) {
	if dir != "" {
		_ = os.Remove(filepath.Join(dir, recordFile))
	}
}

// CleanupStale force-unmounts a volume recorded by a previous run that exited
// without unmounting it (a crash or a forced quit). Its server is gone, so the
// mount is dead and would otherwise make Finder show connection errors. It
// returns the mount point it removed, or "".
func CleanupStale(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	b, err := os.ReadFile(filepath.Join(dir, recordFile))
	if err != nil {
		return "", nil
	}
	defer Forget(dir)
	u, err := url.Parse(strings.TrimSpace(string(b)))
	if err != nil || u.Host == "" {
		return "", nil
	}
	mp := findMountPoint(serverKey(u.Host))
	if mp == "" {
		return "", nil
	}
	// The server behind it is gone, so a polite unmount would only wait for
	// the WebDAV client to time out. Force it right away.
	return mp, forceUnmount(mp)
}
