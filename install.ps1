# Maestria installer for Windows (PowerShell).
# Usage: irm https://raw.githubusercontent.com/paulo-lucas/maestria/main/install.ps1 | iex
$ErrorActionPreference = "Stop"

$Repo = if ($env:MAESTRIA_REPO) { $env:MAESTRIA_REPO } else { "paulo-lucas/maestria" }
$Version = if ($env:MAESTRIA_VERSION) { $env:MAESTRIA_VERSION } else { "latest" }
$InstallDir = if ($env:MAESTRIA_INSTALL_DIR) { $env:MAESTRIA_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\maestria\bin" }
$BaseUrl = $env:MAESTRIA_BASE_URL

$arch = [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString().ToLowerInvariant()
if ($arch -ne "x64") {
    throw "Unsupported Windows architecture: $arch (supported: x64/amd64)"
}

if ($Version -eq "latest") {
    if ($BaseUrl) {
        throw "MAESTRIA_VERSION must be set explicitly when MAESTRIA_BASE_URL is overridden"
    }
    $release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers @{ "User-Agent" = "maestria-installer" }
    $Version = $release.tag_name
    if (-not $Version) {
        throw "Could not resolve latest release version for $Repo"
    }
}

if ($Version.StartsWith("v")) {
    $Tag = $Version
    $VerClean = $Version.Substring(1)
} else {
    $Tag = "v$Version"
    $VerClean = $Version
}

$Archive = "maestria_${VerClean}_windows_amd64.zip"
if ($BaseUrl) {
    $DownloadBase = $BaseUrl.TrimEnd("/")
} else {
    $DownloadBase = "https://github.com/$Repo/releases/download/$Tag"
}

$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("maestria-install-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Force -Path $TempDir | Out-Null

try {
    $ArchivePath = Join-Path $TempDir $Archive
    $SumsPath = Join-Path $TempDir "SHA256SUMS"

    Write-Host "Downloading $Archive..."
    Invoke-WebRequest -Uri "$DownloadBase/$Archive" -OutFile $ArchivePath -UseBasicParsing
    Invoke-WebRequest -Uri "$DownloadBase/SHA256SUMS" -OutFile $SumsPath -UseBasicParsing

    $Expected = $null
    foreach ($line in Get-Content -Path $SumsPath) {
        if ($line -match '^\s*([0-9a-fA-F]{64})\s+\*?(.+?)\s*$') {
            if ($Matches[2] -eq $Archive) {
                $Expected = $Matches[1].ToLowerInvariant()
                break
            }
        }
    }
    if (-not $Expected) {
        throw "SHA256SUMS does not contain an entry for $Archive"
    }

    $Actual = (Get-FileHash -Algorithm SHA256 -Path $ArchivePath).Hash.ToLowerInvariant()
    if ($Actual -ne $Expected) {
        throw "Checksum mismatch for $Archive (expected $Expected, got $Actual)"
    }
    Write-Host "Verified SHA256 checksum ($Actual)."

    $ExtractDir = Join-Path $TempDir "extract"
    Expand-Archive -Path $ArchivePath -DestinationPath $ExtractDir -Force

    $SrcBin = Join-Path $ExtractDir "maestria.exe"
    if (-not (Test-Path $SrcBin)) {
        throw "Archive $Archive did not contain maestria.exe"
    }

    New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
    $DstBin = Join-Path $InstallDir "maestria.exe"
    Copy-Item -Path $SrcBin -Destination $DstBin -Force
    Write-Host "Installed maestria to $DstBin"

    $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
    $PathParts = if ($UserPath) { $UserPath.Split(";") | Where-Object { $_ -ne "" } } else { @() }
    if ($PathParts -notcontains $InstallDir) {
        $NewPath = (@($InstallDir) + $PathParts) -join ";"
        [Environment]::SetEnvironmentVariable("Path", $NewPath, "User")
        $env:Path = "$InstallDir;$env:Path"
        Write-Host "Added $InstallDir to your user PATH (open a new terminal to pick it up)."
    }

    Write-Host 'Run "maestria init" to set up Maestria.'
} finally {
    Remove-Item -Recurse -Force -Path $TempDir -ErrorAction SilentlyContinue
}
