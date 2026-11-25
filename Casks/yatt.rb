cask "yatt" do
  version "0.5.0"
  sha256 "9d1decf28279c0c9d27f26596eed4a9b387353bb11dd205c763e9b69ab1c7bc9"

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
