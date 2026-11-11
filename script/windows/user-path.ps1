# user-path.ps1: add or remove one directory in the per-user PATH.
# Edits the registry value directly so %VAR% entries stay unexpanded and the
# existing value kind (REG_SZ or REG_EXPAND_SZ) is preserved; NSIS strings are
# capped at 1024 characters and would truncate long PATHs.
param(
    [Parameter(Mandatory)][ValidateSet('add', 'remove')][string]$Action,
    [Parameter(Mandatory)][string]$Dir
)
$ErrorActionPreference = 'Stop'

$norm = { param($p) $p.Trim().TrimEnd('\') }
# GetFullPath folds the "dir\." form the MSI passes to avoid a '\"' argument.
$target = & $norm ([System.IO.Path]::GetFullPath($Dir))
$key = [Microsoft.Win32.Registry]::CurrentUser.CreateSubKey('Environment')
try {
    $kind = [Microsoft.Win32.RegistryValueKind]::ExpandString
    $old = ''
    if ($key.GetValueNames() -contains 'Path') {
        $kind = $key.GetValueKind('Path')
        $old = $key.GetValue('Path', '', [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
    }
    $parts = @($old -split ';' | Where-Object { $_.Trim() -ne '' })
    $present = @($parts | Where-Object { (& $norm $_) -ieq $target }).Count -gt 0
    if ($Action -eq 'add') {
        if ($present) { exit 0 }
        $parts += $target
    } else {
        if (-not $present) { exit 0 }
        $parts = @($parts | Where-Object { (& $norm $_) -ine $target })
    }
    $key.SetValue('Path', ($parts -join ';'), $kind)
} finally {
    $key.Close()
}
