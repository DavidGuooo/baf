#!/bin/sh
# release.sh TAG: build the release binaries into dist/ with TAG as their
# version, and write dist/SHA256SUMS. Publishing them is a separate step.
set -eu
tag=${1:?usage: deploy/release.sh TAG (for example v0.1.0)}
cd "$(dirname "$0")/.."
rm -f dist/baf-* dist/SHA256SUMS
for target in darwin/arm64 linux/amd64 linux/arm64; do
	os=${target%/*} arch=${target#*/}
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X main.version=$tag" -o "dist/baf-$os-$arch" ./cmd/baf
done
(cd dist && shasum -a 256 baf-* > SHA256SUMS)
cat dist/SHA256SUMS
