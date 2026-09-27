package davfs

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestFS returns an FS with no device and a cached storage folder holding
// one file, enough for code paths that never reach the device.
func newTestFS(t *testing.T) *FS {
	t.Helper()
	f, err := New(nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	photo := &node{path: "/S/photo.jpg", name: "photo.jpg", handle: 42, size: 1000}
	f.nodes["/S"] = &node{path: "/S", name: "S", isDir: true, isStorage: true}
	f.nodes["/S/photo.jpg"] = photo
	// A listing that stays fresh for the test, so lookups of new names
	// never go to the (missing) device.
	f.children["/S"] = &listing{at: time.Now().Add(time.Hour), items: []*node{photo}}
	return f
}

func spoolFiles(t *testing.T, f *FS) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(f.tmpDir, "*"))
	return m
}

func TestOpenForWriteWithoutTruncateKeepsContent(t *testing.T) {
	f := newTestFS(t)
	// PROPPATCH opens existing files O_RDWR; that must not become a
	// replacing write (which would commit an empty file on Close).
	h, err := f.OpenFile(context.Background(), "/S/photo.jpg", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.(*readFile); !ok {
		t.Fatalf("O_RDWR on an existing file returned %T, want *readFile", h)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	h, err = f.OpenFile(context.Background(), "/S/photo.jpg", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if w, ok := h.(*writeFile); !ok || w.replace == nil {
		t.Fatalf("O_TRUNC returned %T, want a replacing *writeFile", h)
	}
}

type failingReader struct{ n int }

func (r *failingReader) Read(p []byte) (int, error) {
	if r.n > 0 {
		r.n--
		return copy(p, "partial data"), nil
	}
	return 0, io.ErrUnexpectedEOF
}

func TestIncompleteUploadIsNotCommitted(t *testing.T) {
	f := newTestFS(t)
	h, err := f.OpenFile(context.Background(), "/S/new.mov", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	// This is how the WebDAV handler fills the file for PUT and COPY.
	if _, err := io.Copy(h, &failingReader{n: 3}); err == nil {
		t.Fatal("copy should fail")
	}
	// Close must refuse without touching the device (f.dev is nil here).
	err = h.Close()
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("Close after a failed copy = %v, want an incomplete-upload error", err)
	}
	if left := spoolFiles(t, f); len(left) != 0 {
		t.Errorf("spool files left behind: %v", left)
	}
}

func TestCancelledUploadIsNotCommitted(t *testing.T) {
	f := newTestFS(t)
	ctx, cancel := context.WithCancel(context.Background())
	h, err := f.OpenFile(ctx, "/S/photo.jpg", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.Write([]byte("half a file")); err != nil {
		t.Fatal(err)
	}
	cancel() // the client dropped the connection
	if err := h.Close(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close after cancel = %v, want context.Canceled", err)
	}
	if left := spoolFiles(t, f); len(left) != 0 {
		t.Errorf("spool files left behind: %v", left)
	}
}
