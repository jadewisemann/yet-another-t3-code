cask "yatt" do
  version "0.4.0"
  sha256 "e5327000f98203279202c09b8b718259936477515fe1d17b82934425480e2da4"

  url "https://github.com/jadewisemann/yet-another-t3-code/releases/download/v#{version}/YATT-macos-arm64.zip"
  name "YATT"
  desc "Native GPUI client for T3 Code with its own Go server"
  homepage "https://github.com/jadewisemann/yet-another-t3-code"

  depends_on arch: :arm64
  depends_on macos: :monterey

  app "YATT.app"
  binary "#{appdir}/YATT.app/Contents/MacOS/yatt"

  # The app is ad-hoc signed, not notarized; clear quarantine so it opens without a prompt.
  postflight_steps do
    run "/usr/bin/xattr",
        args:         ["-dr", "com.apple.quarantine", "{{appdir}}/YATT.app"],
        must_succeed: false
  end

  zap trash: [
    "~/.yatt",
    "~/Library/Application Support/yatt",
  ]
end
