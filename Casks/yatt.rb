cask "yatt" do
  version "0.6.0"
  sha256 "28cb3c8286d06a170fa368e33d5936fbbfcf3e412b5415e380e05c5fe9591920"

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
