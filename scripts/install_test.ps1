# install_test.ps1: tests of install.ps1, for CI on Windows and for a local check.
#
#   powershell -NoProfile -File scripts\install_test.ps1 [-Version v0.10.1]
#   pwsh       -NoProfile -File scripts\install_test.ps1 [-Version v0.10.1]
#
# Everything is installed into temporary folders with -NoPath, so the user Path and the real
# profile are never touched. It downloads one release's Windows files from GitHub once, into a
# local mirror, and runs the installer against that mirror (file://) where Windows PowerShell can;
# PowerShell 7's web cmdlets do not read file:// URLs, so there it installs from GitHub directly.

param([string]$Version = 'v0.10.1')

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$script = Join-Path $PSScriptRoot 'install.ps1'
$root = Join-Path ([IO.Path]::GetTempPath()) ("install-ps1-test-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $root | Out-Null
$failures = 0

function Check([string]$Name, [scriptblock]$Body) {
    try {
        & $Body
        Write-Host "ok   $Name"
    } catch {
        $script:failures++
        Write-Host "FAIL $Name : $($_.Exception.Message)"
    }
}

function Assert-Installed([string]$Dir) {
    foreach ($exe in 'agentop.exe', 'cortex.exe') {
        $f = Join-Path $Dir $exe
        if (-not (Test-Path -LiteralPath $f)) { throw "$f is missing" }
        if (Get-Item -LiteralPath $f -Stream * | Where-Object Stream -eq 'Zone.Identifier') { throw "$f still has the downloaded mark" }
    }
    $v = & (Join-Path $Dir 'cortex.exe') --version
    if ("$v" -notmatch [regex]::Escape($Version)) { throw "cortex --version printed '$v', want $Version" }
}

try {
    $arch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    $arch = @{ 'AMD64' = 'amd64'; 'ARM64' = 'arm64' }[$arch]
    $mirrorOK = $PSVersionTable.PSVersion.Major -le 5

    Check 'Test-PathHasDir matches a folder on Path, case and trailing \ aside' {
        $ast = [Management.Automation.Language.Parser]::ParseFile($script, [ref]$null, [ref]$null)
        $fn = $ast.FindAll({ $args[0] -is [Management.Automation.Language.FunctionDefinitionAst] -and $args[0].Name -eq 'Test-PathHasDir' }, $true)
        Invoke-Expression $fn[0].Extent.Text
        $cases = @(
            @('C:\a;C:\Users\me\.local\bin;C:\b', 'C:\Users\me\.local\bin', $true),
            @('C:\a;c:\users\ME\.local\bin\;C:\b', 'C:\Users\me\.local\bin', $true),
            @('C:\a;C:\Users\me\.local\bin2', 'C:\Users\me\.local\bin', $false),
            @('', 'C:\x', $false),
            @(';;C:\x;', 'C:\x\', $true)
        )
        foreach ($c in $cases) {
            if ((Test-PathHasDir $c[0] $c[1]) -ne $c[2]) { throw "Test-PathHasDir('$($c[0])', '$($c[1])') is not $($c[2])" }
        }
    }

    if ($mirrorOK) {
        $mirror = Join-Path $root 'mirror'
        New-Item -ItemType Directory -Path $mirror | Out-Null
        $base = "https://github.com/rossoctl/cortex/releases/download/$Version"
        foreach ($f in 'checksums.txt', "agentop_${Version}_windows_${arch}.zip", "cortex_${Version}_windows_${arch}.zip") {
            Invoke-WebRequest -UseBasicParsing -Uri "$base/$f" -OutFile (Join-Path $mirror $f)
        }
        $url = 'file:///' + $mirror.Replace('\', '/')

        Check 'installs from a mirror, twice, with no downloaded mark left' {
            $dir = Join-Path $root 'twice'
            & $script -Version $Version -InstallDir $dir -NoPath -BaseUrl $url | Out-Null
            & $script -Version $Version -InstallDir $dir -NoPath -BaseUrl $url | Out-Null
            Assert-Installed $dir
        }

        Check 'stops on a checksum mismatch, and installs nothing' {
            $bad = Join-Path $root 'mirror-bad'
            Copy-Item -Recurse $mirror $bad
            [IO.File]::AppendAllText((Join-Path $bad "cortex_${Version}_windows_${arch}.zip"), 'x')
            $dir = Join-Path $root 'tamper'
            $stopped = $false
            try {
                & $script -Version $Version -InstallDir $dir -NoPath -BaseUrl ('file:///' + $bad.Replace('\', '/')) | Out-Null
            } catch {
                if ($_.Exception.Message -notmatch 'SHA-256 mismatch') { throw "stopped for another reason: $($_.Exception.Message)" }
                $stopped = $true
            }
            if (-not $stopped) { throw 'a tampered zip was installed' }
            if (Test-Path -LiteralPath $dir) { throw "the install folder was created: $dir" }
        }
    } else {
        Check 'installs from GitHub, as the one-liner does (options from the environment)' {
            $dir = Join-Path $root 'iex'
            $env:CORTEX_VERSION = $Version
            $env:CORTEX_INSTALL_DIR = $dir
            $env:CORTEX_NO_PATH = '1'
            try { Get-Content -Raw $script | Invoke-Expression | Out-Null }
            finally { Remove-Item Env:CORTEX_VERSION, Env:CORTEX_INSTALL_DIR, Env:CORTEX_NO_PATH -ErrorAction SilentlyContinue }
            Assert-Installed $dir
        }
    }
} finally {
    Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
}

if ($failures) { Write-Host "$failures check(s) failed"; exit 1 }
Write-Host 'all install.ps1 checks passed'
