class LeanproxyMcp < Formula
  desc "LeanProxy-MCP - A lightweight token firewall for MCP servers"
  homepage "https://github.com/trs-80/leanproxy-mcp-bob"
  license "MIT"

  version "0.10.1"

  # Asset names carry no version: the fork's .goreleaser.yml uses an
  # unversioned name_template so the release URLs stay stable. Only the tag in
  # the path moves between releases.
  on_macos do
    on_arm do
      url "https://github.com/trs-80/leanproxy-mcp-bob/releases/download/v0.10.1/leanproxy-mcp_darwin_arm64.tar.gz"
      sha256 "976c60d86972f939841ebb53bec70931e9a630ec1229fe4200793778b74ed7b9"
    end
    on_intel do
      url "https://github.com/trs-80/leanproxy-mcp-bob/releases/download/v0.10.1/leanproxy-mcp_darwin_amd64.tar.gz"
      sha256 "0ca6696f43a17b46a0981e0bfbb158bbef0adee2c9efd2da84304bb6994462b3"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/trs-80/leanproxy-mcp-bob/releases/download/v0.10.1/leanproxy-mcp_linux_arm64.tar.gz"
      sha256 "a20987eb131f1f312be92a1c36a577a0156e32f44cf6a1b0f1ba40250ac71404"
    end
    on_intel do
      url "https://github.com/trs-80/leanproxy-mcp-bob/releases/download/v0.10.1/leanproxy-mcp_linux_amd64.tar.gz"
      sha256 "dfa5d3929c88e45199993fc57a22ce1d5b4e9cbe3548823d443bd9be3e08ffee"
    end
  end

  def install
    bin.install "leanproxy-mcp"
  end

  test do
    system "#{bin}/leanproxy-mcp", "version"
  end
end
