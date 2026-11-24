cask "yat3" do
  version "0.1.0"
  sha256 :no_check

  url "https://github.com/jadewisemann/yet-another-t3-code/releases/download/v#{version}/YAT3-macos-arm64.zip"
  name "YAT3"
  desc "Native GPUI client for the T3 Code server"
  homepage "https://github.com/jadewisemann/yet-another-t3-code"

  depends_on arch: :arm64
  depends_on macos: ">= :monterey"

  app "YAT3.app"
  binary "#{appdir}/YAT3.app/Contents/MacOS/yat3"

  # The app is ad-hoc signed, not notarized; clear quarantine so it opens without a prompt.
  postflight do
    system_command "/usr/bin/xattr",
                   args: ["-dr", "com.apple.quarantine", "#{appdir}/YAT3.app"]
  end

  zap trash: "~/Library/Application Support/yat3"
end
