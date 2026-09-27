// Package davfs adapts an MTP device to the golang.org/x/net/webdav
// FileSystem interface, so the device can be served over WebDAV and mounted
// by Finder. Top-level directories correspond to the device's storages
// (internal memory, SD card); everything below maps to MTP folders and files.
package davfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/webdav"

	"shioriko/internal/mtpdev"
)

// listTTL is how long a cached directory listing is trusted before a re-fetch.
const listTTL = 3 * time.Second

// FS implements webdav.FileSystem over an MTP device.
type FS struct {
	dev *mtpdev.Device

	mu        sync.Mutex
	storages  []mtpdev.Storage
	storageAt time.Time
	// nodes is keyed by clean absolute path ("/", "/Internal storage", ...).
	nodes map[string]*node
	// children caches a directory's listing.
	children map[string]*listing
	// tmpDir holds spooled upload bodies.
	tmpDir string
}

type node struct {
	path      string
	name      string
	isDir     bool
	isStorage bool
	handle    uint32
	storageID uint32
	parent    uint32 // MTP parent handle (ParentRoot for storage root)
	size      int64
	modTime   time.Time
}

type listing struct {
	at    time.Time
	items []*node
}

// New returns a FileSystem for dev. tmpDir is used to spool uploads; it is
// created if necessary.
func New(dev *mtpdev.Device, tmpDir string) (*FS, error) {
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return nil, err
	}
	return &FS{
		dev:      dev,
		nodes:    map[string]*node{},
		children: map[string]*listing{},
		tmpDir:   tmpDir,
	}, nil
}

func clean(name string) string {
	if name == "" {
		return "/"
	}
	p := path.Clean("/" + strings.TrimPrefix(name, "/"))
	return p
}

func split(p string) (dir, base string) {
	p = clean(p)
	if p == "/" {
		return "/", ""
	}
	dir = path.Dir(p)
	base = path.Base(p)
	return dir, base
}

// storageList returns the storages, refreshing at most once per listTTL.
func (f *FS) storageList() ([]mtpdev.Storage, error) {
	if time.Since(f.storageAt) < listTTL && f.storages != nil {
		return f.storages, nil
	}
	st, err := f.dev.Storages()
	if err != nil {
		return nil, err
	}
	f.storages = st
	f.storageAt = time.Now()
	return st, nil
}

// storageNodeName makes a Finder-friendly, unique folder name for a storage.
func storageNodeName(s mtpdev.Storage, seen map[string]bool) string {
	base := s.Description
	if base == "" {
		base = fmt.Sprintf("Storage-%08X", s.ID)
	}
	base = strings.ReplaceAll(base, "/", "-")
	name := base
	for i := 2; seen[name]; i++ {
		name = fmt.Sprintf("%s (%d)", base, i)
	}
	seen[name] = true
	return name
}

// resolve returns the node for a clean path, populating the cache as it walks.
// It holds f.mu.
func (f *FS) resolve(p string) (*node, error) {
	p = clean(p)
	if p == "/" {
		return &node{path: "/", name: "", isDir: true}, nil
	}
	if n, ok := f.nodes[p]; ok {
		return n, nil
	}
	dir, base := split(p)
	items, err := f.listDir(dir)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if it.name == base {
			return it, nil
		}
	}
	return nil, os.ErrNotExist
}

// listDir returns the children of a clean directory path, using the cache.
// It holds f.mu.
func (f *FS) listDir(dir string) ([]*node, error) {
	dir = clean(dir)
	if l, ok := f.children[dir]; ok && time.Since(l.at) < listTTL {
		return l.items, nil
	}
	var items []*node
	if dir == "/" {
		st, err := f.storageList()
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, s := range st {
			name := storageNodeName(s, seen)
			n := &node{
				path:      clean("/" + name),
				name:      name,
				isDir:     true,
				isStorage: true,
				handle:    mtpdev.ParentRoot,
				storageID: s.ID,
				parent:    mtpdev.ParentRoot,
				modTime:   time.Now(),
			}
			items = append(items, n)
		}
	} else {
		parent, err := f.resolve(dir)
		if err != nil {
			return nil, err
		}
		if !parent.isDir {
			return nil, syscallNotDir()
		}
		objs, err := f.dev.List(parent.storageID, parent.handle)
		if err != nil {
			return nil, err
		}
		for _, o := range objs {
			n := &node{
				path:      childPath(dir, o.Name),
				name:      o.Name,
				isDir:     o.IsDir,
				handle:    o.Handle,
				storageID: o.StorageID,
				parent:    parent.handle,
				size:      o.Size,
				modTime:   o.ModTime,
			}
			items = append(items, n)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	// Refresh the node cache for this directory's entries.
	for _, it := range items {
		f.nodes[it.path] = it
	}
	f.children[dir] = &listing{at: time.Now(), items: items}
	return items, nil
}

func childPath(dir, name string) string {
	if dir == "/" {
		return clean("/" + name)
	}
	return clean(dir + "/" + name)
}

// invalidate drops cached listings and nodes at and under dir.
func (f *FS) invalidate(dir string) {
	dir = clean(dir)
	delete(f.children, dir)
	prefix := dir
	if prefix != "/" {
		prefix += "/"
	}
	for k := range f.children {
		if k == dir || strings.HasPrefix(k, prefix) {
			delete(f.children, k)
		}
	}
	for k := range f.nodes {
		if k == dir || strings.HasPrefix(k, prefix) {
			delete(f.nodes, k)
		}
	}
}

// --- webdav.FileSystem implementation ---

// Mkdir creates a directory.
func (f *FS) Mkdir(ctx context.Context, name string, perm os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := clean(name)
	if p == "/" {
		return os.ErrExist
	}
	dir, base := split(p)
	if dir == "/" {
		// Creating a top-level "storage" is not supported.
		return os.ErrPermission
	}
	parent, err := f.resolve(dir)
	if err != nil {
		return toDavErr(err)
	}
	if _, err := f.resolve(p); err == nil {
		return os.ErrExist
	}
	if _, err := f.dev.CreateFolder(parent.storageID, parent.handle, base); err != nil {
		return toDavErr(err)
	}
	f.invalidate(dir)
	return nil
}

// OpenFile opens or creates a file or directory.
func (f *FS) OpenFile(ctx context.Context, name string, flag int, perm os.FileMode) (webdav.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := clean(name)

	existing, rerr := f.resolve(p)
	creating := flag&os.O_CREATE != 0

	if rerr != nil && !os.IsNotExist(rerr) {
		return nil, toDavErr(rerr)
	}

	if existing != nil {
		if flag&os.O_EXCL != 0 && creating {
			return nil, os.ErrExist
		}
		if existing.isDir {
			return newDirFile(f, existing), nil
		}
		if flag&os.O_TRUNC != 0 {
			// Truncate/overwrite: model as create-new replacing the old object.
			return f.newWriteFile(ctx, p, existing)
		}
		// Opening for writing without truncating (PROPPATCH opens O_RDWR
		// just to reach the properties) must not replace the content: MTP
		// cannot modify a file in place, so hand out a read handle.
		return newReadFile(f, existing), nil
	}

	// Does not exist.
	if !creating {
		return nil, os.ErrNotExist
	}
	dir, _ := split(p)
	if dir == "/" {
		return nil, os.ErrPermission // cannot create a storage
	}
	if _, err := f.resolve(dir); err != nil {
		return nil, toDavErr(err)
	}
	return f.newWriteFile(ctx, p, nil)
}

// RemoveAll deletes a file or directory tree.
func (f *FS) RemoveAll(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := clean(name)
	if p == "/" {
		return os.ErrPermission
	}
	n, err := f.resolve(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return toDavErr(err)
	}
	if n.isStorage {
		return os.ErrPermission
	}
	if err := f.removeTree(n); err != nil {
		return toDavErr(err)
	}
	dir, _ := split(p)
	f.invalidate(dir)
	return nil
}

// removeTree deletes n, recursing into directories if the device refuses a
// non-empty delete.
func (f *FS) removeTree(n *node) error {
	err := f.dev.Delete(n.handle)
	if err == nil {
		return nil
	}
	if !n.isDir {
		return err
	}
	// Recurse.
	objs, lerr := f.dev.List(n.storageID, n.handle)
	if lerr != nil {
		return err // report the original delete error
	}
	for _, o := range objs {
		child := &node{
			isDir: o.IsDir, handle: o.Handle, storageID: o.StorageID,
			parent: n.handle, name: o.Name,
		}
		if rerr := f.removeTree(child); rerr != nil {
			return rerr
		}
	}
	return f.dev.Delete(n.handle)
}

// Rename moves or renames a file or directory.
func (f *FS) Rename(ctx context.Context, oldName, newName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	op := clean(oldName)
	np := clean(newName)
	if op == "/" || np == "/" {
		return os.ErrPermission
	}
	src, err := f.resolve(op)
	if err != nil {
		return toDavErr(err)
	}
	if src.isStorage {
		return os.ErrPermission
	}
	oldDir, _ := split(op)
	newDir, newBase := split(np)

	dstParent, err := f.resolve(newDir)
	if err != nil {
		return toDavErr(err)
	}
	if newDir == "/" {
		return os.ErrPermission
	}
	// Remove an existing destination first (WebDAV MOVE overwrites).
	if dst, derr := f.resolve(np); derr == nil {
		if dst.isDir {
			return os.ErrExist // avoid clobbering a directory
		}
		if delErr := f.dev.Delete(dst.handle); delErr != nil {
			return toDavErr(delErr)
		}
		f.invalidate(newDir)
	}

	sameDir := oldDir == newDir
	if sameDir {
		if err := f.dev.Rename(src.handle, newBase); err != nil {
			return toDavErr(err)
		}
		f.invalidate(oldDir)
		return nil
	}

	// Different directory: try MoveObject, then rename if the base changed.
	if dstParent.storageID == src.storageID && f.dev.SupportsMove() {
		if err := f.dev.Move(src.handle, dstParent.storageID, dstParent.handle); err != nil {
			if !errors.Is(err, errors.ErrUnsupported) {
				return toDavErr(err)
			}
		} else {
			if newBase != src.name {
				if rerr := f.dev.Rename(src.handle, newBase); rerr != nil {
					return toDavErr(rerr)
				}
			}
			f.invalidate(oldDir)
			f.invalidate(newDir)
			return nil
		}
	}

	// Fallback: copy across, then delete the source. Files only.
	if src.isDir {
		return toDavErr(errors.ErrUnsupported)
	}
	if err := f.copyFile(src, dstParent, newBase); err != nil {
		return toDavErr(err)
	}
	if err := f.dev.Delete(src.handle); err != nil {
		return toDavErr(err)
	}
	f.invalidate(oldDir)
	f.invalidate(newDir)
	return nil
}

// copyFile streams a file object into a new object under dstParent. f.mu held.
func (f *FS) copyFile(src *node, dstParent *node, name string) error {
	tmp, err := os.CreateTemp(f.tmpDir, "copy-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err := f.dev.ReadAll(src.handle, tmp); err != nil {
		return err
	}
	size, err := tmp.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = f.dev.CreateFile(dstParent.storageID, dstParent.handle, name, size, src.modTime, tmp)
	return err
}

// Stat returns file info for a path.
func (f *FS) Stat(ctx context.Context, name string) (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.resolve(clean(name))
	if err != nil {
		return nil, toDavErr(err)
	}
	return n.info(), nil
}
