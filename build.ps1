# Builds runwhp.exe, its containerd shim, and the Linux guest init they
# boot into out\.
$ErrorActionPreference = 'Stop'
Set-Location $PSScriptRoot

# A container running in the background keeps the runwhp.exe that runs it
# open, as a containerd task keeps its shim, and Windows will not overwrite
# a running program, but will rename one. Move a busy one aside, and remove
# what earlier builds moved aside once nothing runs it.
New-Item -ItemType Directory -Force out | Out-Null
foreach ($exe in 'runwhp.exe', 'containerd-shim-runwhp-v1.exe') {
    Get-ChildItem out -Filter "$exe.*.old" | ForEach-Object {
        Remove-Item $_.FullName -ErrorAction SilentlyContinue
    }
    if (Test-Path "out\$exe") {
        try {
            [IO.File]::Open("$PSScriptRoot\out\$exe", 'Open', 'ReadWrite', 'None').Close()
        } catch {
            $aside = "out\$exe.$([DateTime]::Now.Ticks).old"
            Move-Item "out\$exe" $aside
            Write-Host "moved the running out\$exe aside to $aside"
        }
    }
}

go build -o out/runwhp.exe ./cmd/runwhp
if ($LASTEXITCODE) { exit $LASTEXITCODE }
go build -o out/containerd-shim-runwhp-v1.exe ./cmd/containerd-shim-runwhp-v1
if ($LASTEXITCODE) { exit $LASTEXITCODE }

$env:GOOS = 'linux'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
try {
    go build -trimpath -ldflags='-s -w' -o out/runwhp-init ./guest/init
    if ($LASTEXITCODE) { exit $LASTEXITCODE }
} finally {
    Remove-Item Env:GOOS, Env:GOARCH, Env:CGO_ENABLED
}
Write-Host 'built out\runwhp.exe, out\containerd-shim-runwhp-v1.exe and out\runwhp-init'
