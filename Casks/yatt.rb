cask "yatt" do
  version "0.2.0"
  sha256 "7522b2d4597ccd6545ee2a1e5fe9faab5656ecdef3852c3c5999506ac6739201"

  url "https://github.com/jadewisemann/yet-another-t3-code/releases/download/v#{version}/YATT-macos-arm64.zip"
  name "YATT"
  desc "Native GPUI client for the T3 Code server"
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

  zap trash: "~/Library/Application Support/yatt"
end
