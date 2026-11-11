#!/bin/sh
# Assembles PocketBox.app with the embedded runtime and engine binaries.
#
# Inputs (all produced by earlier CI steps or committed):
#   $BUILD_DIR/PocketBox.app          Xcode/xcodebuild product
#   $RUNTIME_DIR/ish                  custom ish built for arm64-apple-ios
#   $RUNTIME_DIR/fakefsify            rootfs importer built for the same target
#   $ENGINE_BIN/pocketbox             guest engine (linux/arm64, static)
#   $ENGINE_BIN/tar, $ENGINE_BIN/gzip guest tools shipped for layer work
#
# Output: $BUILD_DIR/PocketBox.app with Frameworks/ish.framework and
# ish-bin/ populated, ready for an unsigned IPA.
set -eu

APP="${1:?usage: package-app.sh <PocketBox.app> <runtime-dir> <engine-dir>}"
RUNTIME="${2:?runtime dir (ish, fakefsify)}"
ENGINE="${3:?engine dir (pocketbox, tar, gzip)}"

FW="$APP/Frameworks/ish.framework"
mkdir -p "$FW/Modules" "$APP/ish-bin"

cp "$RUNTIME/ish" "$FW/ish"
cp "$RUNTIME/fakefsify" "$APP/ish-bin/fakefsify"
for tool in pocketbox tar gzip; do
    [ -f "$ENGINE/$tool" ] && cp "$ENGINE/$tool" "$APP/ish-bin/$tool"
done
chmod +x "$FW/ish" "$APP"/ish-bin/*

cat > "$FW/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleDevelopmentRegion</key><string>en</string>
	<key>CFBundleExecutable</key><string>ish</string>
	<key>CFBundleIdentifier</key><string>dev.pocketbox.runtime</string>
	<key>CFBundleInfoDictionaryVersion</key><string>6.0</string>
	<key>CFBundleName</key><string>ish</string>
	<key>CFBundlePackageType</key><string>FMWK</string>
	<key>CFBundleShortVersionString</key><string>1.0</string>
	<key>CFBundleVersion</key><string>1</string>
	<key>MinimumOSVersion</key><string>16.0</string>
</dict>
</plist>
PLIST

# Sanity checks that catch the two failure modes this step exists for: a
# Linux binary shipped to iOS, or a missing engine.
for bin in "$FW/ish" "$APP/ish-bin/pocketbox"; do
    if ! file "$bin" | grep -q "Mach-O 64-bit executable arm64"; then
        echo "not an arm64 Mach-O: $bin" >&2
        file "$bin" >&2
        exit 1
    fi
done
if ! file "$ENGINE/pocketbox" | grep -q "ELF 64-bit LSB executable, ARM aarch64"; then
    echo "guest engine must be a static linux/arm64 ELF: $ENGINE/pocketbox" >&2
    exit 1
fi

du -sh "$APP" "$FW" "$APP/ish-bin"
echo "packaged: $APP"
