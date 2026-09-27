// Native (AppKit) half of the menu bar UI. Implemented in menubar_darwin.m.
#ifndef SHIORIKO_MENUBAR_H
#define SHIORIKO_MENUBAR_H

// Menu actions forwarded to Go (shkGoAction). Keep in sync with menubar_darwin.go.
#define SHK_ACTION_EJECT        1
#define SHK_ACTION_MOUNT        2
#define SHK_ACTION_TOGGLE_PAUSE 3

// shkInit creates the application object. Call it on the main thread first.
void shkInit(const char *version, const char *logPath);
// shkRun runs the AppKit event loop on the main thread. It does not return;
// the process exits once termination is confirmed with shkTerminateReply.
void shkRun(void);

// The functions below are safe to call from any thread.

// shkSetStatus replaces the menu content with a JSON-encoded view (see view.go).
void shkSetStatus(const char *json);
// shkNotify posts a user notification if notifications are enabled. path, if
// not empty, is opened in Finder when the notification is clicked.
void shkNotify(const char *title, const char *body, const char *path);
// shkAlert shows a modal alert and reports the chosen button (1 or 2, 0 if
// dismissed) through shkGoAlertResult(tag, button). button2 may be NULL.
void shkAlert(int tag, const char *title, const char *message,
              const char *button1, const char *button2, int button2Destructive);
// shkTerminateReply tells AppKit that Go has finished cleaning up and the app
// may exit. If termination was not already requested, it starts it.
void shkTerminateReply(void);
// shkPrefBool reads a boolean user default, returning def if it is unset.
int shkPrefBool(const char *key, int def);

#endif
