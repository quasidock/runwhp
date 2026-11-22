# Builds runwhp.exe and the Linux guest init it boots into out\.
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

# A container running in the background keeps the runwhp.exe that runs it
# open, and Windows will not overwrite a running program, but will rename
# one. Move a busy runwhp.exe aside, and remove what earlier builds moved
# aside once nothing runs it.
New-Item -ItemType Directory -Force out | Out-Null
Get-ChildItem out -Filter 'runwhp.exe.*.old' | ForEach-Object {
    Remove-Item $_.FullName -ErrorAction SilentlyContinue
}
if (Test-Path out\runwhp.exe) {
    try {
        [IO.File]::Open("$PSScriptRoot\out\runwhp.exe", 'Open', 'ReadWrite', 'None').Close()
    } catch {
        $aside = "out\runwhp.exe.$([DateTime]::Now.Ticks).old"
        Move-Item out\runwhp.exe $aside
        Write-Host "moved the running out\runwhp.exe aside to $aside"
    }
}

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
