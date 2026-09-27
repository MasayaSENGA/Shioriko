#!/bin/bash
# Install a LaunchAgent that runs shioriko headless (without the menu bar UI)
# at login. The menu bar app has its own "Launch at login" setting instead.
set -euo pipefail
BIN="${1:-$HOME/.local/bin/shioriko}"
LABEL="com.love-live.jp.shioriko"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
LOGDIR="$HOME/Library/Logs"

if [[ ! -x "$BIN" ]]; then
  echo "error: $BIN not found or not executable. Run 'make autostart-install'." >&2
  exit 1
fi

mkdir -p "$HOME/Library/LaunchAgents" "$LOGDIR"
cat > "$PLIST" <<PL
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$LABEL</string>
    <key>ProgramArguments</key>
    <array>
        <string>$BIN</string>
        <string>-headless</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ProcessType</key>
    <string>Interactive</string>
    <key>StandardOutPath</key>
    <string>$LOGDIR/shioriko.log</string>
    <key>StandardErrorPath</key>
    <string>$LOGDIR/shioriko.log</string>
</dict>
</plist>
PL

launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
# Enable first: bootstrapping a job that was disabled earlier fails.
launchctl enable "gui/$(id -u)/$LABEL"
launchctl bootstrap "gui/$(id -u)" "$PLIST"
echo "Installed and started LaunchAgent $LABEL."
echo "Logs: $LOGDIR/shioriko.log"
