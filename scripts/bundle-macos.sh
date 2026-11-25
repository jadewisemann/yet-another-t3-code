#!/usr/bin/env bash
# Builds YATT.app from a release binary. Usage: scripts/bundle-macos.sh [target-dir]
set -euo pipefail
cd "$(dirname "$0")/.."
target_dir="${1:-target/release}"
version="$(grep -m1 '^version' Cargo.toml | cut -d'"' -f2)"
app="$(cd "$target_dir" && pwd)/YATT.app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$target_dir/yatt" "$app/Contents/MacOS/yatt"

# Bundle yatt-server, the Go port of the T3 Code server in server/. Set YATT_SERVER_BIN to
# bundle a binary built elsewhere instead.
if [ -n "${YATT_SERVER_BIN:-}" ]; then
  cp "$YATT_SERVER_BIN" "$app/Contents/Resources/yatt-server"
else
  (cd server && CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -trimpath \
    -ldflags "-s -w -X github.com/jadewisemann/yet-another-t3-code/server/internal/server.BuildVersion=${version}" \
    -o "$app/Contents/Resources/yatt-server" ./cmd/yatt-server)
fi
cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>YATT</string>
  <key>CFBundleDisplayName</key><string>YATT</string>
  <key>CFBundleIdentifier</key><string>com.github.jadewisemann.yatt</string>
  <key>CFBundleVersion</key><string>${version}</string>
  <key>CFBundleShortVersionString</key><string>${version}</string>
  <key>CFBundleExecutable</key><string>yatt</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST
# Ad-hoc signature so Apple silicon runs it; Gatekeeper still asks on first open.
codesign --force --deep --sign - "$app"
echo "$app"
