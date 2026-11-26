# Cortex on Windows

Windows support is a preview (#1520). Cortex records and shows your agent sessions on Windows as it does on macOS and Linux. The difference is that it runs in a terminal, not as a background service, so you start it yourself and it runs while its terminal is open.

This guide covers Windows 10 and 11 on `amd64` and `arm64`, in Windows PowerShell 5.1 or PowerShell 7.

## Install

In PowerShell (no administrator rights needed):

```powershell
irm https://www.rossoctl.dev/cortex/install.ps1 | iex
```

rossoctl.dev unreachable? The same script is in this repository: `irm https://raw.githubusercontent.com/rossoctl/cortex/main/scripts/install.ps1 | iex`.

The installer:

- downloads `cortex` and `agentop` for your PC's architecture from the latest release, with `checksums.txt`;
- checks each download's SHA-256 against `checksums.txt`, and stops without installing anything if one does not match;
- removes the "downloaded from the internet" mark, so SmartScreen does not block the unsigned binaries;
- unpacks both into `$HOME\.local\bin` and adds that folder to your **user** `Path` (not the machine `Path`).

Running it again installs over the same files. To choose a version or a folder, or to leave `Path` alone, set these before the one-liner:

```powershell
$env:CORTEX_VERSION = 'v0.10.1'          # default: the latest release
$env:CORTEX_INSTALL_DIR = 'C:\tools\cortex'  # default: $HOME\.local\bin
$env:CORTEX_NO_PATH = '1'                # don't add the folder to Path
irm https://www.rossoctl.dev/cortex/install.ps1 | iex
```

The same options are parameters too. Without saving a file:

```powershell
& ([scriptblock]::Create((irm https://www.rossoctl.dev/cortex/install.ps1))) -Version v0.10.1 -InstallDir C:\tools\cortex -NoPath
```

PowerShell's default execution policy blocks a saved copy of a script (`running scripts is disabled on this system`), so run a saved copy with `powershell -ExecutionPolicy Bypass -File .\install.ps1 -Version v0.10.1`. That applies to this one run only and changes no setting. The script is [`scripts/install.ps1`](../scripts/install.ps1) in this repository; read it first if you like.

The installer adds the folder to your user `Path` in place: entries such as `%USERPROFILE%\...` keep their variables.

**Then open a new terminal.** A terminal that is already open keeps the old `Path`.

### Install by hand

1. From the [releases page](https://github.com/rossoctl/cortex/releases), download `agentop_<version>_windows_amd64.zip` and `cortex_<version>_windows_amd64.zip` (`arm64` on an Arm PC), and `checksums.txt`.
2. Check each zip. `checksums.txt` has lower-case hashes, and `Get-FileHash` prints upper case, so compare `(Get-FileHash .\agentop_<version>_windows_amd64.zip -Algorithm SHA256).Hash.ToLower()` with the line for that file, and the same for `cortex`.
3. Unblock both: `Unblock-File .\agentop_<version>_windows_amd64.zip`, and the same for `cortex`.
4. Unzip both into one folder, for example `$HOME\.local\bin`, and add that folder to your user `Path`.
5. Open a new terminal.

## First run

1. In a new terminal, start the proxy:

   ```powershell
   cortex --local
   ```

   It writes `~/.cortex/config.yaml` on the first run and listens on `127.0.0.1:47600`. Leave this terminal open. `Ctrl-C` stops it.
2. In a second new terminal, open the viewer:

   ```powershell
   agentop observe
   ```

## Connect Claude Code

To send every Claude Code session through Cortex:

```powershell
agentop configure claude-code enable
```

Then restart Claude Code. `claude --resume` opens your conversation again. To undo it: `agentop configure claude-code disable`, then restart Claude Code.

To send one session only, without changing any settings:

```powershell
agentop exec -- claude
```

Other agents connect the same way; see [Getting started](getting-started.md#connect-your-agent).

**Start `cortex --local` before Claude Code.** An agent that is connected sends its traffic to Cortex, so while Cortex is not running its requests fail (see [Troubleshooting](#troubleshooting)).

## Don't record a session

To run one Claude Code session through Cortex without recording it:

```powershell
agentop exec --no-record -- claude
```

Or set `$env:CORTEX_RECORD = 'off'` before `agentop exec -- claude`: it does the same. Cortex then keeps no content for that session, and `agentop observe` marks it as not recorded. This works for Claude Code only for now.

## Update

`agentop update` does not run on Windows yet. To update:

1. Stop `cortex` and `agentop observe` (`Ctrl-C` in their terminals). Windows cannot replace a program while it runs.
2. Run the install one-liner again.
3. Start `cortex --local` again in a new terminal.

## Uninstall

`agentop uninstall` does not run on Windows yet. Remove Cortex by hand:

1. Disconnect each agent you connected: `agentop configure <agent> disable`. Then restart the agent.
2. Stop the proxy: `Ctrl-C` in its terminal.
3. Delete `agentop.exe` and `cortex.exe`, and remove their folder from your user `Path` if you added it only for them.
4. Optional, and separate: delete `~/.cortex`. That deletes everything Cortex kept, including the session archive, the cost ledger and the TLS bridge's CA.

## Not on Windows yet

These are tracked in #1520:

- A background service: `agentop service` refuses on Windows, so the proxy runs only while its terminal is open.
- `agentop setup`, `agentop update` and `agentop uninstall` refuse and say so. Use the steps above.
- Inside a session, tools other than Node (`git`, `curl`, Python) don't trust the TLS bridge yet, so their HTTPS requests through Cortex can fail certificate checks.

## Troubleshooting

Run `agentop doctor` first. Each line that starts with `✗` names the command that repairs it.

**The viewer has dark stripes on a light terminal.** agentop cannot ask a Windows terminal for its background colour, so it uses the dark palette. On a light terminal, set `$env:AGENTOP_THEME = 'light'` before `agentop observe`, or put `theme: light` in agentop's settings file. `dark` and `auto` are the other values.

**SmartScreen blocks `cortex.exe` or `agentop.exe`.** The binaries are not signed yet. The installer removes the "downloaded from the internet" mark; after a manual install, run `Unblock-File` on each `.exe` (or on the zips before unzipping).

**`cortex` or `agentop` is not recognized.** Open a new terminal: an open terminal keeps the `Path` it started with. If a new terminal still does not find them, check that the install folder is on your user `Path`: `[Environment]::GetEnvironmentVariable('Path', 'User')`.

**Claude Code says `502 Cortex is routed in this environment (HTTPS_PROXY) but is not serving`.** Claude Code is connected but Cortex is not running. Start `cortex --local` in a terminal, or disconnect Claude Code with `agentop configure claude-code disable`. The message suggests `agentop service start`, which does not run on Windows yet.

**A tool in the session fails with a certificate error.** On Windows only Node tools trust the TLS bridge's CA so far. `agentop exec --print` lists the CA files; tools that read `SSL_CERT_FILE`, `CURL_CA_BUNDLE`, `REQUESTS_CA_BUNDLE` or `GIT_SSL_CAINFO` can be pointed at `~/.cortex/ca/bundle.crt` (the bridge CA plus the platform roots). See [Any other agent](agents/any-agent.md).

**`cortex --local` says it can't parse `~/.cortex/config.yaml`.** A config written by an earlier Windows build can hold a Windows path that is not valid YAML. Delete `~/.cortex/config.yaml` and run `cortex --local` again: it writes a fresh one.

**A port is already in use.** Cortex listens on `127.0.0.1:47600` (the proxy), `47601` (the session API), `47602` (stats) and `47604` (health). Another `cortex` still running in another terminal is the usual cause. To see what holds a port: `Get-NetTCPConnection -LocalPort 47600 | Select-Object OwningProcess`, then `Get-Process -Id <pid>`.

**The download fails, or the agent's requests fail, on a corporate network.** The installer downloads from `github.com` and `api.github.com`; if your network blocks them, install by hand from a machine that can reach them. Cortex connects to the model server itself, so it needs the network access your agent had: on a network that needs a VPN to reach the server, connect the VPN before you start `cortex --local`. Rows that show `dial-failed` in `agentop observe` mean Cortex could not reach the server.

For more, read [Troubleshooting](troubleshooting.md).
