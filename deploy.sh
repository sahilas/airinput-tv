#!/bin/sh
# Build airinput-tv and install it on your Android TV over adb.
#
# Usage:
#   ./deploy.sh                    # use the only device adb currently sees
#   ./deploy.sh 192.168.1.42:5555  # or name one explicitly (IP:PORT, or a serial)
#
# If the app is set up to auto-start (see "Start it automatically" in the
# README), this also restarts it: killing it is enough, because Android's init
# brings it straight back with the new binary. Without that setup, start it
# yourself after deploying — the script tells you which case you are in.
set -e
cd "$(dirname "$0")"

DEVICE="$1"

if [ -n "$DEVICE" ]; then
  # An IP needs a `connect` first; a USB serial does not. Harmless either way.
  adb connect "$DEVICE" >/dev/null 2>&1 || true
else
  # No device given: use the only one attached. Refuse to guess between several,
  # so we never deploy to the wrong TV.
  COUNT=$(adb devices | grep -cw 'device' || true)
  if [ "$COUNT" -eq 0 ]; then
    echo "No device found. Connect your TV first:" >&2
    echo "  adb connect <TV-IP>:5555" >&2
    echo "then re-run, or pass it directly:  ./deploy.sh <TV-IP>:5555" >&2
    exit 1
  fi
  if [ "$COUNT" -gt 1 ]; then
    echo "More than one device attached — say which one:" >&2
    adb devices | grep -w 'device' | sed 's/^/  /' >&2
    echo "  ./deploy.sh <serial-or-ip:port>" >&2
    exit 1
  fi
  DEVICE=$(adb devices | grep -w 'device' | cut -f1)
  echo "Using the only attached device: $DEVICE"
fi

echo "Building for linux/arm (armeabi-v7a)..."
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags="-s -w" -o airinput-tv .

echo "Pushing to $DEVICE..."
adb -s "$DEVICE" push airinput-tv /data/local/tmp/airinput-tv.new >/dev/null
# Write to .new then move, so a half-copied binary is never the live one.
adb -s "$DEVICE" shell "chmod 755 /data/local/tmp/airinput-tv.new && mv /data/local/tmp/airinput-tv.new /data/local/tmp/airinput-tv && pkill -f airinput-tv" || true

sleep 2
if adb -s "$DEVICE" shell 'pgrep -f airinput-tv >/dev/null' 2>/dev/null; then
  echo "Done — auto-start brought it back with the new build."
  adb -s "$DEVICE" shell 'tail -1 /data/local/tmp/airinput.log' 2>/dev/null || true
else
  echo "Done. Now start it:"
  echo "  adb -s $DEVICE shell /data/local/tmp/airinput-tv"
fi
