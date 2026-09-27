// Package mtpdev wraps github.com/hanwen/go-mtpfs/mtp with a
// goroutine-safe, error-normalising API tailored for a file-system bridge.
package mtpdev

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hanwen/usb"
	"shioriko/internal/mtp"
)

// ParentRoot is the MTP object handle used to denote the storage root.
const ParentRoot uint32 = 0xFFFFFFFF

// MaxSendObjectSize is the largest file we push with a plain SendObject.
// Larger files (or files of unknown size) use the Android partial-object
// extension when available.
const MaxSendObjectSize = int64(0xFFFFFFFF) - 12

// MTP / Android operation codes not exported (or exported buggily) by the library.
const (
	opGetPartialObject       = 0x101B
	opMoveObject             = 0x1019
	opGetObjectPropList      = 0x9805
	opAndroidGetPartial64    = 0x95C1
	opAndroidSendPartial     = 0x95C2
	opAndroidBeginEditObject = 0x95C4
	opAndroidEndEditObject   = 0x95C5

	dpcDeviceFriendlyName = 0xD402

	propObjectFormat   = 0xDC02
	propObjectSize     = 0xDC04
	propObjectFileName = 0xDC07
	propDateModified   = 0xDC09
	propParentObject   = 0xDC0B

	rcInvalidStorageID    = 0x2008
	rcInvalidParentObject = 0x201A
	rcPartialDeletion     = 0x2012
	rcSpecByGroupUnsupp   = 0xA807
	rcSpecByDepthUnsupp   = 0xA808
)

var (
	// ErrNoDevice is returned by Open when no MTP device is attached.
	ErrNoDevice = errors.New("mtp: no MTP device found")
	// ErrDeviceLost is returned once the USB connection has gone away.
	ErrDeviceLost = errors.New("mtp: device disconnected")
	// ErrBusy is returned by Open when another process owns the device.
	ErrBusy = errors.New("mtp: device is busy (claimed by another process)")
)

// defaultTimeoutMS is the per-transaction USB timeout. It must be generous:
// large directory listings and the device waking from sleep can take seconds.
const defaultTimeoutMS = 20000

// sessionTimeoutMS bounds the OpenSession / CloseSession handshake in Open.
const sessionTimeoutMS = 3000

// maxRetries is how many times an operation that times out is retried after a
// cancel+drain recovery before giving up.
const maxRetries = 2

// Storage describes one MTP storage (internal memory, SD card, ...).
type Storage struct {
	ID          uint32
	Description string
	Capacity    uint64
	Free        uint64
	ReadOnly    bool
}

// Object is a file or folder on the device.
type Object struct {
	Handle    uint32
	StorageID uint32
	Parent    uint32
	Name      string
	IsDir     bool
	Size      int64
	ModTime   time.Time
}

// Device is an opened MTP device with a session.
type Device struct {
	mu   sync.Mutex
	dev  *mtp.Device
	ops  map[uint16]bool
	lost atomic.Bool

	// bytesRead / bytesWritten count file content moved over USB, for
	// showing transfer activity.
	bytesRead    atomic.Int64
	bytesWritten atomic.Int64

	Info mtp.DeviceInfo
	Name string
	// Location is where the device is attached on the USB bus.
	Location mtp.Location
	Verbose  bool
}

// Open finds the (single) attached MTP device and opens a session.
func Open(pattern string) (*Device, error) {
	raw, err := mtp.SelectDevice(pattern)
	if err != nil {
		return nil, classifySelectError(err)
	}
	raw.Timeout = defaultTimeoutMS
	if os.Getenv("SHIORIKO_MTPDEBUG") != "" {
		raw.MTPDebug = true
		raw.USBDebug = true
	}
	// SelectDevice has opened the USB handle and claimed the interface, but a
	// previous host process (notably macOS ptpcamerad) may have left the
	// device mid-transaction. Clear that before opening our session.
	raw.DrainIn(300)
	if err := raw.CancelPending(); err != nil {
		// Not fatal: a freshly-attached device has nothing to cancel and may
		// reject the request. Log only in verbose mode.
	}
	if err := openSession(raw); err != nil {
		raw.Close()
		raw.Done()
		return nil, fmt.Errorf("mtp: open session: %w", err)
	}
	d := &Device{dev: raw, ops: map[uint16]bool{}, Location: raw.Location()}
	if err := d.call(func() error { return raw.GetDeviceInfo(&d.Info) }); err != nil {
		d.Close()
		return nil, fmt.Errorf("mtp: GetDeviceInfo: %w", err)
	}
	for _, op := range d.Info.OperationsSupported {
		d.ops[op] = true
	}
	d.Name = d.friendlyName()
	return d, nil
}

// classifySelectError maps mtp.SelectDevice failures to ErrNoDevice and
// ErrBusy where they mean that.
func classifySelectError(err error) error {
	msg := err.Error()
	switch {
	// Check claim failures first: when every candidate fails to claim, the
	// error reads "no MTP devices found (claim failed: ...ACCESS)", but the
	// phone is there and held by another program.
	case errors.Is(err, usb.ERROR_ACCESS), errors.Is(err, usb.ERROR_BUSY),
		strings.Contains(msg, "LIBUSB_ERROR_ACCESS"), strings.Contains(msg, "LIBUSB_ERROR_BUSY"):
		return ErrBusy
	case strings.Contains(msg, "no MTP devices found"), strings.Contains(msg, "no device matched"):
		return ErrNoDevice
	}
	return err
}

// openSession opens an MTP session, recovering from the states a previous
// host process commonly leaves behind.
//
// On macOS ptpcamerad opens a session on every attached device; after we
// evict it the device still believes that session is open and answers
// OpenSession with SessionAlreadyOpened. Android accepts CloseSession
// without a valid session ID, so close the stale session and open ours.
func openSession(raw *mtp.Device) error {
	// A device still busy with the previous host may not answer the first
	// CloseSession at all. Fail fast here so the poll loop retries within a
	// few seconds instead of waiting out the full transfer timeout.
	saved := raw.Timeout
	raw.Timeout = sessionTimeoutMS
	defer func() { raw.Timeout = saved }()

	err := raw.OpenSession()
	if err == nil {
		return nil
	}
	if isSessionAlreadyOpened(err) {
		raw.CloseSession()
		if err = raw.OpenSession(); err == nil {
			return nil
		}
	}
	// Retry once after another drain: some firmwares need it.
	raw.DrainIn(300)
	raw.CancelPending()
	err2 := raw.OpenSession()
	if err2 != nil && isSessionAlreadyOpened(err2) {
		raw.CloseSession()
		err2 = raw.OpenSession()
	}
	if err2 != nil {
		return err
	}
	return nil
}

func isSessionAlreadyOpened(err error) bool {
	rc, ok := err.(mtp.RCError)
	return ok && uint16(rc) == mtp.RC_SessionAlreadyOpened
}

func (d *Device) friendlyName() string {
	var s mtp.StringValue
	if err := d.dev.GetDevicePropValue(dpcDeviceFriendlyName, &s); err == nil {
		if n := strings.TrimSpace(s.Value); n != "" {
			return n
		}
	}
	n := strings.TrimSpace(d.Info.Manufacturer + " " + d.Info.Model)
	if n == "" {
		n = "Android"
	}
	return n
}

// Close ends the session and releases the USB device.
func (d *Device) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.dev != nil {
		d.dev.Close()
		d.dev.Done()
		d.dev = nil
	}
	d.lost.Store(true)
}

// Lost reports whether the device connection has been lost.
func (d *Device) Lost() bool { return d.lost.Load() }

// Supports reports whether the device advertises the given operation.
func (d *Device) Supports(op uint16) bool { return d.ops[op] }

// SupportsPartialWrite reports whether streaming uploads of unknown / huge size work.
func (d *Device) SupportsPartialWrite() bool {
	return d.ops[opAndroidBeginEditObject] && d.ops[opAndroidSendPartial] && d.ops[opAndroidEndEditObject]
}

// SupportsMove reports whether MoveObject is available.
func (d *Device) SupportsMove() bool { return d.ops[opMoveObject] }

func (d *Device) logf(format string, args ...interface{}) {
	if d.Verbose {
		log.Printf("mtp: "+format, args...)
	}
}

// wrap normalises library errors into fs / syscall errors and detects disconnects.
func (d *Device) wrap(err error) error {
	if err == nil {
		return nil
	}
	if ue, ok := err.(usb.Error); ok {
		if ue == usb.ERROR_TIMEOUT {
			// A transfer timeout is recoverable (see call); only other
			// libusb errors mean the device is gone.
			return syscall.ETIMEDOUT
		}
		d.lost.Store(true)
		return ErrDeviceLost
	}
	if _, ok := err.(mtp.SyncError); ok {
		d.lost.Store(true)
		return ErrDeviceLost
	}
	if strings.Contains(err.Error(), "device is not open") {
		d.lost.Store(true)
		return ErrDeviceLost
	}
	if rc, ok := err.(mtp.RCError); ok {
		switch uint16(rc) {
		case mtp.RC_InvalidObjectHandle, rcInvalidParentObject, rcInvalidStorageID:
			return fs.ErrNotExist
		case mtp.RC_StoreFull:
			return syscall.ENOSPC
		case mtp.RC_AccessDenied, mtp.RC_ObjectWriteProtected, mtp.RC_StoreReadOnly:
			return fs.ErrPermission
		case mtp.RC_OperationNotSupported, rcSpecByGroupUnsupp, rcSpecByDepthUnsupp:
			return errors.ErrUnsupported
		}
	}
	return err
}

// recover clears a stuck data phase after an aborted transfer by draining the
// bulk-IN endpoint and issuing a PTP Cancel, keeping the existing session.
func (d *Device) recover() {
	if d.dev == nil || !d.dev.IsOpen() {
		d.lost.Store(true)
		return
	}
	d.logf("recovering after aborted transfer")
	d.dev.DrainIn(300)
	if err := d.dev.CancelPending(); err != nil {
		d.logf("recovery cancel failed: %v", err)
	}
}

// call runs an MTP operation, recovering from transaction timeouts by draining
// the bulk-IN endpoint and issuing a PTP Cancel, then retrying. It assumes
// d.mu is held and the device is open.
func (d *Device) call(fn func() error) error {
	var err error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		err = fn()
		if err == nil {
			return nil
		}
		werr := d.wrap(err)
		if errors.Is(werr, ErrDeviceLost) {
			return werr
		}
		// RCErrors are real protocol answers, not sync problems: return them.
		if _, isRC := err.(mtp.RCError); isRC {
			return werr
		}
		if !isTimeoutErr(err) {
			return werr
		}
		// Timeout: the device may be mid data-phase. Recover and retry.
		if !d.dev.IsOpen() {
			d.lost.Store(true)
			return ErrDeviceLost
		}
		d.logf("operation timed out (attempt %d); draining + cancelling", attempt+1)
		d.dev.DrainIn(300)
		if cerr := d.dev.CancelPending(); cerr != nil {
			d.logf("cancel after timeout failed: %v", cerr)
		}
	}
	return d.wrap(err)
}

func isTimeoutErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "TIMEOUT")
}

func (d *Device) ensure() error {
	if d.lost.Load() || d.dev == nil {
		return ErrDeviceLost
	}
	return nil
}

// Ping performs a cheap round-trip so disconnects are noticed even when idle.
// It does nothing if the device is busy with another operation.
func (d *Device) Ping() error {
	if !d.mu.TryLock() {
		return nil
	}
	defer d.mu.Unlock()
	if err := d.ensure(); err != nil {
		return err
	}
	var ids mtp.Uint32Array
	return d.call(func() error { return d.dev.GetStorageIDs(&ids) })
}

// Storages lists the device storages.
func (d *Device) Storages() ([]Storage, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.storages()
}

// TryStorages is like Storages but gives up immediately (ok == false) when
// another operation, such as a long transfer, is using the device.
func (d *Device) TryStorages() (st []Storage, ok bool, err error) {
	if !d.mu.TryLock() {
		return nil, false, nil
	}
	defer d.mu.Unlock()
	st, err = d.storages()
	return st, true, err
}

func (d *Device) storages() ([]Storage, error) {
	if err := d.ensure(); err != nil {
		return nil, err
	}
	var ids mtp.Uint32Array
	if err := d.call(func() error { return d.dev.GetStorageIDs(&ids) }); err != nil {
		return nil, err
	}
	var out []Storage
	for _, id := range ids.Values {
		var si mtp.StorageInfo
		if err := d.call(func() error { return d.dev.GetStorageInfo(id, &si) }); err != nil {
			if errors.Is(err, ErrDeviceLost) {
				return nil, err
			}
			d.logf("GetStorageInfo(%x): %v", id, err)
			continue
		}
		desc := strings.TrimSpace(si.StorageDescription)
		if desc == "" {
			desc = strings.TrimSpace(si.VolumeLabel)
		}
		if desc == "" {
			desc = fmt.Sprintf("Storage %08X", id)
		}
		out = append(out, Storage{
			ID:          id,
			Description: desc,
			Capacity:    si.MaxCapability,
			Free:        si.FreeSpaceInBytes,
			ReadOnly:    si.AccessCapability != mtp.AC_ReadWrite,
		})
	}
	return out, nil
}

// List returns the direct children of parent (ParentRoot for the storage root).
func (d *Device) List(storageID, parent uint32) ([]Object, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensure(); err != nil {
		return nil, err
	}
	if parent != ParentRoot && d.ops[opGetObjectPropList] {
		objs, err := d.listPropList(parent)
		if err == nil {
			return objs, nil
		}
		if errors.Is(d.wrap(err), ErrDeviceLost) {
			return nil, ErrDeviceLost
		}
		d.logf("GetObjectPropList(%x) failed (%v); falling back to GetObjectInfo", parent, err)
	}
	var handles mtp.Uint32Array
	if err := d.call(func() error { return d.dev.GetObjectHandles(storageID, 0, parent, &handles) }); err != nil {
		return nil, err
	}
	out := make([]Object, 0, len(handles.Values))
	for _, h := range handles.Values {
		obj, err := d.getInfo(h)
		if err != nil {
			if errors.Is(err, ErrDeviceLost) {
				return nil, err
			}
			d.logf("GetObjectInfo(%x): %v", h, err)
			continue
		}
		out = append(out, obj)
	}
	return out, nil
}

// GetInfo fetches metadata for a single object.
func (d *Device) GetInfo(handle uint32) (Object, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensure(); err != nil {
		return Object{}, err
	}
	return d.getInfo(handle)
}

func (d *Device) getInfo(handle uint32) (Object, error) {
	var oi mtp.ObjectInfo
	if err := d.call(func() error { return d.dev.GetObjectInfo(handle, &oi) }); err != nil {
		return Object{}, err
	}
	obj := Object{
		Handle:    handle,
		StorageID: oi.StorageID,
		Parent:    oi.ParentObject,
		Name:      oi.Filename,
		IsDir:     oi.ObjectFormat == mtp.OFC_Association,
		Size:      int64(oi.CompressedSize),
		ModTime:   localize(oi.ModificationDate),
	}
	if oi.CompressedSize == 0xFFFFFFFF && !obj.IsDir {
		var v mtp.Uint64Value
		if err := d.dev.GetObjectPropValue(handle, propObjectSize, &v); err == nil {
			obj.Size = int64(v.Value)
		}
	}
	if obj.IsDir {
		obj.Size = 0
	}
	return obj, nil
}

// localize reinterprets a UTC-decoded MTP timestamp (which carries no zone) as local time.
func localize(t time.Time) time.Time {
	if t.IsZero() {
		return time.Time{}
	}
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.Local)
}

// listPropList uses GetObjectPropList (one transaction per folder) to fetch
// name/size/format/date of all children at once.
func (d *Device) listPropList(parent uint32) ([]Object, error) {
	var req, rep mtp.Container
	req.Code = opGetObjectPropList
	req.Param = []uint32{parent, 0, 0xFFFFFFFF, 0, 1}
	var buf bytes.Buffer
	if err := d.call(func() error {
		buf.Reset()
		return d.dev.RunTransaction(&req, &rep, &buf, nil, 0)
	}); err != nil {
		return nil, err
	}
	return decodePropList(buf.Bytes(), parent)
}

type propObj struct {
	Object
	format  uint16
	gotFmt  bool
	gotName bool
}

func decodePropList(b []byte, parent uint32) ([]Object, error) {
	r := bytes.NewReader(b)
	var count uint32
	if err := binary.Read(r, binary.LittleEndian, &count); err != nil {
		return nil, err
	}
	objs := map[uint32]*propObj{}
	var order []uint32
	for i := uint32(0); i < count; i++ {
		var hdr struct {
			Handle uint32
			Prop   uint16
			Type   uint16
		}
		if err := binary.Read(r, binary.LittleEndian, &hdr); err != nil {
			return nil, fmt.Errorf("proplist: element %d: %w", i, err)
		}
		val, err := readPropValue(r, hdr.Type)
		if err != nil {
			return nil, fmt.Errorf("proplist: element %d value: %w", i, err)
		}
		o := objs[hdr.Handle]
		if o == nil {
			o = &propObj{Object: Object{Handle: hdr.Handle, Parent: parent}}
			objs[hdr.Handle] = o
			order = append(order, hdr.Handle)
		}
		switch hdr.Prop {
		case propObjectFileName:
			if s, ok := val.(string); ok {
				o.Name = s
				o.gotName = true
			}
		case propObjectSize:
			if n, ok := val.(uint64); ok {
				o.Size = int64(n)
			}
		case propObjectFormat:
			if f, ok := val.(uint64); ok {
				o.format = uint16(f)
				o.gotFmt = true
			}
		case propDateModified:
			if s, ok := val.(string); ok {
				o.ModTime = parseMTPTime(s)
			}
		case propParentObject:
			if p, ok := val.(uint64); ok {
				o.Parent = uint32(p)
			}
		case 0xDC01: // StorageID
			if p, ok := val.(uint64); ok {
				o.StorageID = uint32(p)
			}
		}
	}
	out := make([]Object, 0, len(order))
	for _, h := range order {
		o := objs[h]
		if !o.gotName || !o.gotFmt {
			return nil, fmt.Errorf("proplist: incomplete metadata for %x", h)
		}
		if o.Parent != parent {
			// Some firmwares ignore depth and return unrelated objects.
			return nil, fmt.Errorf("proplist: object %x has parent %x, want %x", h, o.Parent, parent)
		}
		o.IsDir = o.format == mtp.OFC_Association
		if o.IsDir {
			o.Size = 0
		}
		out = append(out, o.Object)
	}
	return out, nil
}

// readPropValue decodes a single MTP typed value. Integers are returned as uint64
// (sign-extended values are not needed for the properties we care about),
// strings as string, arrays / 128-bit values as []byte.
func readPropValue(r *bytes.Reader, typ uint16) (interface{}, error) {
	size := func(t uint16) int {
		switch t & 0x00FF {
		case 0x01, 0x02:
			return 1
		case 0x03, 0x04:
			return 2
		case 0x05, 0x06:
			return 4
		case 0x07, 0x08:
			return 8
		case 0x09, 0x0A:
			return 16
		}
		return 0
	}
	switch {
	case typ == 0xFFFF:
		var n uint8
		if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
			return nil, err
		}
		if n == 0 {
			return "", nil
		}
		u16 := make([]uint16, n)
		if err := binary.Read(r, binary.LittleEndian, u16); err != nil {
			return nil, err
		}
		return decodeUTF16(u16), nil
	case typ&0x4000 != 0:
		var n uint32
		if err := binary.Read(r, binary.LittleEndian, &n); err != nil {
			return nil, err
		}
		sz := size(typ)
		if sz == 0 {
			return nil, fmt.Errorf("unknown array type %x", typ)
		}
		buf := make([]byte, int(n)*sz)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return buf, nil
	default:
		sz := size(typ)
		if sz == 0 {
			return nil, fmt.Errorf("unknown data type %x", typ)
		}
		buf := make([]byte, sz)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		if sz == 16 {
			return buf, nil
		}
		var v uint64
		for i := sz - 1; i >= 0; i-- {
			v = v<<8 | uint64(buf[i])
		}
		return v, nil
	}
}

func decodeUTF16(u []uint16) string {
	// strip terminating NUL(s)
	for len(u) > 0 && u[len(u)-1] == 0 {
		u = u[:len(u)-1]
	}
	var sb strings.Builder
	for i := 0; i < len(u); i++ {
		c := rune(u[i])
		if c >= 0xD800 && c < 0xDC00 && i+1 < len(u) {
			c2 := rune(u[i+1])
			if c2 >= 0xDC00 && c2 < 0xE000 {
				c = (c-0xD800)<<10 + (c2 - 0xDC00) + 0x10000
				i++
			}
		}
		sb.WriteRune(c)
	}
	return sb.String()
}

func parseMTPTime(s string) time.Time {
	s = strings.TrimRight(strings.TrimSpace(s), "Z")
	if i := strings.IndexByte(s, '.'); i > 0 {
		s = s[:i]
	}
	if s == "" {
		return time.Time{}
	}
	if t, err := time.ParseInLocation("20060102T150405", s, time.Local); err == nil {
		return t
	}
	if t, err := time.Parse("20060102T150405-0700", s); err == nil {
		return t
	}
	return time.Time{}
}

// ReadAt copies up to size bytes starting at offset into w.
func (d *Device) ReadAt(handle uint32, offset int64, size int, w io.Writer) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensure(); err != nil {
		return err
	}
	w = &countingWriter{w: w, n: &d.bytesRead}
	switch {
	case d.ops[opAndroidGetPartial64]:
		return d.wrapTransfer(d.dev.AndroidGetPartialObject64(handle, w, offset, uint32(size)))
	case d.ops[opGetPartialObject] && offset+int64(size) <= 0xFFFFFFFF:
		var req, rep mtp.Container
		req.Code = opGetPartialObject
		req.Param = []uint32{handle, uint32(offset), uint32(size)}
		return d.wrapTransfer(d.dev.RunTransaction(&req, &rep, w, nil, 0))
	default:
		return errors.ErrUnsupported
	}
}

// ReadAll streams the whole object into w.
func (d *Device) ReadAll(handle uint32, w io.Writer) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensure(); err != nil {
		return err
	}
	return d.wrapTransfer(d.dev.GetObject(handle, &countingWriter{w: w, n: &d.bytesRead}))
}

// Transferred reports the total file content read from and written to the
// device so far.
func (d *Device) Transferred() (read, written int64) {
	return d.bytesRead.Load(), d.bytesWritten.Load()
}

type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// wrapTransfer is like wrap but assumes a data phase may have been interrupted.
func (d *Device) wrapTransfer(err error) error {
	if err == nil {
		return nil
	}
	werr := d.wrap(err)
	if !errors.Is(werr, ErrDeviceLost) {
		if _, isRC := err.(mtp.RCError); !isRC {
			d.recover()
		}
	}
	return werr
}

// CreateFolder creates a directory and returns it.
func (d *Device) CreateFolder(storageID, parent uint32, name string) (Object, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensure(); err != nil {
		return Object{}, err
	}
	oi := mtp.ObjectInfo{
		StorageID:        storageID,
		ObjectFormat:     mtp.OFC_Association,
		ParentObject:     datasetParent(parent),
		AssociationType:  mtp.AT_GenericFolder,
		Filename:         name,
		ModificationDate: time.Now(),
	}
	_, _, h, err := d.dev.SendObjectInfo(storageID, parent, &oi)
	if err != nil {
		return Object{}, d.wrap(err)
	}
	return Object{Handle: h, StorageID: storageID, Parent: parent, Name: name, IsDir: true, ModTime: time.Now()}, nil
}

func datasetParent(p uint32) uint32 {
	if p == ParentRoot {
		return 0
	}
	return p
}

// CreateFile creates a file of exactly size bytes, streaming its content from r.
// The device is locked for the whole duration of the transfer.
func (d *Device) CreateFile(storageID, parent uint32, name string, size int64, mod time.Time, r io.Reader) (Object, error) {
	if size < 0 || size > MaxSendObjectSize {
		return Object{}, fmt.Errorf("mtp: CreateFile: size %d out of range", size)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensure(); err != nil {
		return Object{}, err
	}
	if mod.IsZero() {
		mod = time.Now()
	}
	oi := mtp.ObjectInfo{
		StorageID:        storageID,
		ObjectFormat:     mtp.OFC_Undefined,
		ParentObject:     datasetParent(parent),
		CompressedSize:   uint32(size),
		Filename:         name,
		ModificationDate: mod,
	}
	_, _, h, err := d.dev.SendObjectInfo(storageID, parent, &oi)
	if err != nil {
		return Object{}, d.wrap(err)
	}
	if err := d.dev.SendObject(&countingReader{r: r, n: &d.bytesWritten}, size); err != nil {
		werr := d.wrapTransfer(err)
		if !errors.Is(werr, ErrDeviceLost) {
			_ = d.dev.DeleteObject(h) // best effort cleanup of the partial object
		}
		return Object{}, werr
	}
	return Object{Handle: h, StorageID: storageID, Parent: parent, Name: name, Size: size, ModTime: mod}, nil
}

// BeginPartialFile creates an empty file and opens it for partial writes.
func (d *Device) BeginPartialFile(storageID, parent uint32, name string, mod time.Time) (Object, error) {
	if !d.SupportsPartialWrite() {
		return Object{}, errors.ErrUnsupported
	}
	obj, err := d.CreateFile(storageID, parent, name, 0, mod, bytes.NewReader(nil))
	if err != nil {
		return Object{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensure(); err != nil {
		return Object{}, err
	}
	if err := d.dev.AndroidBeginEditObject(obj.Handle); err != nil {
		_ = d.dev.DeleteObject(obj.Handle)
		return Object{}, d.wrap(err)
	}
	return obj, nil
}

// WritePartial appends/overwrites data at offset in a file opened with BeginPartialFile.
func (d *Device) WritePartial(handle uint32, offset int64, data []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensure(); err != nil {
		return err
	}
	return d.wrapTransfer(d.dev.AndroidSendPartialObject(handle, offset, uint32(len(data)), &countingReader{r: bytes.NewReader(data), n: &d.bytesWritten}))
}

// EndPartialFile commits a partial-write session.
func (d *Device) EndPartialFile(handle uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensure(); err != nil {
		return err
	}
	return d.wrap(d.dev.AndroidEndEditObject(handle))
}

// Delete removes an object. Folders may or may not be deleted recursively by
// the device; callers should fall back to explicit recursion on error.
func (d *Device) Delete(handle uint32) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensure(); err != nil {
		return err
	}
	return d.wrap(d.dev.DeleteObject(handle))
}

// Rename changes an object's file name within its folder.
func (d *Device) Rename(handle uint32, newName string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensure(); err != nil {
		return err
	}
	return d.wrap(d.dev.SetObjectPropValue(handle, propObjectFileName, &mtp.StringValue{Value: newName}))
}

// Move relocates an object to another folder on the same storage.
func (d *Device) Move(handle, storageID, newParent uint32) error {
	if !d.SupportsMove() {
		return errors.ErrUnsupported
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.ensure(); err != nil {
		return err
	}
	var req, rep mtp.Container
	req.Code = opMoveObject
	req.Param = []uint32{handle, storageID, newParent}
	return d.wrap(d.dev.RunTransaction(&req, &rep, nil, nil, 0))
}
