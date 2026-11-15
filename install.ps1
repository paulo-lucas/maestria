# Maestria installer for Windows (PowerShell).
# Usage: irm https://raw.githubusercontent.com/paulo-lucas/maestria/main/install.ps1 | iex
#
# What it checks before it installs anything:
#   - cosign installed: the release signature on SHA256SUMS must verify against the identity of
#     this repository's release workflow for exactly this tag. Anything missing or failing aborts.
#   - cosign not installed: a loud warning, then the checksum is the only check (it comes from
#     the same release as the archive, so it catches corruption, not a malicious release).
#     MAESTRIA_REQUIRE_SIGNATURE=1 turns that warning into an error.
#
# Nothing runs until the last line: only functions are defined before Install-Maestria is called,
# so a download cut short mid-pipe parses to an error and executes nothing.

# Windows PowerShell 5.1 negotiates TLS 1.0/1.1 by default; GitHub needs 1.2.
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

function Get-MaestriaFile {
    # https only (file:// is kept for tests and offline mirrors).
    param([string]$Uri, [string]$OutFile)
    if ($Uri.StartsWith("file://")) {
        $local = ([System.Uri]$Uri).LocalPath
        Copy-Item -Path $local -Destination $OutFile -ErrorAction Stop
        return
    }
    if (-not $Uri.StartsWith("https://")) {
        throw "Refusing to download over a non-https URL: $Uri"
    }
    Invoke-WebRequest -Uri $Uri -OutFile $OutFile -UseBasicParsing
}

function Confirm-MaestriaSignature {
    # The identity is exact: this repository's release workflow, run for this very tag, on a push,
    # in this repository. Pinning the tag means another release's signed SHA256SUMS cannot be
    # swapped in; the repository and trigger claims mean a workflow that only shares our file name
    # in another repository does not count. The repository is MAESTRIA_REPO, the user's own choice;
    # MAESTRIA_BASE_URL (a mirror) changes where files come from, never who must have signed them.
    param([string]$Repo, [string]$Tag, [string]$SumsPath, [string]$BundleUrl, [string]$BundlePath)

    $cosign = Get-Command cosign -CommandType Application -ErrorAction SilentlyContinue
    if (-not $cosign) {
        if ($env:MAESTRIA_REQUIRE_SIGNATURE -eq "1") {
            throw "cosign is required (MAESTRIA_REQUIRE_SIGNATURE=1) but was not found; install it from https://docs.sigstore.dev/cosign/system_config/installation/"
        }
        Write-Warning "cosign was not found, so the release signature was NOT checked."
        Write-Warning "Only the checksum from the same release is verified, which does not prove who built it."
        Write-Warning "To check by hand, with cosign: cosign verify-blob --bundle SHA256SUMS.sigstore.json --certificate-identity https://github.com/$Repo/.github/workflows/release.yml@refs/tags/$Tag --certificate-oidc-issuer https://token.actions.githubusercontent.com --certificate-github-workflow-repository $Repo --certificate-github-workflow-trigger push SHA256SUMS"
        Write-Warning "Or with the GitHub CLI, on the archive: gh attestation verify <archive> -R $Repo --signer-workflow $Repo/.github/workflows/release.yml"
        return
    }

    Write-Host "Verifying the release signature with cosign..."
    Get-MaestriaFile -Uri $BundleUrl -OutFile $BundlePath
    & $cosign.Source verify-blob `
        --bundle $BundlePath `
        --certificate-identity "https://github.com/$Repo/.github/workflows/release.yml@refs/tags/$Tag" `
        --certificate-oidc-issuer "https://token.actions.githubusercontent.com" `
        --certificate-github-workflow-repository $Repo `
        --certificate-github-workflow-trigger push `
        $SumsPath
    if ($LASTEXITCODE -ne 0) {
        $installed = "unknown version"
        try {
            $versionLine = (& $cosign.Source version 2>$null | Select-String -Pattern '^GitVersion:\s*(.+)$' | Select-Object -First 1)
            if ($versionLine) { $installed = $versionLine.Matches[0].Groups[1].Value }
        } catch {
            # The version is only a hint for the error message.
        }
        Write-Warning "installed cosign: $installed"
        throw "The signature on SHA256SUMS did not verify; nothing was installed. If the message above is about the bundle format, upgrade cosign (releases are signed with cosign v3) and run this again."
    }
    Write-Host "Verified the release signature (built by the $Repo release workflow for $Tag)."
}

function Install-Maestria {
    $ErrorActionPreference = "Stop"

    $Repo = if ($env:MAESTRIA_REPO) { $env:MAESTRIA_REPO } else { "paulo-lucas/maestria" }
    $Version = if ($env:MAESTRIA_VERSION) { $env:MAESTRIA_VERSION } else { "latest" }
    $InstallDir = if ($env:MAESTRIA_INSTALL_DIR) { $env:MAESTRIA_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA "Programs\maestria\bin" }
    $BaseUrl = $env:MAESTRIA_BASE_URL

    # owner/name only, because it becomes part of a URL and of the signing identity.
    if ($Repo -notmatch '\A[A-Za-z0-9._-]+/[A-Za-z0-9._-]+\z') {
        throw "MAESTRIA_REPO must look like owner/name"
    }

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
    if ($Tag -notmatch '\A[A-Za-z0-9._+-]+\z') {
        throw "Unexpected characters in the version: $Tag"
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
        $BundlePath = Join-Path $TempDir "SHA256SUMS.sigstore.json"

        Write-Host "Downloading $Archive..."
        Get-MaestriaFile -Uri "$DownloadBase/$Archive" -OutFile $ArchivePath
        Get-MaestriaFile -Uri "$DownloadBase/SHA256SUMS" -OutFile $SumsPath

        # The signature comes first: no line of SHA256SUMS is trusted until it has verified.
        Confirm-MaestriaSignature -Repo $Repo -Tag $Tag -SumsPath $SumsPath -BundleUrl "$DownloadBase/SHA256SUMS.sigstore.json" -BundlePath $BundlePath

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
}

Install-Maestria
