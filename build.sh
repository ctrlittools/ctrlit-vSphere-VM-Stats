#!/usr/bin/env sh
# Cross-compiles dist/ctrlit-vSphere-VM-Stats.exe from Linux or macOS. Needs Go 1.24 or newer.
set -e
cd "$(dirname "$0")"
go mod tidy
go run github.com/akavel/rsrc@v0.10.2 -manifest app.manifest -ico app.ico -arch amd64 -o rsrc_windows_amd64.syso
go test ./...
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H windowsgui" -o dist/ctrlit-vSphere-VM-Stats.exe .
echo "Built dist/ctrlit-vSphere-VM-Stats.exe"
