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

# Import-InstallFunction defines install.ps1's named functions here, from its AST, without
# running the script.
function Import-InstallFunction([string[]]$Names) {
    $ast = [Management.Automation.Language.Parser]::ParseFile($script, [ref]$null, [ref]$null)
    foreach ($name in $Names) {
        $fn = $ast.FindAll({ $args[0] -is [Management.Automation.Language.FunctionDefinitionAst] -and $args[0].Name -eq $name }, $true)
        . ([scriptblock]::Create($fn[0].Extent.Text))
    }
}

# Get-UserPathRaw is the user Path as the registry holds it, %VAR% references unexpanded.
function Get-UserPathRaw {
    $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment')
    try { return [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames) }
    finally { $key.Dispose() }
}

# Assert-PathUnchanged runs an install that was asked not to touch Path, and checks it did not.
function Assert-PathUnchanged([scriptblock]$Install) {
    $before = Get-UserPathRaw
    & $Install
    if ((Get-UserPathRaw) -ne $before) { throw 'the install changed the user Path although it was told not to' }
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
        . Import-InstallFunction 'Test-PathHasDir'
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

    Check 'finds the latest release from the github.com redirect, not the API' {
        . Import-InstallFunction 'Get-LatestTag'
        $tag = Get-LatestTag 'rossoctl/cortex' -RedirectOnly # no API fallback: the redirect itself must work
        if ($tag -notmatch '^v[0-9]') { throw "Get-LatestTag returned '$tag'" }
    }

    # The user Path is written only on CI, into a throwaway registry key, never into the real
    # HKCU\Environment of whoever runs this.
    if ($env:CI -eq 'true') {
        Check 'adds to the Path in the registry keeping %VAR% entries, once' {
            . Import-InstallFunction 'Write-Step', 'Test-PathHasDir', 'Add-ToUserPath'
            $keyPath = 'Software\cortex-install-test-' + [Guid]::NewGuid().ToString('N')
            $key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey($keyPath)
            $orig = '%USERPROFILE%\AppData\Local\Microsoft\WindowsApps;C:\other'
            $key.SetValue('Path', $orig, [Microsoft.Win32.RegistryValueKind]::ExpandString)
            $key.Dispose()
            try {
                Add-ToUserPath 'C:\cortex-test-bin' -KeyPath $keyPath -NoBroadcast | Out-Null
                Add-ToUserPath 'C:\cortex-test-bin' -KeyPath $keyPath -NoBroadcast | Out-Null
                $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey($keyPath)
                $raw = [string]$key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
                $kind = $key.GetValueKind('Path')
                $key.Dispose()
                if ($raw -ne "$orig;C:\cortex-test-bin") { throw "the Path became '$raw'" }
                if ($kind -ne [Microsoft.Win32.RegistryValueKind]::ExpandString) { throw "the Path's kind became $kind" }
            } finally {
                [Microsoft.Win32.Registry]::CurrentUser.DeleteSubKeyTree($keyPath, $false)
            }
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
            Assert-PathUnchanged {
                & $script -Version $Version -InstallDir $dir -NoPath -BaseUrl $url | Out-Null
                & $script -Version $Version -InstallDir $dir -NoPath -BaseUrl $url | Out-Null
            }
            Assert-Installed $dir
        }

        Check 'resolves a relative install folder' {
            Push-Location $root
            try {
                Assert-PathUnchanged { & $script -Version $Version -InstallDir 'relative-bin' -NoPath -BaseUrl $url | Out-Null }
            } finally { Pop-Location }
            Assert-Installed (Join-Path $root 'relative-bin')
        }

        Check 'refuses to replace a running binary, and changes nothing' {
            $dir = Join-Path $root 'twice'
            $agentop = Join-Path $dir 'agentop.exe'
            $before = (Get-Item -LiteralPath $agentop).LastWriteTimeUtc
            $lock = [IO.File]::Open((Join-Path $dir 'cortex.exe'), 'Open', 'Read', 'Read') # as a running exe holds it
            $refused = $false
            try {
                & $script -Version $Version -InstallDir $dir -NoPath -BaseUrl $url | Out-Null
            } catch {
                if ($_.Exception.Message -notmatch 'is it running') { throw "stopped for another reason: $($_.Exception.Message)" }
                $refused = $true
            } finally { $lock.Dispose() }
            if (-not $refused) { throw 'it replaced a file that is in use' }
            if ((Get-Item -LiteralPath $agentop).LastWriteTimeUtc -ne $before) { throw 'agentop.exe was replaced before the refusal' }
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
            # The one-liner runs at a user's prompt: the session's top-level scope. A fresh
            # PowerShell process gives the same scope here (this check's own scope is a compiled
            # scriptblock, which is not what a user has). Afterwards nothing of the install may
            # be left in that session: preferences, functions or parameters.
            $probe = @'
$ErrorActionPreference = 'Continue'
$ProgressPreference = 'Continue'
Get-Content -Raw $env:CORTEX_TEST_SCRIPT | Invoke-Expression | Out-Null
$leaks = @()
if ($ErrorActionPreference -ne 'Continue' -or $ProgressPreference -ne 'Continue') { $leaks += 'preferences' }
if (Get-Command Install-Cortex -ErrorAction SilentlyContinue) { $leaks += 'functions' }
foreach ($n in 'Version', 'InstallDir', 'NoPath', 'Repo', 'BaseUrl') { if (Get-Variable -Name $n -ErrorAction SilentlyContinue) { $leaks += "`$$n" } }
if ($leaks) { "LEAKED: $($leaks -join ', ')" } else { 'CLEAN' }
'@
            $env:CORTEX_VERSION = $Version
            $env:CORTEX_INSTALL_DIR = $dir
            $env:CORTEX_NO_PATH = '1'
            $env:CORTEX_TEST_SCRIPT = $script
            try {
                $exe = (Get-Process -Id $PID).Path
                Assert-PathUnchanged { $script:probeOut = & $exe -NoProfile -ExecutionPolicy Bypass -Command $probe 2>&1 | Out-String }
            } finally {
                Remove-Item Env:CORTEX_VERSION, Env:CORTEX_INSTALL_DIR, Env:CORTEX_NO_PATH, Env:CORTEX_TEST_SCRIPT -ErrorAction SilentlyContinue
            }
            if ($script:probeOut -notmatch 'CLEAN') { throw "the piped install left something in the session: $($script:probeOut.Trim())" }
            Assert-Installed $dir
        }
    }
} finally {
    Remove-Item -LiteralPath $root -Recurse -Force -ErrorAction SilentlyContinue
}

if ($failures) { Write-Host "$failures check(s) failed"; exit 1 }
Write-Host 'all install.ps1 checks passed'
