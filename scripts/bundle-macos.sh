#!/usr/bin/env bash
# Builds YAT3.app from a release binary. Usage: scripts/bundle-macos.sh [target-dir]
set -euo pipefail
cd "$(dirname "$0")/.."
target_dir="${1:-target/release}"
version="$(grep -m1 '^version' Cargo.toml | cut -d'"' -f2)"
app="$target_dir/YAT3.app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$target_dir/yat3" "$app/Contents/MacOS/yat3"
cat > "$app/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleName</key><string>YAT3</string>
  <key>CFBundleDisplayName</key><string>YAT3</string>
  <key>CFBundleIdentifier</key><string>com.github.jadewisemann.yat3</string>
  <key>CFBundleVersion</key><string>${version}</string>
  <key>CFBundleShortVersionString</key><string>${version}</string>
  <key>CFBundleExecutable</key><string>yat3</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>LSMinimumSystemVersion</key><string>12.0</string>
  <key>NSHighResolutionCapable</key><true/>
</dict>
</plist>
PLIST
# Ad-hoc signature so Apple silicon runs it; Gatekeeper still asks on first open.
codesign --force --deep --sign - "$app"
echo "$app"
