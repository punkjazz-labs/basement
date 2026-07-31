# RunOnSpark one-line installer bootstrap for Windows:
#   irm https://github.com/punkjazz-labs/runonspark-manager/releases/latest/download/setup.ps1 | iex
# Downloads the manager binary for this machine, verifies its checksum, and
# runs `runonspark-manager setup` — which discovers GB10 machines on the
# network and installs over SSH.
$ErrorActionPreference = "Stop"

$repo = "punkjazz-labs/runonspark-manager"
$base = "https://github.com/$repo/releases/latest/download"

$arch = if ([System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture -eq [System.Runtime.InteropServices.Architecture]::Arm64) { "arm64" } else { "amd64" }
$asset = "runonspark-manager-windows-$arch.exe"

$dir = Join-Path ([System.IO.Path]::GetTempPath()) ("runonspark-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $dir | Out-Null
try {
    Write-Host "Downloading RunOnSpark Manager (windows/$arch)..."
    $binary = Join-Path $dir "runonspark-manager.exe"
    Invoke-WebRequest -UseBasicParsing "$base/$asset" -OutFile $binary
    Invoke-WebRequest -UseBasicParsing "$base/$asset.sha256" -OutFile (Join-Path $dir "checksum")

    $expected = (Get-Content (Join-Path $dir "checksum") -First 1).Split(" ")[0].ToLowerInvariant()
    $actual = (Get-FileHash $binary -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($expected -notmatch '^[0-9a-f]{64}$' -or $actual -ne $expected) {
        throw "checksum verification failed"
    }

    & $binary setup @args
    exit $LASTEXITCODE
}
finally {
    Remove-Item -Recurse -Force $dir -ErrorAction SilentlyContinue
}
