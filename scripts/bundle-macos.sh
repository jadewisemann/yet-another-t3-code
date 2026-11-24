#!/usr/bin/env bash
# Builds YATT.app from a release binary. Usage: scripts/bundle-macos.sh [target-dir]
set -euo pipefail
cd "$(dirname "$0")/.."
target_dir="${1:-target/release}"
version="$(grep -m1 '^version' Cargo.toml | cut -d'"' -f2)"
app="$target_dir/YATT.app"
rm -rf "$app"
mkdir -p "$app/Contents/MacOS" "$app/Contents/Resources"
cp "$target_dir/yatt" "$app/Contents/MacOS/yatt"

# Bundle the T3 Code server: the self-contained build that the `t3` npm package downloads,
# checked against the release's SHA256SUMS. Bump T3_VERSION to ship a newer server; it must
# speak the orchestration protocol in src/protocol.rs.
t3_version="$(cat t3-version)"
t3_archive="t3-${t3_version}-darwin-arm64.tar.gz"
t3_base="https://github.com/pingdotgg/t3code/releases/download/v${t3_version}"
t3_cache="$target_dir/t3-cache"
mkdir -p "$t3_cache"
if [ ! -f "$t3_cache/$t3_archive" ]; then
  curl -fsSL --retry 3 -o "$t3_cache/$t3_archive.part" "$t3_base/$t3_archive"
  mv "$t3_cache/$t3_archive.part" "$t3_cache/$t3_archive"
fi
curl -fsSL --retry 3 -o "$t3_cache/SHA256SUMS" "$t3_base/SHA256SUMS"
expected="$(awk -v f="$t3_archive" '$2 == f || $2 == "*"f { print $1 }' "$t3_cache/SHA256SUMS")"
actual="$(shasum -a 256 "$t3_cache/$t3_archive" | cut -d' ' -f1)"
if [ -z "$expected" ] || [ "$expected" != "$actual" ]; then
  echo "checksum mismatch for $t3_archive" >&2
  rm -f "$t3_cache/$t3_archive"
  exit 1
fi
mkdir -p "$app/Contents/Resources/t3"
tar -xzf "$t3_cache/$t3_archive" -C "$app/Contents/Resources/t3" --strip-components=1
echo "$t3_version" > "$app/Contents/Resources/t3/.install-complete"
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
