# Builds runwhp.exe and the Linux guest init it boots into out\.
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

go build -o out/runwhp.exe ./cmd/runwhp
if ($LASTEXITCODE) { exit $LASTEXITCODE }

$env:GOOS = 'linux'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
try {
    go build -trimpath -ldflags='-s -w' -o out/runwhp-init ./guest/init
    if ($LASTEXITCODE) { exit $LASTEXITCODE }
} finally {
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
}
Write-Host 'built out\runwhp.exe and out\runwhp-init'
