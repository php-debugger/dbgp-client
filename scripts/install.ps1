<#
.SYNOPSIS
  dbgp installer (Windows).

.DESCRIPTION
  Downloads the latest dbgp release for Windows, checks it against the
  release's SHA256SUMS, installs it system-wide and adds it to the system
  PATH. Run it from PowerShell as administrator.

  Usage:
    powershell -c "irm https://github.com/php-debugger/dbgp-client/releases/latest/download/install.ps1 | iex"

  Environment overrides:
    $env:INSTALL_DIR  where to place the binary (default: %ProgramFiles%\dbgp)
    $env:VERSION      the release to install (default: the latest)
#>

$ErrorActionPreference = "Stop"

$Repo = "php-debugger/dbgp-client"
$Bin  = "dbgp.exe"
$InstallDir = if ($env:INSTALL_DIR) { $env:INSTALL_DIR } else { Join-Path $env:ProgramFiles "dbgp" }
$IsAdmin = ([Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()).IsInRole(
    [Security.Principal.WindowsBuiltInRole]::Administrator)

# The default location and the system PATH need administrator rights.
if (-not $env:INSTALL_DIR -and -not $IsAdmin) {
    throw "Installing to $InstallDir needs administrator rights: run PowerShell as administrator, or set `$env:INSTALL_DIR"
}

# Only one Windows build is published (x64); Windows-on-ARM runs it via emulation.
$Asset = "dbgp-windows-amd64.zip"

if ($env:VERSION) {
    $Base = "https://github.com/$Repo/releases/download/$($env:VERSION)"
    Write-Host "Installing dbgp $($env:VERSION) (windows/amd64)"
} else {
    # The releases/latest/download URL always resolves to the newest release.
    $Base = "https://github.com/$Repo/releases/latest/download"
    Write-Host "Installing dbgp (latest, windows/amd64)"
}

$tmp = Join-Path $env:TEMP ("dbgp-" + [System.Guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    $zip = Join-Path $tmp $Asset
    Invoke-WebRequest -UseBasicParsing -Uri "$Base/$Asset" -OutFile $zip
    $sums = Join-Path $tmp "SHA256SUMS"
    Invoke-WebRequest -UseBasicParsing -Uri "$Base/SHA256SUMS" -OutFile $sums
    # Lines are "checksum  file", as sha256sum writes them.
    $want = Get-Content $sums | ForEach-Object {
        $sum, $name = $_ -split '\s+', 2
        if ($name -and $name.TrimStart('*') -eq $Asset) { $sum }
    } | Select-Object -First 1
    if (-not $want) { throw "SHA256SUMS has no checksum for $Asset" }
    $got = (Get-FileHash -Algorithm SHA256 -Path $zip).Hash
    # -ne ignores case: Get-FileHash gives upper case, sha256sum lower.
    if ($got -ne $want) { throw "$Asset does not match its checksum (got $got, want $want): not installing it" }
    Expand-Archive -Path $zip -DestinationPath $tmp -Force

    $src = Join-Path $tmp $Bin
    if (-not (Test-Path $src)) { throw "archive did not contain $Bin" }

    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    $dest = Join-Path $InstallDir $Bin
    try {
        Move-Item -Force $src $dest
    } catch {
        throw "Cannot replace $dest, probably because dbgp is running: stop it (dbgp mcp stop for the background MCP server) and run this again"
    }
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

Write-Host ""
Write-Host "Installed $(& $dest -version) to $dest"

# Add the directory to the PATH: the system one when installing system-wide.
$scope = if ($IsAdmin) { "Machine" } else { "User" }
$path = [Environment]::GetEnvironmentVariable("Path", $scope)
if (-not $path) { $path = "" }
if (-not (($path -split ';') -contains $InstallDir)) {
    [Environment]::SetEnvironmentVariable("Path", ((@($path.TrimEnd(';'), $InstallDir) | Where-Object { $_ }) -join ';'), $scope)
    Write-Host "Added $InstallDir to the $($scope.ToLower()) PATH: open a new terminal to use dbgp."
}
