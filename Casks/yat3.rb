cask "yat3" do
  version "0.1.1"
  sha256 "bdeeb9a75e4e765c9158831d32e0c309f15b9c34785bc4dae390c100a0339e3f"

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
