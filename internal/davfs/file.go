package davfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"os"
	"sync"
	"syscall"
	"time"

	"shioriko/internal/mtpdev"
)

// fileInfo implements os.FileInfo for an MTP node.
type fileInfo struct {
	name    string
	size    int64
	isDir   bool
	modTime time.Time
}

func (fi fileInfo) Name() string { return fi.name }
func (fi fileInfo) Size() int64  { return fi.size }
func (fi fileInfo) Mode() os.FileMode {
	if fi.isDir {
		return os.ModeDir | 0o755
	}
	return 0o644
}
func (fi fileInfo) ModTime() time.Time { return fi.modTime }
func (fi fileInfo) IsDir() bool        { return fi.isDir }
func (fi fileInfo) Sys() interface{}   { return nil }

func (n *node) info() os.FileInfo {
	name := n.name
	if n.path == "/" {
		name = "/"
	}
	return fileInfo{name: name, size: n.size, isDir: n.isDir, modTime: n.modTime}
}

func syscallNotDir() error { return &os.PathError{Op: "open", Err: syscall.ENOTDIR} }

// toDavErr normalises errors into the os.Err* / syscall values the webdav
// handler maps to HTTP status codes.
func toDavErr(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		return os.ErrNotExist
	case errors.Is(err, os.ErrPermission):
		return os.ErrPermission
	case errors.Is(err, os.ErrExist):
		return os.ErrExist
	case errors.Is(err, mtpdev.ErrDeviceLost):
		return &os.PathError{Op: "io", Err: syscall.ENODEV}
	case errors.Is(err, errors.ErrUnsupported):
		return &os.PathError{Op: "io", Err: syscall.ENOTSUP}
	case errors.Is(err, syscall.ENOSPC):
		return &os.PathError{Op: "write", Err: syscall.ENOSPC}
	}
	return err
}

// --- directory handle ---

type dirFile struct {
	fs  *FS
	n   *node
	pos int
}

func newDirFile(fs *FS, n *node) *dirFile { return &dirFile{fs: fs, n: n} }

func (d *dirFile) Close() error                      { return nil }
func (d *dirFile) Read([]byte) (int, error)          { return 0, syscall.EISDIR }
func (d *dirFile) Write([]byte) (int, error)         { return 0, syscall.EISDIR }
func (d *dirFile) Seek(int64, int) (int64, error)    { return 0, syscall.EISDIR }
func (d *dirFile) Stat() (os.FileInfo, error)        { return d.n.info(), nil }

func (d *dirFile) Readdir(count int) ([]os.FileInfo, error) {
	d.fs.mu.Lock()
	items, err := d.fs.listDir(d.n.path)
	d.fs.mu.Unlock()
	if err != nil {
		return nil, toDavErr(err)
	}
	if d.pos >= len(items) {
		if count <= 0 {
			return nil, nil
		}
		return nil, io.EOF
	}
	end := len(items)
	if count > 0 && d.pos+count < end {
		end = d.pos + count
	}
	out := make([]os.FileInfo, 0, end-d.pos)
	for _, it := range items[d.pos:end] {
		out = append(out, it.info())
	}
	d.pos = end
	return out, nil
}

// --- read handle ---

// readFile streams object content on demand, supporting Seek so Finder can
// read ranges (e.g. Quick Look reading a file's header and footer).
type readFile struct {
	fs     *FS
	n      *node
	mu     sync.Mutex
	offset int64
}

func newReadFile(fs *FS, n *node) *readFile { return &readFile{fs: fs, n: n} }

func (r *readFile) Close() error               { return nil }
func (r *readFile) Write([]byte) (int, error)  { return 0, syscall.EBADF }
func (r *readFile) Stat() (os.FileInfo, error) { return r.n.info(), nil }

func (r *readFile) Readdir(int) ([]os.FileInfo, error) { return nil, syscall.ENOTDIR }

func (r *readFile) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.offset + offset
	case io.SeekEnd:
		abs = r.n.size + offset
	default:
		return 0, syscall.EINVAL
	}
	if abs < 0 {
		return 0, syscall.EINVAL
	}
	r.offset = abs
	return abs, nil
}

func (r *readFile) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.offset >= r.n.size {
		return 0, io.EOF
	}
	want := len(p)
	if int64(want) > r.n.size-r.offset {
		want = int(r.n.size - r.offset)
	}
	w := &sliceWriter{buf: p[:want]}
	if err := r.fs.dev.ReadAt(r.n.handle, r.offset, want, w); err != nil {
		if errors.Is(err, errors.ErrUnsupported) {
			// Device lacks partial reads: fall back to a full read when at start.
			if r.offset == 0 {
				return r.readAllInto(p)
			}
		}
		return 0, toDavErr(err)
	}
	n := w.n
	r.offset += int64(n)
	if n < want {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

func (r *readFile) readAllInto(p []byte) (int, error) {
	// Only used as a fallback for devices without partial reads. Spool the
	// whole object to a temp file, then serve from it.
	tmp, err := os.CreateTemp(r.fs.tmpDir, "read-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := r.fs.dev.ReadAll(r.n.handle, tmp); err != nil {
		return 0, toDavErr(err)
	}
	if _, err := tmp.Seek(r.offset, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := tmp.Read(p)
	r.offset += int64(n)
	return n, err
}

// sliceWriter fills a fixed byte slice, ignoring overflow.
type sliceWriter struct {
	buf []byte
	n   int
}

func (s *sliceWriter) Write(p []byte) (int, error) {
	c := copy(s.buf[s.n:], p)
	s.n += c
	if c < len(p) {
		return c, io.ErrShortWrite
	}
	return c, nil
}

// --- write handle ---

// writeFile spools uploaded bytes to a temp file, then commits them to the
// device with a single SendObject on Close. This matches how Finder does a
// WebDAV PUT (write-then-close) and lets MTP receive a known object size.
type writeFile struct {
	fs *FS
	// ctx is the request's context. It is cancelled when the client drops
	// the connection mid-upload (for example a forced unmount), and then
	// the partial spool must not replace anything on the device.
	ctx       context.Context
	path      string
	name      string
	parent    *node
	replace   *node // existing object being overwritten, if any
	tmp       *os.File
	mu        sync.Mutex
	committed bool
	closed    bool
	// failed is set when spooling failed (a write error, or a read error
	// from the source while copying), so Close must not commit the file.
	failed bool
}

func (f *FS) newWriteFile(ctx context.Context, p string, existing *node) (*writeFile, error) {
	dir, base := split(p)
	parent, err := f.resolve(dir)
	if err != nil {
		return nil, toDavErr(err)
	}
	tmp, err := os.CreateTemp(f.tmpDir, "put-*")
	if err != nil {
		return nil, err
	}
	return &writeFile{fs: f, ctx: ctx, path: p, name: base, parent: parent, replace: existing, tmp: tmp}, nil
}

func (w *writeFile) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, syscall.EBADF
	}
	n, err := w.tmp.Write(p)
	if err != nil {
		w.failed = true
	}
	return n, err
}

// ReadFrom is what io.Copy uses to fill the file (PUT bodies, COPY). Unlike
// Write, it also sees errors reading the source, which the WebDAV handler
// does not pass on before it closes the file.
func (w *writeFile) ReadFrom(r io.Reader) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, syscall.EBADF
	}
	n, err := io.Copy(onlyWriter{w.tmp}, r)
	if err != nil {
		w.failed = true
	}
	return n, err
}

// onlyWriter hides other methods (such as ReadFrom) of the wrapped writer.
type onlyWriter struct{ io.Writer }

func (w *writeFile) Read([]byte) (int, error)              { return 0, syscall.EBADF }
func (w *writeFile) Readdir(int) ([]os.FileInfo, error)    { return nil, syscall.ENOTDIR }

func (w *writeFile) Seek(offset int64, whence int) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.tmp.Seek(offset, whence)
}

func (w *writeFile) Stat() (os.FileInfo, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	size := int64(0)
	if w.tmp != nil {
		if fi, err := w.tmp.Stat(); err == nil {
			size = fi.Size()
		}
	}
	return fileInfo{name: w.name, size: size, isDir: false, modTime: time.Now()}, nil
}

// Close flushes the spooled content to the device.
func (w *writeFile) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.closed = true
	defer func() {
		w.tmp.Close()
		os.Remove(w.tmp.Name())
	}()

	// The WebDAV handler closes the file even when copying the request body
	// failed, so check that the upload actually completed.
	if w.failed {
		return fmt.Errorf("davfs: upload of %s incomplete", w.path)
	}
	if w.ctx != nil && w.ctx.Err() != nil {
		return fmt.Errorf("davfs: upload of %s aborted: %w", w.path, w.ctx.Err())
	}
	size, err := w.tmp.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if size > mtpdev.MaxSendObjectSize {
		return fmt.Errorf("davfs: file too large (%d bytes)", size)
	}
	if _, err := w.tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}

	w.fs.mu.Lock()
	defer w.fs.mu.Unlock()
	// Even a failed replace may have changed the folder, so always refresh.
	dir, _ := split(w.path)
	defer w.fs.invalidate(dir)

	if w.replace != nil {
		err = w.commitReplace(size)
	} else {
		_, err = w.fs.dev.CreateFile(w.parent.storageID, w.parent.handle, w.name, size, time.Now(), w.tmp)
	}
	if err != nil {
		return toDavErr(err)
	}
	w.committed = true
	return nil
}

// commitReplace overwrites w.replace with the spooled content. MTP has no
// overwrite, so the content is first sent under a hidden temporary name and
// the original is deleted only once that transfer has succeeded; the new
// object is then renamed into place. A transfer cut short (a quit timeout, a
// forced unmount, a device error) leaves the original intact, plus at worst a
// stray temporary object. w.fs.mu held.
func (w *writeFile) commitReplace(size int64) error {
	tmpName := replaceTempName(w.name)
	obj, err := w.fs.dev.CreateFile(w.parent.storageID, w.parent.handle, tmpName, size, time.Now(), w.tmp)
	if err != nil {
		return err // CreateFile already removed the partial object
	}
	if err := w.fs.dev.Delete(w.replace.handle); err != nil {
		_ = w.fs.dev.Delete(obj.Handle) // best effort; the original stays
		return err
	}
	rerr := w.fs.dev.Rename(obj.Handle, w.name)
	if rerr == nil {
		return nil
	}

	// The device cannot rename the object: fall back to creating the file
	// under its real name, keeping the temporary copy until that succeeds.
	log.Printf("davfs: could not rename %s into place (%v); sending it again under its real name", w.path, rerr)
	if _, err := w.tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := w.fs.dev.CreateFile(w.parent.storageID, w.parent.handle, w.name, size, time.Now(), w.tmp); err != nil {
		log.Printf("davfs: could not write %s (%v); its new content is left on the device as %s", w.path, err, tmpName)
		return err
	}
	_ = w.fs.dev.Delete(obj.Handle)
	return nil
}

// replaceTempName returns a hidden sibling name for the new object while it
// is being written over name.
func replaceTempName(name string) string {
	tmp := fmt.Sprintf(".shioriko-%08x-%s", rand.Uint32(), name)
	if len(tmp) > 255 { // longest file name Android allows
		tmp = fmt.Sprintf(".shioriko-%08x", rand.Uint32())
	}
	return tmp
}
