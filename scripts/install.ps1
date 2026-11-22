# install.ps1: install Cortex (cortex.exe and agentop.exe) on Windows.
#
#   irm https://www.rossoctl.dev/cortex/install.ps1 | iex
#
# Or, with options, from a saved copy:
#
#   .\install.ps1 -Version v0.10.1 -InstallDir C:\tools\cortex -NoPath
#
# Piped to iex, the script takes its options from environment variables instead:
# CORTEX_VERSION, CORTEX_INSTALL_DIR and CORTEX_NO_PATH=1.
#
# What it does: finds the release (the latest, or the one asked for), downloads the two Windows
# zips for this PC's architecture and checksums.txt, checks each zip's SHA-256 against
# checksums.txt and stops on any mismatch, removes the "downloaded from the internet" mark
# (Unblock-File), unpacks both into one folder, and adds that folder to the USER Path if it is
# not there yet. It needs no administrator rights and changes nothing machine-wide. Running it
# again installs over the same files. Works in Windows PowerShell 5.1 and PowerShell 7.
#
# Not done on Windows yet (#1520): a background service, `agentop setup`, `update` and
# `uninstall`. See docs/windows.md.

param(
    [string]$Version = $(if ($env:CORTEX_VERSION) { $env:CORTEX_VERSION } else { 'latest' }),
    [string]$InstallDir = $(if ($env:CORTEX_INSTALL_DIR) { $env:CORTEX_INSTALL_DIR } else { Join-Path $HOME '.local\bin' }),
    [switch]$NoPath = $($env:CORTEX_NO_PATH -eq '1'),
    [string]$Repo = 'rossoctl/cortex',
    # Where the release's files are, if not GitHub: a mirror that keeps the release layout
    # (<BaseUrl>/<file>). The checksums are then read from the mirror too.
    [string]$BaseUrl = $env:CORTEX_BASE_URL
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue' # Windows PowerShell 5.1 downloads far slower with a progress bar

function Write-Step([string]$Message) { Write-Host "==> $Message" }

function Get-CortexArch {
    # PROCESSOR_ARCHITEW6432 is set when a 32-bit PowerShell runs on a 64-bit Windows.
    $arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    switch ($arch) {
        'AMD64' { return 'amd64' }
        'ARM64' { return 'arm64' }
        default { throw "Cortex has Windows builds for amd64 and arm64 only; this PC is '$arch'." }
    }
}

function Resolve-CortexVersion([string]$Version, [string]$Repo) {
    if ($Version -and $Version -ne 'latest') {
        if ($Version -notmatch '^v') { $Version = "v$Version" }
        return $Version
    }
    $release = Invoke-RestMethod -UseBasicParsing -Headers @{ 'User-Agent' = 'cortex-install-ps1' } `
        -Uri "https://api.github.com/repos/$Repo/releases/latest"
    if (-not $release.tag_name) { throw "Could not find the latest release of $Repo." }
    return $release.tag_name
}

function Get-ExpectedHash([string]$ChecksumsFile, [string]$Name) {
    foreach ($line in Get-Content -LiteralPath $ChecksumsFile) {
        $parts = $line.Trim() -split '\s+', 2
        # A line is "<sha256>  ./<name>": sha256sum's format, with an optional "./" and the
        # optional "*" of binary mode before the name.
        if ($parts.Count -eq 2) {
            $file = $parts[1].TrimStart('*')
            if ($file.StartsWith('./')) { $file = $file.Substring(2) }
            if ($file -eq $Name) { return $parts[0].ToLowerInvariant() }
        }
    }
    throw "checksums.txt has no line for ${Name}: stopping. Nothing was installed."
}

# The file helpers use .NET and the file system provider directly, not Get-FileHash,
# Expand-Archive or Unblock-File: those live in modules that a PowerShell started from another
# shell (its PSModulePath inherited) can fail to load.

function Get-Sha256([string]$File) {
    $sha = [Security.Cryptography.SHA256]::Create()
    $stream = [IO.File]::OpenRead($File)
    try { $bytes = $sha.ComputeHash($stream) } finally { $stream.Dispose(); $sha.Dispose() }
    return -join ($bytes | ForEach-Object { $_.ToString('x2') })
}

function Expand-Zip([string]$Zip, [string]$Dest) {
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    New-Item -ItemType Directory -Force -Path $Dest | Out-Null
    $archive = [IO.Compression.ZipFile]::OpenRead($Zip)
    try {
        $root = [IO.Path]::GetFullPath($Dest).TrimEnd('\') + '\'
        foreach ($entry in $archive.Entries) {
            if (-not $entry.Name) { continue } # a directory entry
            $target = [IO.Path]::GetFullPath((Join-Path $Dest $entry.FullName))
            if (-not $target.StartsWith($root, [StringComparison]::OrdinalIgnoreCase)) {
                throw "The zip $Zip has an entry outside its folder ($($entry.FullName)): nothing was installed."
            }
            New-Item -ItemType Directory -Force -Path (Split-Path $target) | Out-Null
            [IO.Compression.ZipFileExtensions]::ExtractToFile($entry, $target, $true)
        }
    } finally { $archive.Dispose() }
}

# Unblock removes the "downloaded from the internet" mark: the Zone.Identifier stream.
function Unblock([string]$File) {
    Remove-Item -LiteralPath $File -Stream Zone.Identifier -ErrorAction SilentlyContinue
}

function Test-PathHasDir([string]$PathValue, [string]$Dir) {
    $want = $Dir.TrimEnd('\')
    foreach ($entry in ($PathValue -split ';')) {
        if ($entry -and ($entry.TrimEnd('\') -ieq $want)) { return $true }
    }
    return $false
}

function Add-ToUserPath([string]$Dir) {
    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    if (-not $userPath) { $userPath = '' }
    if (Test-PathHasDir $userPath $Dir) {
        Write-Step "$Dir is already on your user Path."
    } else {
        $new = if ($userPath.TrimEnd(';')) { $userPath.TrimEnd(';') + ';' + $Dir } else { $Dir }
        [Environment]::SetEnvironmentVariable('Path', $new, 'User')
        Write-Step "Added $Dir to your user Path."
    }
    if (-not (Test-PathHasDir $env:Path $Dir)) { $env:Path = $env:Path.TrimEnd(';') + ';' + $Dir }
}

function Install-Cortex {
    # TLS 1.2 for GitHub, which Windows PowerShell 5.1 does not always enable by default.
    try { [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12 } catch { }

    $arch = Get-CortexArch
    $tag = Resolve-CortexVersion $Version $Repo
    Write-Step "Installing Cortex $tag (windows/$arch) into $InstallDir"

    $base = if ($BaseUrl) { $BaseUrl.TrimEnd('/') } else { "https://github.com/$Repo/releases/download/$tag" }
    $work = Join-Path ([IO.Path]::GetTempPath()) ("cortex-install-" + [Guid]::NewGuid().ToString('N'))
    New-Item -ItemType Directory -Path $work | Out-Null
    try {
        $sums = Join-Path $work 'checksums.txt'
        Invoke-WebRequest -UseBasicParsing -Uri "$base/checksums.txt" -OutFile $sums

        $zips = foreach ($tool in 'agentop', 'cortex') {
            $name = "${tool}_${tag}_windows_${arch}.zip"
            $zip = Join-Path $work $name
            Write-Step "Downloading $name"
            Invoke-WebRequest -UseBasicParsing -Uri "$base/$name" -OutFile $zip
            $want = Get-ExpectedHash $sums $name
            $got = Get-Sha256 $zip
            if ($got -ne $want) {
                throw "SHA-256 mismatch for ${name}: checksums.txt says $want, the download is $got. Nothing was installed."
            }
            Write-Step "Checked $name against checksums.txt"
            # The binaries are not signed yet, and a downloaded zip passes its "from the internet"
            # mark on to what it holds, so SmartScreen could block them.
            Unblock $zip
            $zip
        }

        # Unpack both first, so a bad zip changes nothing in the install folder.
        $stage = Join-Path $work 'stage'
        foreach ($zip in $zips) { Expand-Zip $zip $stage }
        $exes = @(Get-ChildItem -LiteralPath $stage -Recurse -Filter '*.exe')
        foreach ($want in 'agentop.exe', 'cortex.exe') {
            if (-not ($exes | Where-Object Name -eq $want)) { throw "The release has no ${want}: nothing was installed." }
        }

        New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
        foreach ($exe in $exes) {
            $dest = Join-Path $InstallDir $exe.Name
            try {
                Copy-Item -LiteralPath $exe.FullName -Destination $dest -Force
            } catch {
                throw "Could not write ${dest}: is it running? Stop cortex and agentop (Ctrl-C in their terminals), then run this again. ($($_.Exception.Message))"
            }
            Unblock $dest
            Write-Step "Installed $dest"
        }
    } finally {
        Remove-Item -LiteralPath $work -Recurse -Force -ErrorAction SilentlyContinue
    }

    if ($NoPath) {
        Write-Step "Left your Path alone (-NoPath). Run the tools as $InstallDir\cortex.exe, or add the folder yourself."
    } else {
        Add-ToUserPath $InstallDir
    }

    Write-Host ''
    Write-Host 'Next steps:'
    Write-Host '  1. Open a NEW terminal (one that is already open keeps the old Path).'
    Write-Host '  2. Run the proxy:          cortex --local'
    Write-Host '  3. In a second terminal:   agentop observe'
    Write-Host '  4. Connect Claude Code:    agentop configure claude-code enable'
    Write-Host '     (or one session only:   agentop exec -- claude)'
    Write-Host 'Guide: https://github.com/rossoctl/cortex/blob/main/docs/windows.md'
}

Install-Cortex
