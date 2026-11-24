cask "yat3" do
  version "0.1.0"
  sha256 "846d2fdc3c3b8d580ab6ad977e295592b8a0c8e36f9e0431f9abee9cd2adeab2"

  url "https://github.com/jadewisemann/yet-another-t3-code/releases/download/v#{version}/YAT3-macos-arm64.zip"
  name "YAT3"
  desc "Native GPUI client for the T3 Code server"
  homepage "https://github.com/jadewisemann/yet-another-t3-code"

  depends_on arch: :arm64
  depends_on macos: :monterey

  app "YAT3.app"
  binary "#{appdir}/YAT3.app/Contents/MacOS/yat3"

  # The app is ad-hoc signed, not notarized; clear quarantine so it opens without a prompt.
  postflight_steps do
    run "/usr/bin/xattr",
        args:         ["-dr", "com.apple.quarantine", "{{appdir}}/YAT3.app"],
        must_succeed: false
  end

  zap trash: "~/Library/Application Support/yat3"
end
