#!/bin/bash
# build.sh — Build the NAC Relay menubar app for Apple Silicon Macs.
#
# Compiles the Go nac-relay binary (arm64), then builds the SwiftUI
# wrapper app and bundles them together.
#
# Usage:
#   cd tools/nac-relay-app
#   ./build.sh

set -euo pipefail
cd "$(dirname "$0")"

# 1. Build the Go nac-relay binary for arm64
echo "Building Go nac-relay binary for arm64..."
RELAY_DIR="../nac-relay"
RELAY_BIN=".build/nac-relay"
mkdir -p .build

# Build with CGO for the ObjC NAC code
CGO_ENABLED=1 GOARCH=arm64 GOOS=darwin \
    go build -o "$RELAY_BIN" "$RELAY_DIR"

echo "  Built $RELAY_BIN"
file "$RELAY_BIN"

# 2. Build the Swift menubar app
echo ""
echo "Building NACRelayApp for arm64 (Apple Silicon)..."
swift build -c release --arch arm64

BINARY=".build/release/NACRelayApp"
if [ ! -f "$BINARY" ]; then
    BINARY=$(find .build -name NACRelayApp -type f -perm +111 2>/dev/null | grep -v nac-relay | head -1)
fi

if [ ! -f "$BINARY" ]; then
    echo "ERROR: Build succeeded but binary not found"
    exit 1
fi

echo ""
echo "Binary: $BINARY"
file "$BINARY"

# 3. Create .app bundle
APP="NACRelay.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS"
mkdir -p "$APP/Contents/Resources"

cp "$BINARY" "$APP/Contents/MacOS/NACRelayApp"
cp "$RELAY_BIN" "$APP/Contents/Resources/nac-relay"

cat > "$APP/Contents/Info.plist" << 'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleIdentifier</key>
    <string>com.imessage.nac-relay-app</string>
    <key>CFBundleName</key>
    <string>NAC Relay</string>
    <key>CFBundleDisplayName</key>
    <string>iMessage NAC Relay</string>
    <key>CFBundleExecutable</key>
    <string>NACRelayApp</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>CFBundleVersion</key>
    <string>1.0</string>
    <key>CFBundleShortVersionString</key>
    <string>1.0</string>
    <key>CFBundleInfoDictionaryVersion</key>
    <string>6.0</string>
    <key>LSMinimumSystemVersion</key>
    <string>13.0</string>
    <key>LSUIElement</key>
    <true/>
    <key>NSHighResolutionCapable</key>
    <true/>
</dict>
</plist>
PLIST

# Strip xattrs and AppleDouble (._*) files. If either remains when we sign,
# they get sealed in (or worse, appear later and invalidate the seal — which
# macOS reports as "the app is damaged and can't be opened").
find "$APP" -name '._*' -delete
xattr -cr "$APP"

# Ad-hoc sign (required for Gatekeeper on recent macOS). --deep covers the
# embedded nac-relay binary in Resources; we let failures surface instead of
# swallowing them.
codesign --force --deep --sign - "$APP"
codesign --verify --verbose=2 "$APP"

# Package a release-ready zip. `ditto` is the only zipper that preserves the
# exec bit reliably across the GitHub upload/download path. --norsrc/--noextattr
# prevent xattrs from being smuggled into the archive as AppleDouble entries
# that would re-materialize on extract and break the signature seal.
ZIP="${APP}.zip"
rm -f "$ZIP"
COPYFILE_DISABLE=1 ditto --norsrc --noextattr --noacl -c -k --keepParent "$APP" "$ZIP"

# Round-trip the zip to confirm what downloaders will see.
VERIFY_DIR=$(mktemp -d)
ditto -x -k "$ZIP" "$VERIFY_DIR"
codesign --verify --verbose=2 "$VERIFY_DIR/$APP"
rm -rf "$VERIFY_DIR"

echo ""
echo "App bundle: $(pwd)/$APP"
echo "Zip:        $(pwd)/$ZIP   (upload this to GitHub releases)"
echo ""
echo "To run:"
echo "  open $APP"
echo ""
echo "The app appears as a menubar icon (antenna). No dock icon."
echo "It auto-starts the NAC relay on launch."
