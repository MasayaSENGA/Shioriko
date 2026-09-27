//go:build darwin

// Package menubar is Shioriko's macOS menu bar front end: a status item whose
// menu shows the connected phone, its storage, and transfer activity, with
// commands to open, eject, remount, and pause automatic mounting.
//
// The AppKit side lives in menubar_darwin.m. Go owns the state and wording
// (view.go) and pushes JSON snapshots to it; menu commands come back through
// the exported shkGo* functions.
package menubar

/*
#cgo CFLAGS: -x objective-c -fobjc-arc -Wno-deprecated-declarations
#cgo LDFLAGS: -framework Cocoa -framework ServiceManagement -framework UserNotifications
#include <stdlib.h>
#include "menubar.h"
*/
import "C"

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"
	"unsafe"

	"shioriko/internal/service"
)

// Pref keys shared with menubar_darwin.m.
const prefRevealOnMount = "RevealOnMount"

// quitTimeout bounds how long quitting waits for the volume to unmount. It
// covers the worst case of the service's own limits: a polite and a forced
// unmount (5s each), stopping the server (3s), and releasing the device (5s).
const quitTimeout = 20 * time.Second

// UI is the menu bar front end. There is only one per process.
type UI struct {
	mu       sync.Mutex
	svc      *service.Service
	last     string // last JSON pushed, to skip identical updates
	quitting bool
	finished bool
	onQuit   func()

	alertMu sync.Mutex
	alerts  map[int]chan int
	nextTag int
}

var current *UI

// New creates the application. It must be called on the main thread (the
// main goroutine, locked with runtime.LockOSThread) before Run.
func New(version, logPath string) *UI {
	if current != nil {
		panic("menubar: New called twice")
	}
	u := &UI{alerts: map[int]chan int{}}
	current = u
	cv, cl := C.CString(version), C.CString(logPath)
	defer C.free(unsafe.Pointer(cv))
	defer C.free(unsafe.Pointer(cl))
	C.shkInit(cv, cl)
	u.push(startingView())
	return u
}

// Run runs the AppKit event loop on the main thread. It never returns: the
// process exits after Finished is called (or the quit timeout expires).
func (u *UI) Run() {
	C.shkRun()
}

// OnQuit sets the function called when the user quits (or the system logs
// out). It should stop the service; call Finished once it has stopped.
func (u *UI) OnQuit(fn func()) {
	u.mu.Lock()
	u.onQuit = fn
	u.mu.Unlock()
}

// Attach connects the UI to a service before it starts running.
func (u *UI) Attach(svc *service.Service) {
	u.mu.Lock()
	u.svc = svc
	u.mu.Unlock()
}

func (u *UI) service() *service.Service {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.svc
}

// Finished reports that cleanup is complete; the app then exits.
func (u *UI) Finished() {
	u.mu.Lock()
	done := u.finished
	u.finished = true
	u.mu.Unlock()
	if !done {
		C.shkTerminateReply()
	}
}

// RevealOnMount reports whether the user wants new volumes opened in Finder.
func (u *UI) RevealOnMount() bool {
	k := C.CString(prefRevealOnMount)
	defer C.free(unsafe.Pointer(k))
	return C.shkPrefBool(k, 1) != 0
}

// SetStatus shows a service status. It is a service.Options.OnStatus callback.
func (u *UI) SetStatus(st service.Status) {
	u.mu.Lock()
	quitting := u.quitting
	u.mu.Unlock()
	if quitting {
		return
	}
	u.push(makeView(st))
}

// ShowMessage replaces the menu header with a message, for states outside the
// service (such as waiting for another instance to exit).
func (u *UI) ShowMessage(title, subtitle string) {
	v := startingView()
	v.Title, v.Subtitle, v.Tone = title, subtitle, "warning"
	v.CanPause = false
	u.push(v)
}

func (u *UI) push(v view) {
	if v.Storages == nil {
		v.Storages = []storageView{}
	}
	if v.Notes == nil {
		v.Notes = []string{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		log.Printf("menubar: %v", err)
		return
	}
	s := string(b)
	u.mu.Lock()
	same := s == u.last
	u.last = s
	u.mu.Unlock()
	if same {
		return
	}
	cs := C.CString(s)
	defer C.free(unsafe.Pointer(cs))
	C.shkSetStatus(cs)
}

// HandleEvent posts notifications and alerts. It is a service.Options.OnEvent
// callback.
func (u *UI) HandleEvent(e service.Event) {
	u.mu.Lock()
	quitting := u.quitting
	u.mu.Unlock()
	if quitting {
		return
	}
	name := quoted(e.Device)
	switch e.Kind {
	case service.EventMounted:
		Notify(name+"をマウントしました", e.MountPoint, e.MountPoint)
	case service.EventDisconnected:
		Notify(name+"が取り外されました", "スマホとの接続が切れたため、ボリュームを取り外しました", "")
	case service.EventFailed:
		Notify("スマホをマウントできませんでした", describeError(e.Err), "")
	case service.EventBusy:
		Notify("スマホが他のアプリで使用中です", "Android File Transfer や adb などを終了すると、自動的にマウントします", "")
	case service.EventEjectFailed:
		// Ask off the service goroutine: the answer can take a while.
		go func() {
			choice := u.Ask(context.Background(), name+"を取り出せません",
				"ボリューム上のファイルが使用中です。使用中のアプリを終了してから、もう一度お試しください。\n\n強制的に取り出すと、転送中のデータが失われることがあります。",
				"キャンセル", "強制的に取り出す", true)
			if choice == 2 {
				if svc := u.service(); svc != nil {
					svc.Eject(true)
				}
			}
		}()
	}
}

// Notify posts a user notification (if the user has them turned on).
func Notify(title, body, path string) {
	ct, cb, cp := C.CString(title), C.CString(body), C.CString(path)
	defer C.free(unsafe.Pointer(ct))
	defer C.free(unsafe.Pointer(cb))
	defer C.free(unsafe.Pointer(cp))
	C.shkNotify(ct, cb, cp)
}

// Ask shows a modal alert and returns the chosen button: 1, 2, or 0 if it was
// dismissed or ctx ended first. button2 may be empty. It blocks, so never
// call it on the main thread (from a menu action or other AppKit callback).
func (u *UI) Ask(ctx context.Context, title, message, button1, button2 string, destructive bool) int {
	ch := make(chan int, 1)
	u.alertMu.Lock()
	u.nextTag++
	tag := u.nextTag
	u.alerts[tag] = ch
	u.alertMu.Unlock()

	ct, cm, c1 := C.CString(title), C.CString(message), C.CString(button1)
	defer C.free(unsafe.Pointer(ct))
	defer C.free(unsafe.Pointer(cm))
	defer C.free(unsafe.Pointer(c1))
	var c2 *C.char
	if button2 != "" {
		c2 = C.CString(button2)
		defer C.free(unsafe.Pointer(c2))
	}
	d := C.int(0)
	if destructive {
		d = 1
	}
	C.shkAlert(C.int(tag), ct, cm, c1, c2, d)
	select {
	case b := <-ch:
		return b
	case <-ctx.Done():
		return 0
	}
}

//export shkGoAlertResult
func shkGoAlertResult(tag, button C.int) {
	u := current
	u.alertMu.Lock()
	ch := u.alerts[int(tag)]
	delete(u.alerts, int(tag))
	u.alertMu.Unlock()
	if ch != nil {
		ch <- int(button)
	}
}

//export shkGoAction
func shkGoAction(action C.int) {
	// Called on the main thread: hand off so AppKit never waits on Go.
	svc := current.service()
	if svc == nil {
		return
	}
	switch action {
	case C.SHK_ACTION_EJECT:
		go svc.Eject(false)
	case C.SHK_ACTION_MOUNT:
		go svc.Mount()
	case C.SHK_ACTION_TOGGLE_PAUSE:
		go svc.SetPaused(!svc.Status().Paused)
	}
}

//export shkGoQuit
func shkGoQuit() {
	current.Quit()
}

// Quit stops the app: it shows the quitting state, calls the OnQuit function,
// and exits once Finished is called, or after quitTimeout at the latest. The
// menu's Quit command, logout, and signals all end up here.
func (u *UI) Quit() {
	u.mu.Lock()
	if u.quitting {
		u.mu.Unlock()
		return
	}
	u.quitting = true
	fn := u.onQuit
	u.mu.Unlock()

	log.Printf("quit requested")
	u.push(quittingView())
	if fn != nil {
		go fn()
	} else {
		go u.Finished()
	}
	// Never hang on quit: a wedged unmount is cleaned up on the next launch.
	time.AfterFunc(quitTimeout, func() {
		log.Printf("cleanup is taking too long; quitting anyway")
		u.Finished()
	})
}
