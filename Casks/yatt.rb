cask "yatt" do
  version "0.3.0"
  sha256 "1280002a2a18ed13dd10d5d109375571ba99f927a999af784ee3a9f8fce2df52"

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
