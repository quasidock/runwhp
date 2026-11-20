# Builds runwhp's kernel with Docker inside WSL. See "Kernel" in README.md.
#
#   .\kernel\build.ps1                    out\kernel\: bzImage, config, build-info.json
#   .\kernel\build.ps1 -Target config     regenerates kernel\full.config
#   .\kernel\build.ps1 -Target release    out\release\: what CI publishes
#
# -Distro names the WSL distro that has Docker, if it is not the default one.
param(
    [ValidateSet('kernel', 'config', 'release')]
    [string] $Target = 'kernel',
    [string] $Distro = ''
)
$ErrorActionPreference = 'Stop'
Set-Location (Split-Path -Parent $PSScriptRoot)

$wslArgs = @()
if ($Distro) { $wslArgs += @('-d', $Distro) }
$dest = "out/$Target"
if (Test-Path $dest) { Remove-Item -Recurse -Force $dest }

# -e runs docker directly rather than through the distro's login shell.
wsl @wslArgs --cd $PWD.Path -e docker build --target $Target --output "type=local,dest=$dest" kernel
if ($LASTEXITCODE) { exit $LASTEXITCODE }

if ($Target -eq 'config') {
    Copy-Item "$dest/full.config" kernel/full.config
    git diff --stat -- kernel/full.config
    Write-Host 'regenerated kernel\full.config; review the difference before committing it'
} else {
    Get-ChildItem $dest | Format-Table Name, Length -AutoSize
}
