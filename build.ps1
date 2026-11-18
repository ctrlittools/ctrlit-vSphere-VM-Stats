# Builds dist\ctrlit-vSphere-VM-Stats.exe. Needs Go 1.24 or newer: https://go.dev/dl/
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot
go mod tidy
# Embed the icon and the Windows manifest (DPI awareness, visual styles) in the .exe
go run github.com/akavel/rsrc@v0.10.2 -manifest app.manifest -ico app.ico -arch amd64 -o rsrc_windows_amd64.syso
go test ./...
$env:GOOS = 'windows'; $env:GOARCH = 'amd64'
go build -trimpath -ldflags "-s -w -H windowsgui" -o dist\ctrlit-vSphere-VM-Stats.exe .
Write-Host "Built dist\ctrlit-vSphere-VM-Stats.exe" -ForegroundColor Green
