# Download and unpack files with `go run`

    go run github.com/ncruces/go-fetch [-unpack] <url> <target>

This is useful to fetch dependencies in Go build scripts, especially on Windows.

Replaces `curl`, `wget`, `gzip`, `bzip2`, `zip`, `tar`.