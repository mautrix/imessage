#!/bin/bash
# build.sh — Build the Extract Key SwiftUI app for Intel Macs.
#
# On an Apple Silicon Mac (M1/M2/M3/M4), this cross-compiles for x86_64.
# The resulting binary runs natively on Intel Macs.
#
# Usage:
#   cd tools/extract-key-app
#   ./build.sh
#   # Binary:  .build/release/ExtractKeyApp
#   # App:     ExtractKey.app/

set -euo pipefail
cd "$(dirname "$0")"

# 1. Assemble the XNU encrypt function for x86_64 macOS
echo "Assembling XNU encrypt function..."
ENCRYPT_S="../../rustpush/open-absinthe/src/asm/encrypt.s"
mkdir -p .build/lib

# Patch the assembly for Mach-O:
#   - Underscore prefix for global symbol (Mach-O convention)
#   - Section directives (ELF → Mach-O)
#   - Alignment directives (absolute → power-of-2)
sed \
    -e 's/\.global sub_ffffff8000ec7320/.globl _sub_ffffff8000ec7320/' \
    -e 's/sub_ffffff8000ec7320:/_sub_ffffff8000ec7320:/' \
    -e 's/^\.section \.data/.data/' \
    -e 's/^\.section \.text/.text/' \
    -e 's/\.align 0x100/.p2align 8/' \
    -e 's/\.align 0x10$/.p2align 4/' \
    "$ENCRYPT_S" > .build/encrypt_macos.s

# Assemble for x86_64 (target 11.0 to match deployment target)
clang -c -arch x86_64 -mmacosx-version-min=10.15 -o .build/encrypt.o .build/encrypt_macos.s

# Create static library
ar rcs .build/lib/libxnu_encrypt.a .build/encrypt.o
echo "  Built .build/lib/libxnu_encrypt.a"

# 2. Build the Swift app
echo ""
# Build for x86_64 regardless of host architecture
echo "Building ExtractKeyApp for x86_64 (Intel)..."
ARCH=$(uname -m)
if [ "$ARCH" = "x86_64" ]; then
    swift build -c release
else
    swift build -c release --arch x86_64
fi

BINARY=".build/release/ExtractKeyApp"
if [ ! -f "$BINARY" ]; then
    # SPM may place it in an arch-specific subdirectory
    BINARY=$(find .build -name ExtractKeyApp -type f -perm +111 2>/dev/null | head -1)
fi

if [ ! -f "$BINARY" ]; then
    echo "ERROR: Build succeeded but binary not found"
    exit 1
fi

echo ""
echo "Binary: $BINARY"
file "$BINARY"

# 3. Wrap in .app bundle
APP="ExtractKey.app"
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS"
cp "$BINARY" "$APP/Contents/MacOS/ExtractKeyApp"

cat > "$APP/Contents/Info.plist" << 'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>CFBundleIdentifier</key>
    <string>com.lrhodin.extract-key</string>
    <key>CFBundleName</key>
    <string>Extract Key</string>
    <key>CFBundleDisplayName</key>
    <string>Hardware Key Extractor</string>
    <key>CFBundleExecutable</key>
    <string>ExtractKeyApp</string>
    <key>CFBundlePackageType</key>
    <string>APPL</string>
    <key>CFBundleVersion</key>
    <string>1.0</string>
    <key>CFBundleShortVersionString</key>
    <string>1.0</string>
    <key>LSMinimumSystemVersion</key>
    <string>10.15</string>
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

# Ad-hoc sign (required for Gatekeeper on recent macOS). --deep covers nested
# resources; we let failures surface instead of swallowing them.
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
echo "To run on this Mac (under Rosetta if Apple Silicon):"
echo "  open $APP"
echo ""
echo "To run on an Intel Mac:"
echo "  Copy $APP to the Intel Mac and double-click it."
