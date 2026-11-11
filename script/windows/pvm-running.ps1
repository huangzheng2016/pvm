# pvm-running.ps1: run by both installers before files are replaced. Files
# under a running daemon or VM cannot be replaced (the MSI would defer them to
# a reboot), so find those processes by executable path.
#   -Mode check : exit with the number of running pvm/QEMU processes
#   -Mode prompt: if any run, offer to stop the daemon (which stops running
#                 VMs) and wait for them to exit; exit 0 once none remain
param(
    [Parameter(Mandatory)][string]$InstallDir,
    [Parameter(Mandatory)][string]$RuntimeDir,
    [ValidateSet('check', 'prompt')][string]$Mode = 'check'
)
$ErrorActionPreference = 'Stop'

function Norm([string]$p) { [System.IO.Path]::GetFullPath($p).TrimEnd('\') + '\' }
$dirs = @((Norm $InstallDir), (Norm $RuntimeDir))

# CIM, not Get-Process: the NSIS installer is 32-bit, and a 32-bit process
# cannot read the image path of 64-bit ones, so .Path would be empty.
function Get-Running {
    @(Get-CimInstance Win32_Process -Filter "Name = 'pvm.exe' OR Name = 'qemu-system-x86_64.exe'" | Where-Object {
        $path = $_.ExecutablePath
        $path -and @($dirs | Where-Object { $path.StartsWith($_, [StringComparison]::OrdinalIgnoreCase) }).Count -gt 0
    } | ForEach-Object { [pscustomobject]@{ Name = $_.Name; Path = $_.ExecutablePath } })
}
function Show([string]$text, [int]$flags) {
    # 0x1000 keeps the box above the installer window.
    (New-Object -ComObject WScript.Shell).Popup($text, 0, 'pvm setup', $flags + 0x1000)
}
function List($procs) { ($procs | ForEach-Object Path | Sort-Object -Unique) -join "`n" }

# @(): Windows PowerShell 5.1 unrolls a one-element result into a bare
# pscustomobject, whose .Count is $null, so "exit $procs.Count" would be 0.
$procs = @(Get-Running)
if ($procs.Count -eq 0) { exit 0 }
if ($Mode -eq 'check') { exit $procs.Count }

$daemon = $procs | Where-Object Name -eq 'pvm.exe' | Select-Object -First 1
if (-not $daemon) {
    # Without a daemon nothing can shut the VMs down cleanly; killing QEMU
    # could lose guest disk writes.
    $null = Show "These pvm VMs are running without a daemon:`n`n$(List $procs)`n`nShut them down, then run setup again." 16
    exit 1
}
if ((Show "pvm is running. Setup must stop the pvm daemon, which also stops all running VMs.`n`nStop it now?" (1 + 48)) -ne 1) {
    exit 1
}
& $daemon.Path daemon stop | Out-Null
for ($i = 0; $i -lt 60 -and @(Get-Running).Count -gt 0; $i++) { Start-Sleep -Seconds 1 }
$left = @(Get-Running)
if ($left.Count -gt 0) {
    $null = Show "These are still running after 'pvm daemon stop':`n`n$(List $left)`n`nClose them, then run setup again." 16
    exit 1
}
exit 0
