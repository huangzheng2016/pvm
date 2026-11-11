# build.ps1: Windows entry point for script/build.sh. The build logic lives
# only in build.sh; this runs it with the bash that ships with Git for Windows.
$ErrorActionPreference = 'Stop'

$git = Get-Command git.exe -ErrorAction SilentlyContinue
if (-not $git) {
    Write-Error 'git.exe not found on PATH; install Git for Windows (provides Git Bash)'
    exit 1
}
# git.exe lives in <root>\cmd or <root>\mingw64\bin; bash.exe in <root>\bin.
$root = Split-Path (Split-Path $git.Source)
if ((Split-Path $root -Leaf) -eq 'mingw64') { $root = Split-Path $root }
$bash = Join-Path $root 'bin\bash.exe'
if (-not (Test-Path $bash)) {
    Write-Error "Git Bash not found at $bash"
    exit 1
}

if (-not $env:PVM_PACK_QEMU) { $env:PVM_PACK_QEMU = '0' }
& $bash (Join-Path $PSScriptRoot 'build.sh') @args
exit $LASTEXITCODE
