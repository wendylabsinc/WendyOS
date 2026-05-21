#requires -Version 5.1
<#
.SYNOPSIS
  Fresh Windows 11 setup for Wendy development machines.

.DESCRIPTION
  Idempotently installs and configures common tools in the same spirit as
  set-up-ubuntu.sh. Run from an elevated PowerShell session.
#>

[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$TraceCommands = if ($env:TRACE_COMMANDS) { $env:TRACE_COMMANDS } else { '1' }
$ScriptDir = if ($PSScriptRoot) { $PSScriptRoot } else { Split-Path -Parent $MyInvocation.MyCommand.Path }
$RepoRoot = (Resolve-Path (Join-Path $ScriptDir '..')).Path
$WendyRepoUrl = if ($env:WENDY_REPO_URL) { $env:WENDY_REPO_URL } else { 'https://github.com/wendylabsinc/wendy-agent.git' }
$CurrentIdentity = [System.Security.Principal.WindowsIdentity]::GetCurrent()
$CurrentPrincipal = [System.Security.Principal.WindowsPrincipal]::new($CurrentIdentity)
$CurrentUser = $CurrentIdentity.Name
$CurrentUserName = [Environment]::UserName
$UserProfile = [Environment]::GetFolderPath('UserProfile')
$UserSid = $CurrentIdentity.User.Value

$script:ConfigureGit = $false
$script:GitName = ''
$script:GitEmail = ''
$script:AuthorizedLoginKeys = [System.Collections.Generic.List[string]]::new()
$script:InstallVisualStudioBuildTools = $true
$script:InstallSwiftToolchain = $true
$script:InstallWendyCli = $false
$script:InstallDirenv = $false
$script:CloneRepository = $false
$script:CloneDestination = ''
$script:EnableSshLogin = $false
$script:ConfigureLoopbackSsh = $false
$script:SetupAutomaticLogin = $false
$script:AutomaticLoginPassword = $null
$script:ConfigureRemoteDesktop = $false
$script:ConfigureTerminalDefaultPowerShell = $false
$script:ConfigureSshDefaultPowerShell = $false
$script:DisableAcSleep = $false
$script:DisableScreenLocking = $false
$script:EnableDeveloperMode = $false

function Write-Bold { param([string]$Message) Write-Host $Message -ForegroundColor White }
function Write-Info { param([string]$Message) Write-Host "`n==> $Message" -ForegroundColor Blue }
function Write-Ok { param([string]$Message) Write-Host "OK $Message" -ForegroundColor Green }
function Write-Warn { param([string]$Message) Write-Host "! $Message" -ForegroundColor Yellow }
function Fail { param([string]$Message) Write-Host "Error: $Message" -ForegroundColor Red; exit 1 }

function Invoke-External {
  param(
    [Parameter(Mandatory)][string]$FilePath,
    [Parameter()][string[]]$Arguments = @()
  )

  if ($TraceCommands -ne '0') {
    Write-Host ('+ {0} {1}' -f $FilePath, ($Arguments -join ' ')) -ForegroundColor DarkGray
  }

  & $FilePath @Arguments
  if ($LASTEXITCODE -ne 0) {
    throw "$FilePath exited with code $LASTEXITCODE"
  }
}

function Ask-YesNo {
  param(
    [Parameter(Mandatory)][string]$Prompt,
    [Parameter()][bool]$Default = $false
  )

  $suffix = if ($Default) { '[Y/n]' } else { '[y/N]' }
  while ($true) {
    $answer = Read-Host "$Prompt $suffix"
    if ([string]::IsNullOrWhiteSpace($answer)) { return $Default }
    switch -Regex ($answer.Trim()) {
      '^(y|yes)$' { return $true }
      '^(n|no)$' { return $false }
      default { Write-Warn 'Please answer yes or no.' }
    }
  }
}

function Test-IsAdministrator {
  return $CurrentPrincipal.IsInRole([System.Security.Principal.WindowsBuiltInRole]::Administrator)
}

function Require-Windows11 {
  if (-not (Test-IsAdministrator)) {
    Fail 'Run this script from an elevated PowerShell session (Run as Administrator).'
  }

  $currentVersion = Get-ItemProperty 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion'
  $build = [int]$currentVersion.CurrentBuildNumber
  if ($build -lt 22000) {
    Fail "This script is intended for Windows 11; detected build $build."
  }
}

function Collect-Configuration {
  Write-Bold 'Fresh Windows 11 setup'
  Write-Host @'

This script is idempotent: it is safe to run repeatedly. Existing packages,
keys, SSH settings, PATH entries, and git settings will be reused or updated
without creating duplicates.
'@

  if (Ask-YesNo 'Configure global git identity?' $false) {
    $script:GitName = Read-Host 'Git user.name (leave empty to skip git configuration)'
    if ([string]::IsNullOrWhiteSpace($script:GitName)) {
      $script:ConfigureGit = $false
    } else {
      $script:GitEmail = Read-Host 'Git user.email (leave empty to skip git configuration)'
      $script:ConfigureGit = -not [string]::IsNullOrWhiteSpace($script:GitEmail)
      if (-not $script:ConfigureGit) { $script:GitName = '' }
    }
  } else {
    $script:ConfigureGit = $false
  }

  $script:EnableSshLogin = Ask-YesNo 'Enable SSH login via OpenSSH Server?' $false

  if (Ask-YesNo "Install additional SSH public keys into $CurrentUserName's authorized_keys?" $false) {
    Write-Host 'Paste one public key per prompt. Leave empty when done.'
    while ($true) {
      $key = Read-Host ('SSH public key {0}' -f ($script:AuthorizedLoginKeys.Count + 1))
      if ([string]::IsNullOrWhiteSpace($key)) { break }
      $script:AuthorizedLoginKeys.Add($key.Trim())
    }
  }

  $script:ConfigureLoopbackSsh = Ask-YesNo 'Enable passwordless loopback SSH for local automation?' $false

  $script:InstallVisualStudioBuildTools = Ask-YesNo 'Install Visual Studio Build Tools for C++/Swift builds?' $true
  $script:InstallSwiftToolchain = Ask-YesNo 'Install the Swift toolchain?' $true
  $script:InstallDirenv = Ask-YesNo 'Install and configure direnv for repository-local developer tooling?' $false
  $script:InstallWendyCli = Ask-YesNo 'Install or update the Wendy CLI?' $false
  if (Test-Path (Join-Path $RepoRoot '.git')) {
    $script:CloneRepository = $false
  } elseif (Ask-YesNo 'Clone the Wendy repository onto this machine?' $false) {
    $script:CloneRepository = $true
    $defaultCloneDestination = Join-Path (Join-Path (Join-Path $UserProfile 'Projects') 'WendyLabs') 'wendy-agent'
    $cloneDestination = Read-Host "Clone destination [$defaultCloneDestination]"
    if ([string]::IsNullOrWhiteSpace($cloneDestination)) { $cloneDestination = $defaultCloneDestination }
    $script:CloneDestination = $cloneDestination
  }
  $script:EnableDeveloperMode = Ask-YesNo 'Enable Windows Developer Mode?' $false
  if (Ask-YesNo 'Enable automatic Windows sign-in on startup? This stores your password in the registry.' $false) {
    $script:SetupAutomaticLogin = $true
    $script:AutomaticLoginPassword = Read-Host 'Windows account password for automatic sign-in' -AsSecureString
    if (-not $script:AutomaticLoginPassword -or $script:AutomaticLoginPassword.Length -eq 0) {
      Fail 'Automatic sign-in password cannot be empty.'
    }
  }
  $script:ConfigureRemoteDesktop = Ask-YesNo 'Enable Remote Desktop?' $false
  $script:ConfigureTerminalDefaultPowerShell = Ask-YesNo 'Make Windows Terminal open PowerShell by default?' $false
  $script:ConfigureSshDefaultPowerShell = Ask-YesNo 'Make SSH sessions start in PowerShell?' $false
  $script:DisableAcSleep = Ask-YesNo 'Disable automatic sleep on AC power?' $false
  $script:DisableScreenLocking = Ask-YesNo 'Disable screen locking for the current user?' $false
}

function Confirm-Plan {
  $gitSummary = if ($script:ConfigureGit) {
    "Global git user.name ($($script:GitName)) and user.email ($($script:GitEmail)) for $CurrentUser"
  } else {
    'Global git identity will not be changed'
  }

  $sshKeySummary = if ($script:AuthorizedLoginKeys.Count -gt 0) {
    "$($script:AuthorizedLoginKeys.Count) additional authorized SSH public key(s) for $CurrentUserName"
  } else {
    'No additional authorized SSH public keys'
  }

  $vsSummary = if ($script:InstallVisualStudioBuildTools) { 'Visual Studio Build Tools will be installed' } else { 'Visual Studio Build Tools will not be installed' }
  $swiftSummary = if ($script:InstallSwiftToolchain) { 'Swift toolchain will be installed' } else { 'Swift toolchain will not be installed' }
  $wendySummary = if ($script:InstallWendyCli) { 'Wendy CLI will be installed or updated' } else { 'Wendy CLI will not be installed' }
  $rdpSummary = if ($script:ConfigureRemoteDesktop) { 'Remote Desktop will be enabled' } else { 'Remote Desktop will not be changed' }
  $terminalSummary = if ($script:ConfigureTerminalDefaultPowerShell) { 'Windows Terminal will open PowerShell by default' } else { 'Windows Terminal default profile will not be changed' }
  $sshShellSummary = if ($script:ConfigureSshDefaultPowerShell) { 'SSH sessions will start in PowerShell' } else { 'SSH default shell will not be changed' }
  $sleepSummary = if ($script:DisableAcSleep) { 'AC sleep will be disabled' } else { 'AC sleep will not be changed' }
  $lockSummary = if ($script:DisableScreenLocking) { 'Screen locking will be disabled for the current user' } else { 'Screen locking will not be changed' }
  $developerModeSummary = if ($script:EnableDeveloperMode) { 'Windows Developer Mode will be enabled' } else { 'Windows Developer Mode will not be changed' }
  $direnvSummary = if ($script:InstallDirenv) { 'direnv will be installed and its PowerShell hook will be configured' } else { 'direnv will not be installed or configured' }
  $sshSummary = if ($script:EnableSshLogin) { 'SSH login via OpenSSH Server will be enabled' } else { 'SSH login will not be changed' }
  $autoLoginSummary = if ($script:SetupAutomaticLogin) { 'Automatic Windows sign-in will be enabled; the password will be stored in the registry' } else { 'Automatic Windows sign-in will not be changed' }
  $loopbackSshSummary = if ($script:ConfigureLoopbackSsh) { 'Generated SSH key will be authorized for passwordless loopback SSH' } else { 'Passwordless loopback SSH will not be configured' }
  $cloneSummary = if ($script:CloneRepository) { "$WendyRepoUrl will be cloned to $($script:CloneDestination)" } else { 'Wendy repository will not be cloned' }

  Write-Host @"

This script will configure this machine by doing the following:

  - Install packages:
      OpenSSH client, then Git, Go, GNU Make, Neovim, PowerShell 7,
      $direnvSummary
      $swiftSummary
      $vsSummary

  - Configure:
      $sshSummary
      SSH key generation for $CurrentUserName
      $sshKeySummary
      $loopbackSshSummary
      Neovim as the default CLI editor
      $direnvSummary
      $developerModeSummary
      Network discovery services and mDNS firewall rules will be enabled
      $autoLoginSummary
      $cloneSummary
      $rdpSummary
      $terminalSummary
      $sshShellSummary
      $sleepSummary
      $lockSummary
      $wendySummary
      $gitSummary

This script does not install wendy-agent because the agent runs on Linux
devices. It can install the Windows Wendy CLI when selected.
"@

  if (-not (Ask-YesNo 'Continue?' $false)) {
    Write-Host 'Aborted.'
    exit 0
  }
}

function Require-Winget {
  if (-not (Get-Command winget -ErrorAction SilentlyContinue)) {
    Fail 'winget was not found. Install App Installer from Microsoft Store, then rerun this script.'
  }
}

function Update-ProcessPath {
  $machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
  $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
  $env:Path = @($machinePath, $userPath) -join ';'
}

function Get-PrimaryIPv4Address {
  return (Get-NetIPAddress -AddressFamily IPv4 -ErrorAction SilentlyContinue |
    Where-Object { $_.IPAddress -notlike '169.254.*' -and $_.IPAddress -ne '127.0.0.1' } |
    Sort-Object InterfaceMetric |
    Select-Object -First 1 -ExpandProperty IPAddress)
}

function Add-UserPathEntry {
  param(
    [Parameter(Mandatory)][string]$PathEntry,
    [switch]$Prepend
  )

  $expanded = [Environment]::ExpandEnvironmentVariables($PathEntry)
  $current = [Environment]::GetEnvironmentVariable('Path', 'User')
  $entries = @()
  if (-not [string]::IsNullOrWhiteSpace($current)) {
    $entries = @($current -split ';' | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
  }

  $filteredEntries = @()
  $alreadyPresent = $false
  foreach ($entry in $entries) {
    if ([string]::Equals([Environment]::ExpandEnvironmentVariables($entry).TrimEnd('\'), $expanded.TrimEnd('\'), [System.StringComparison]::OrdinalIgnoreCase)) {
      $alreadyPresent = $true
      continue
    }

    $filteredEntries += $entry
  }

  if ($Prepend) {
    $newEntries = @($PathEntry) + $filteredEntries
    [Environment]::SetEnvironmentVariable('Path', ($newEntries -join ';'), 'User')
  } elseif (-not $alreadyPresent) {
    $newEntries = $filteredEntries + $PathEntry
    [Environment]::SetEnvironmentVariable('Path', ($newEntries -join ';'), 'User')
  }

  Update-ProcessPath
}

function Test-WingetPackageInstalled {
  param([Parameter(Mandatory)][string]$Id)

  $output = & winget list --id $Id --exact --source winget --disable-interactivity 2>$null
  return ($LASTEXITCODE -eq 0 -and ($output -match [regex]::Escape($Id)))
}

function Install-WingetPackage {
  param(
    [Parameter(Mandatory)][string]$Id,
    [Parameter(Mandatory)][string]$Name,
    [string]$Override = ''
  )

  if (Test-WingetPackageInstalled $Id) {
    Write-Ok "$Name is already installed"
    return
  }

  Write-Info "Installing $Name"
  $args = @(
    'install', '--id', $Id, '--exact', '--source', 'winget',
    '--accept-source-agreements', '--accept-package-agreements',
    '--disable-interactivity', '--silent'
  )
  if (-not [string]::IsNullOrWhiteSpace($Override)) {
    $args += @('--override', $Override)
  }

  Invoke-External 'winget' $args
  Update-ProcessPath
  Write-Ok "$Name installed"
}

function Add-WindowsCapabilityWithProgress {
  param(
    [Parameter(Mandatory)][string]$Name,
    [Parameter(Mandatory)][string]$DisplayName
  )

  $capability = Get-WindowsCapability -Online -Name $Name
  if ($capability.State -eq 'Installed') {
    Write-Ok "$DisplayName is already installed"
    return
  }

  Write-Host "Installing Windows capability $Name ($DisplayName)"
  Write-Host 'This can take several minutes while Windows downloads/install dependencies; DISM progress follows.'

  $args = @('/Online', '/Add-Capability', ('/CapabilityName:{0}' -f $Name), '/NoRestart')
  if ($TraceCommands -ne '0') {
    Write-Host ('+ dism.exe {0}' -f ($args -join ' ')) -ForegroundColor DarkGray
  }

  & dism.exe @args
  $exitCode = $LASTEXITCODE
  if ($exitCode -eq 3010) {
    Write-Warn "$DisplayName installed; Windows reports a reboot is required."
  } elseif ($exitCode -ne 0) {
    throw "dism.exe exited with code $exitCode while installing $DisplayName"
  }
}

function Install-OpenSshPackages {
  if ($script:EnableSshLogin) {
    Write-Info 'Installing OpenSSH server/client first'
  } else {
    Write-Info 'Installing OpenSSH client'
  }

  Add-WindowsCapabilityWithProgress -Name 'OpenSSH.Client~~~~0.0.1.0' -DisplayName 'OpenSSH Client'

  if (-not $script:EnableSshLogin) {
    Write-Ok 'OpenSSH client installed; OpenSSH Server not installed'
    return
  }

  Add-WindowsCapabilityWithProgress -Name 'OpenSSH.Server~~~~0.0.1.0' -DisplayName 'OpenSSH Server'

  Set-Service -Name sshd -StartupType Automatic
  Start-Service -Name sshd -ErrorAction SilentlyContinue

  Enable-OpenSshFirewallRule

  Write-Ok 'OpenSSH packages installed'
}

function Enable-OpenSshFirewallRule {
  $existingRule = Get-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -ErrorAction SilentlyContinue
  if ($existingRule) {
    $existingRule | Remove-NetFirewallRule
  }

  New-NetFirewallRule `
    -Name 'OpenSSH-Server-In-TCP' `
    -DisplayName 'OpenSSH Server (sshd)' `
    -Enabled True `
    -Profile Any `
    -Direction Inbound `
    -Protocol TCP `
    -Action Allow `
    -LocalPort 22 `
    -RemoteAddress Any | Out-Null

  Write-Ok 'OpenSSH firewall rule allows inbound TCP 22 on all network profiles'
}

function Test-LocalSshEndpoint {
  if (-not $script:EnableSshLogin) {
    Write-Ok 'local SSH listener not checked because SSH login was not enabled'
    return
  }

  Write-Info 'Checking local SSH listener'

  $service = Get-Service -Name sshd -ErrorAction SilentlyContinue
  if (-not $service) {
    Fail 'OpenSSH Server service (sshd) was not found after installation.'
  }

  if ($service.Status -ne 'Running') {
    Start-Service -Name sshd -ErrorAction SilentlyContinue
    $service.Refresh()
  }

  if ($service.Status -ne 'Running') {
    Write-Warn "sshd service is $($service.Status); SSH connections will fail until it is running."
    return
  }

  if (Test-NetConnection -ComputerName '127.0.0.1' -Port 22 -InformationLevel Quiet) {
    Write-Ok 'sshd is listening on localhost:22'
  } else {
    Write-Warn 'sshd is running, but localhost:22 did not answer. Check Windows Event Viewer under OpenSSH/Operational.'
  }

  $ipAddress = Get-PrimaryIPv4Address
  if (-not [string]::IsNullOrWhiteSpace($ipAddress)) {
    if (Test-NetConnection -ComputerName $ipAddress -Port 22 -InformationLevel Quiet) {
      Write-Ok "sshd is reachable from this machine at ${ipAddress}:22"
    } else {
      Write-Warn "sshd did not answer on ${ipAddress}:22 from this machine."
    }
  }

  Get-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -ErrorAction SilentlyContinue |
    Format-List Name,Enabled,Profile,Direction,Action
}

function Set-SshdConfigValue {
  param(
    [Parameter(Mandatory)][string]$Path,
    [Parameter(Mandatory)][string]$Key,
    [Parameter(Mandatory)][string]$Value
  )

  if (-not (Test-Path $Path)) {
    New-Item -ItemType File -Path $Path -Force | Out-Null
  }

  $lines = @(Get-Content -Path $Path -ErrorAction SilentlyContinue)
  $found = $false
  $pattern = '^\s*#?\s*' + [regex]::Escape($Key) + '(\s+|$)'
  for ($i = 0; $i -lt $lines.Count; $i++) {
    if ($lines[$i] -match $pattern) {
      if (-not $found) {
        $lines[$i] = "$Key $Value"
        $found = $true
      }
    }
  }

  if (-not $found) {
    $lines += "$Key $Value"
  }

  Set-Content -Path $Path -Value $lines -Encoding ascii
}

function Configure-Ssh {
  if (-not $script:EnableSshLogin) {
    Write-Ok 'SSH login not changed'
    return
  }

  Write-Info 'Enabling SSH login'

  $sshDir = Join-Path $env:ProgramData 'ssh'
  New-Item -ItemType Directory -Path $sshDir -Force | Out-Null
  $sshdConfig = Join-Path $sshDir 'sshd_config'

  Set-SshdConfigValue -Path $sshdConfig -Key 'PasswordAuthentication' -Value 'yes'
  Set-SshdConfigValue -Path $sshdConfig -Key 'KbdInteractiveAuthentication' -Value 'yes'
  Set-SshdConfigValue -Path $sshdConfig -Key 'PubkeyAuthentication' -Value 'yes'

  Restart-Service -Name sshd
  Write-Ok 'SSH login enabled'
}

function Protect-UserSshDirectory {
  param([Parameter(Mandatory)][string]$Path)
  Invoke-External 'icacls.exe' @($Path, '/inheritance:r', '/grant:r', "*${UserSid}:(OI)(CI)F", '*S-1-5-18:(OI)(CI)F') | Out-Null
}

function Protect-UserSshFile {
  param([Parameter(Mandatory)][string]$Path)
  Invoke-External 'icacls.exe' @($Path, '/inheritance:r', '/grant:r', "*${UserSid}:F", '*S-1-5-18:F') | Out-Null
}

function Protect-AdminAuthorizedKeysFile {
  param([Parameter(Mandatory)][string]$Path)
  Invoke-External 'icacls.exe' @($Path, '/inheritance:r', '/grant:r', '*S-1-5-32-544:F', '*S-1-5-18:F') | Out-Null
}

function Add-AuthorizedKey {
  param(
    [Parameter(Mandatory)][string]$Path,
    [Parameter(Mandatory)][string]$Key
  )

  $parts = $Key -split '\s+'
  if ($parts.Count -lt 2) {
    Write-Warn "Skipping malformed SSH public key: $Key"
    return
  }

  $keyType = $parts[0]
  $keyBody = $parts[1]
  $existing = @()
  if (Test-Path $Path) { $existing = @(Get-Content -Path $Path) }

  foreach ($line in $existing) {
    $lineParts = $line -split '\s+'
    if ($lineParts.Count -ge 2 -and $lineParts[0] -eq $keyType -and $lineParts[1] -eq $keyBody) {
      return
    }
  }

  Add-Content -Path $Path -Value $Key -Encoding ascii
}

function Configure-SshKeys {
  Write-Info 'Generating SSH keys and installing authorized login keys'

  $sshDir = Join-Path $UserProfile '.ssh'
  New-Item -ItemType Directory -Path $sshDir -Force | Out-Null
  Protect-UserSshDirectory $sshDir

  $privateKey = Join-Path $sshDir 'id_ed25519'
  $publicKey = Join-Path $sshDir 'id_ed25519.pub'

  if (-not (Test-Path $privateKey)) {
    Invoke-External 'ssh-keygen.exe' @('-t', 'ed25519', '-a', '100', '-N', '""', '-C', "$CurrentUserName@$env:COMPUTERNAME-$(Get-Date -Format yyyyMMdd)", '-f', $privateKey)
  } elseif (-not (Test-Path $publicKey)) {
    Invoke-External 'ssh-keygen.exe' @('-y', '-f', $privateKey) | Out-File -FilePath $publicKey -Encoding ascii
  }

  Protect-UserSshFile $privateKey
  if (Test-Path $publicKey) { Protect-UserSshFile $publicKey }

  $authorizedKeys = Join-Path $sshDir 'authorized_keys'
  if (-not (Test-Path $authorizedKeys)) { New-Item -ItemType File -Path $authorizedKeys -Force | Out-Null }

  $keysToInstall = [System.Collections.Generic.List[string]]::new()
  if ($script:ConfigureLoopbackSsh -and (Test-Path $publicKey)) {
    $generatedPublicKey = (Get-Content -Path $publicKey -Raw).Trim()
    if (-not [string]::IsNullOrWhiteSpace($generatedPublicKey)) { $keysToInstall.Add($generatedPublicKey) }
  }
  foreach ($key in $script:AuthorizedLoginKeys) { $keysToInstall.Add($key) }

  foreach ($key in $keysToInstall) {
    Add-AuthorizedKey -Path $authorizedKeys -Key $key
  }
  Protect-UserSshFile $authorizedKeys

  if ($script:ConfigureLoopbackSsh) {
    $knownHosts = Join-Path $sshDir 'known_hosts'
    if (-not (Test-Path $knownHosts)) { New-Item -ItemType File -Path $knownHosts -Force | Out-Null }
    foreach ($hostAlias in @('localhost', '127.0.0.1', '::1', $env:COMPUTERNAME, "$env:COMPUTERNAME.local")) {
      if ([string]::IsNullOrWhiteSpace($hostAlias)) { continue }
      & ssh-keygen.exe -F $hostAlias -f $knownHosts *> $null
      if ($LASTEXITCODE -ne 0) {
        $scanOutput = & ssh-keyscan.exe -T 5 -H $hostAlias 2>$null
        if ($LASTEXITCODE -eq 0 -and $scanOutput) { Add-Content -Path $knownHosts -Value $scanOutput -Encoding ascii }
      }
    }
    Protect-UserSshFile $knownHosts
  }

  if (Test-IsAdministrator -and $keysToInstall.Count -gt 0) {
    $adminKeys = Join-Path (Join-Path $env:ProgramData 'ssh') 'administrators_authorized_keys'
    if (-not (Test-Path $adminKeys)) { New-Item -ItemType File -Path $adminKeys -Force | Out-Null }
    foreach ($key in $keysToInstall) {
      Add-AuthorizedKey -Path $adminKeys -Key $key
    }
    Protect-AdminAuthorizedKeysFile $adminKeys
  }

  Write-Ok 'SSH keys configured'
}

function Install-Packages {
  Require-Winget

  Write-Info 'Updating winget sources'
  Invoke-External 'winget' @('source', 'update', '--disable-interactivity')
  Write-Ok 'winget sources updated'

  Install-WingetPackage -Id 'Git.Git' -Name 'Git'
  Install-WingetPackage -Id 'GoLang.Go' -Name 'Go'
  Install-WingetPackage -Id 'Neovim.Neovim' -Name 'Neovim'
  Install-WingetPackage -Id 'Microsoft.PowerShell' -Name 'PowerShell 7'

  if ($script:InstallVisualStudioBuildTools) {
    Install-WingetPackage `
      -Id 'Microsoft.VisualStudio.2022.BuildTools' `
      -Name 'Visual Studio Build Tools' `
      -Override '--wait --passive --norestart --add Microsoft.VisualStudio.Workload.VCTools --includeRecommended'
  } else {
    Write-Ok 'Visual Studio Build Tools not installed'
  }

  if ($script:InstallSwiftToolchain) {
    Install-WingetPackage -Id 'Swift.Toolchain' -Name 'Swift toolchain'
  } else {
    Write-Ok 'Swift toolchain not installed'
  }
}

function Get-GnuMakeVersion {
  param([Parameter(Mandatory)][string]$MakePath)

  try {
    $output = & $MakePath '--version' 2>$null
    if ($LASTEXITCODE -ne 0 -or -not $output) { return $null }

    $firstLine = @($output)[0]
    if ($firstLine -match 'GNU Make\s+(\d+)\.(\d+)(?:\.(\d+))?') {
      $patch = if ($Matches[3]) { $Matches[3] } else { '0' }
      return [version]"$($Matches[1]).$($Matches[2]).$patch"
    }
  } catch {
    return $null
  }

  return $null
}

function Find-MakeCommand {
  $commands = @(Get-Command 'make.exe', 'make.cmd', 'make.bat' -ErrorAction SilentlyContinue |
    Where-Object { $_.CommandType -eq 'Application' })

  if ($commands.Count -gt 0) { return $commands[0].Source }
  return $null
}

function Find-Msys2Bash {
  $bash = Get-Command 'bash.exe' -ErrorAction SilentlyContinue
  if ($bash -and $bash.Source -like '*\msys64\usr\bin\bash.exe') {
    return $bash.Source
  }

  $candidates = @(
    (Join-Path $env:SystemDrive 'msys64\usr\bin\bash.exe'),
    (Join-Path $env:ProgramFiles 'msys64\usr\bin\bash.exe'),
    (Join-Path ${env:ProgramFiles(x86)} 'msys64\usr\bin\bash.exe'),
    (Join-Path $env:LOCALAPPDATA 'Programs\MSYS2\usr\bin\bash.exe')
  )

  foreach ($candidate in $candidates) {
    if ($candidate -and (Test-Path $candidate)) { return $candidate }
  }

  return $null
}

function Install-GnuMake {
  $requiredVersion = [version]'4.4.0'
  $existingMake = Find-MakeCommand
  if ($existingMake) {
    $existingVersion = Get-GnuMakeVersion -MakePath $existingMake
    if ($existingVersion -and $existingVersion -ge $requiredVersion) {
      Write-Ok "GNU Make $existingVersion is already installed"
      return
    }

    Write-Warn "Existing make was found at $existingMake, but it is not GNU Make $requiredVersion or newer. Installing a modern GNU Make."
  }

  Write-Info 'Installing GNU Make using MSYS2'
  Install-WingetPackage -Id 'MSYS2.MSYS2' -Name 'MSYS2'

  $bashPath = Find-Msys2Bash
  if (-not $bashPath) { Fail 'MSYS2 was installed, but bash.exe was not found.' }

  Invoke-External $bashPath @('-lc', 'pacman -Sy --needed --noconfirm make')

  $msysBin = Split-Path $bashPath -Parent
  $msysMake = Join-Path $msysBin 'make.exe'
  if (-not (Test-Path $msysMake)) { Fail 'MSYS2 make.exe was not found after installing the make package.' }

  $installedVersion = Get-GnuMakeVersion -MakePath $msysMake
  if (-not $installedVersion -or $installedVersion -lt $requiredVersion) {
    Fail 'MSYS2 did not provide a usable modern GNU Make.'
  }

  $shimDir = Join-Path $env:LOCALAPPDATA 'Programs\GNUmake'
  New-Item -ItemType Directory -Path $shimDir -Force | Out-Null

  $makeCmd = Join-Path $shimDir 'make.cmd'
  $escapedMake = $msysMake.Replace('%', '%%')
  Set-Content -Path $makeCmd -Encoding ascii -Value @(
    '@echo off',
    "`"$escapedMake`" %*",
    'exit /b %ERRORLEVEL%'
  )

  Add-UserPathEntry -PathEntry $shimDir -Prepend
  $env:Path = "$shimDir;$env:Path"

  Write-Ok "GNU Make $installedVersion installed"
}

function Find-PowerShellExecutable {
  $candidates = @(
    (Join-Path $env:ProgramFiles 'PowerShell\7\pwsh.exe'),
    (Join-Path ${env:ProgramFiles(x86)} 'PowerShell\7\pwsh.exe')
  )

  foreach ($candidate in $candidates) {
    if ($candidate -and (Test-Path $candidate)) { return $candidate }
  }

  $command = Get-Command 'pwsh.exe' -ErrorAction SilentlyContinue
  if ($command) { return $command.Source }

  return $null
}

function Get-WindowsTerminalSettingsPath {
  $candidates = @(
    (Join-Path $env:LOCALAPPDATA 'Packages\Microsoft.WindowsTerminal_8wekyb3d8bbwe\LocalState\settings.json'),
    (Join-Path $env:LOCALAPPDATA 'Packages\Microsoft.WindowsTerminalPreview_8wekyb3d8bbwe\LocalState\settings.json'),
    (Join-Path $env:LOCALAPPDATA 'Microsoft\Windows Terminal\settings.json')
  )

  foreach ($candidate in $candidates) {
    if (Test-Path $candidate) { return $candidate }
  }

  if (Get-Command 'wt.exe' -ErrorAction SilentlyContinue) {
    return $candidates[0]
  }

  return $null
}

function Set-JsonTopLevelStringProperty {
  param(
    [Parameter(Mandatory)][string]$Path,
    [Parameter(Mandatory)][string]$Name,
    [Parameter(Mandatory)][string]$Value
  )

  $parent = Split-Path -Parent $Path
  if (-not (Test-Path $parent)) { New-Item -ItemType Directory -Path $parent -Force | Out-Null }

  $escapedName = [regex]::Escape($Name)
  $escapedValue = $Value.Replace('\', '\\').Replace('"', '\"')
  $propertyLine = '    "{0}": "{1}"' -f $Name, $escapedValue

  if (-not (Test-Path $Path)) {
    Set-Content -Path $Path -Encoding utf8 -Value @('{', $propertyLine, '}')
    return
  }

  $settings = Get-Content -Path $Path -Raw
  if ([string]::IsNullOrWhiteSpace($settings) -or $settings -match '^\s*\{\s*\}\s*$') {
    Set-Content -Path $Path -Encoding utf8 -Value @('{', $propertyLine, '}')
    return
  }

  $pattern = '(?m)^(\s*)"' + $escapedName + '"\s*:\s*"[^"]*"'
  if ([regex]::IsMatch($settings, $pattern)) {
    $replacement = '$1"{0}": "{1}"' -f $Name, $escapedValue
    $settings = [regex]::Replace($settings, $pattern, $replacement, 1)
  } else {
    $settings = [regex]::Replace($settings, '\{', "{`r`n$propertyLine,", 1)
  }

  Set-Content -Path $Path -Encoding utf8 -Value $settings
}

function Clone-Repository {
  if (-not $script:CloneRepository) {
    Write-Ok 'Wendy repository not cloned'
    return
  }

  Write-Info 'Cloning Wendy repository'
  Update-ProcessPath

  $parent = Split-Path -Parent $script:CloneDestination
  if (-not (Test-Path $parent)) { New-Item -ItemType Directory -Path $parent -Force | Out-Null }

  if (Test-Path (Join-Path $script:CloneDestination '.git')) {
    Write-Ok "Wendy repository already exists at $($script:CloneDestination)"
    return
  }

  if (Test-Path $script:CloneDestination) {
    $existing = @(Get-ChildItem -LiteralPath $script:CloneDestination -Force -ErrorAction SilentlyContinue | Select-Object -First 1)
    if ($existing.Count -gt 0) {
      Write-Warn "$($script:CloneDestination) exists and is not an empty git checkout; skipping clone."
      return
    }
  }

  Invoke-External 'git' @('clone', $WendyRepoUrl, $script:CloneDestination)
  Write-Ok "Wendy repository cloned to $($script:CloneDestination)"
}

function Configure-WindowsTerminalDefaultPowerShell {
  if (-not $script:ConfigureTerminalDefaultPowerShell) {
    Write-Ok 'Windows Terminal default profile not changed'
    return
  }

  Write-Info 'Setting Windows Terminal to open PowerShell by default'

  $settingsPath = Get-WindowsTerminalSettingsPath
  if (-not $settingsPath) {
    Write-Warn 'Windows Terminal was not found; skipping default profile configuration.'
    return
  }

  # Windows Terminal's built-in PowerShell 7 dynamic profile GUID.
  Set-JsonTopLevelStringProperty `
    -Path $settingsPath `
    -Name 'defaultProfile' `
    -Value '{574e775e-4f2a-5b96-ac1e-a2962a402336}'

  Write-Ok "Windows Terminal default profile is PowerShell ($settingsPath)"
}

function Configure-SshDefaultPowerShell {
  if (-not $script:ConfigureSshDefaultPowerShell) {
    Write-Ok 'SSH default shell not changed'
    return
  }

  Write-Info 'Setting SSH sessions to start in PowerShell'

  $pwshPath = Find-PowerShellExecutable
  if (-not $pwshPath) { Fail 'PowerShell 7 was not found after installation.' }

  $openSshPath = 'HKLM:\SOFTWARE\OpenSSH'
  if (-not (Test-Path $openSshPath)) { New-Item -Path $openSshPath -Force | Out-Null }
  New-ItemProperty -Path $openSshPath -Name 'DefaultShell' -Value $pwshPath -PropertyType String -Force | Out-Null

  Write-Ok "SSH sessions will start in PowerShell ($pwshPath)"
}

function Configure-Editor {
  Write-Info 'Setting Neovim as the default CLI editor'
  [Environment]::SetEnvironmentVariable('EDITOR', 'nvim', 'User')
  [Environment]::SetEnvironmentVariable('VISUAL', 'nvim', 'User')
  $env:EDITOR = 'nvim'
  $env:VISUAL = 'nvim'
  Write-Ok 'Neovim is the default editor for new shells'
}

function Configure-Direnv {
  if (-not $script:InstallDirenv) {
    Write-Ok 'direnv not installed or configured'
    return
  }

  Write-Info 'Installing and configuring direnv'
  Install-WingetPackage -Id 'direnv.direnv' -Name 'direnv'

  $profilePath = $PROFILE.CurrentUserAllHosts
  $profileDir = Split-Path -Parent $profilePath
  if (-not (Test-Path $profileDir)) { New-Item -ItemType Directory -Path $profileDir -Force | Out-Null }
  if (-not (Test-Path $profilePath)) { New-Item -ItemType File -Path $profilePath -Force | Out-Null }

  $hookLine = 'Invoke-Expression "$(direnv hook pwsh)"'
  $existing = @(Get-Content -Path $profilePath -ErrorAction SilentlyContinue)
  if ($existing -notcontains $hookLine) {
    Add-Content -Path $profilePath -Value ''
    Add-Content -Path $profilePath -Value $hookLine
  }

  Write-Ok "direnv installed and PowerShell hook configured in $profilePath"
}

function Configure-DeveloperMode {
  if (-not $script:EnableDeveloperMode) {
    Write-Ok 'Developer Mode not changed'
    return
  }

  Write-Info 'Enabling Windows Developer Mode'
  $path = 'HKLM:\SOFTWARE\Microsoft\Windows\CurrentVersion\AppModelUnlock'
  if (-not (Test-Path $path)) { New-Item -Path $path -Force | Out-Null }
  New-ItemProperty -Path $path -Name 'AllowDevelopmentWithoutDevLicense' -Value 1 -PropertyType DWord -Force | Out-Null
  Write-Ok 'Developer Mode enabled'
}

function Configure-NetworkDiscovery {
  Write-Info 'Enabling network discovery services and mDNS-friendly firewall rules'

  foreach ($serviceName in @('FDResPub', 'fdPHost')) {
    $service = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
    if ($service) {
      try {
        Set-Service -Name $serviceName -StartupType Automatic
        Start-Service -Name $serviceName -ErrorAction SilentlyContinue
      } catch {
        Write-Warn "Could not configure service ${serviceName}: $($_.Exception.Message)"
      }
    }
  }

  $dnsClient = Get-Service -Name 'Dnscache' -ErrorAction SilentlyContinue
  if ($dnsClient -and $dnsClient.Status -ne 'Running') {
    try {
      Start-Service -Name 'Dnscache' -ErrorAction SilentlyContinue
    } catch {
      Write-Warn "Could not start DNS Client service: $($_.Exception.Message)"
    }
  }

  Get-NetFirewallRule -DisplayGroup 'Network Discovery' -ErrorAction SilentlyContinue | Enable-NetFirewallRule | Out-Null

  $mdnsRule = Get-NetFirewallRule -DisplayName 'mDNS (UDP 5353-In)' -ErrorAction SilentlyContinue
  if ($mdnsRule) {
    Enable-NetFirewallRule -DisplayName 'mDNS (UDP 5353-In)' | Out-Null
  } else {
    New-NetFirewallRule `
      -DisplayName 'mDNS (UDP 5353-In)' `
      -Direction Inbound `
      -Action Allow `
      -Protocol UDP `
      -LocalPort 5353 | Out-Null
  }

  Write-Ok 'network discovery and mDNS firewall rules enabled where supported'
}

function Configure-AutomaticLogin {
  if (-not $script:SetupAutomaticLogin) {
    Write-Ok 'automatic Windows sign-in not changed'
    return
  }

  Write-Info 'Enabling automatic Windows sign-in'

  $bstr = [IntPtr]::Zero
  $plainPassword = $null
  try {
    $bstr = [Runtime.InteropServices.Marshal]::SecureStringToBSTR($script:AutomaticLoginPassword)
    $plainPassword = [Runtime.InteropServices.Marshal]::PtrToStringBSTR($bstr)

    $domain = $env:COMPUTERNAME
    if ($CurrentUser -like '*\*') {
      $domain = ($CurrentUser -split '\\', 2)[0]
    }

    $winlogonPath = 'HKLM:\SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon'
    Set-ItemProperty -Path $winlogonPath -Name 'AutoAdminLogon' -Value '1'
    Set-ItemProperty -Path $winlogonPath -Name 'DefaultUserName' -Value $CurrentUserName
    Set-ItemProperty -Path $winlogonPath -Name 'DefaultDomainName' -Value $domain
    Set-ItemProperty -Path $winlogonPath -Name 'DefaultPassword' -Value $plainPassword
  } finally {
    if ($bstr -ne [IntPtr]::Zero) { [Runtime.InteropServices.Marshal]::ZeroFreeBSTR($bstr) }
    $plainPassword = $null
  }

  Write-Ok 'automatic Windows sign-in configured for next boot'
}

function Configure-RemoteDesktopAccess {
  if (-not $script:ConfigureRemoteDesktop) {
    Write-Ok 'Remote Desktop not changed'
    return
  }

  Write-Info 'Enabling Remote Desktop'
  Set-ItemProperty -Path 'HKLM:\System\CurrentControlSet\Control\Terminal Server' -Name 'fDenyTSConnections' -Value 0
  Enable-NetFirewallRule -DisplayGroup 'Remote Desktop' -ErrorAction SilentlyContinue | Out-Null
  Set-Service -Name TermService -StartupType Manual
  Start-Service -Name TermService -ErrorAction SilentlyContinue
  Write-Ok 'Remote Desktop enabled where supported by this Windows edition'
}

function Configure-PowerSettings {
  if (-not $script:DisableAcSleep -and -not $script:DisableScreenLocking) {
    Write-Ok 'power and lock settings not changed'
    return
  }

  if ($script:DisableAcSleep) {
    Write-Info 'Disabling automatic sleep on AC power'

    Invoke-External 'powercfg.exe' @('/change', 'standby-timeout-ac', '0')
    Invoke-External 'powercfg.exe' @('/change', 'hibernate-timeout-ac', '0')

    Write-Ok 'AC sleep policy configured'
  } else {
    Write-Ok 'AC sleep policy not changed'
  }

  if ($script:DisableScreenLocking) {
    Write-Info 'Disabling screen locking for the current user'

    $desktopPath = 'HKCU:\Control Panel\Desktop'
    Set-ItemProperty -Path $desktopPath -Name 'ScreenSaverIsSecure' -Value '0'

    Write-Ok 'screen locking disabled for the current user'
  } else {
    Write-Ok 'screen locking not changed'
  }
}

function Install-WendyCli {
  if (-not $script:InstallWendyCli) {
    Write-Ok 'Wendy CLI not installed'
    return
  }

  Write-Info 'Installing or updating Wendy CLI'

  $arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64'; break }
    'ARM64' { 'arm64'; break }
    default { Fail "Unsupported Windows architecture: $env:PROCESSOR_ARCHITECTURE" }
  }

  $repo = 'wendylabsinc/wendy-agent'
  $tag = if ($env:WENDY_VERSION) {
    $env:WENDY_VERSION
  } else {
    (Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest" -UseBasicParsing).tag_name
  }

  if ([string]::IsNullOrWhiteSpace($tag)) { Fail 'Could not determine latest Wendy CLI version.' }

  $version = $tag.TrimStart('v')
  $artifact = "wendy-cli-windows-$arch-$version.zip"
  $url = "https://github.com/$repo/releases/download/$tag/$artifact"
  $installDir = Join-Path $UserProfile 'bin'
  $tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("wendy-cli-$([System.Guid]::NewGuid())")
  $zipPath = Join-Path $tempDir $artifact

  New-Item -ItemType Directory -Path $installDir -Force | Out-Null
  New-Item -ItemType Directory -Path $tempDir -Force | Out-Null

  try {
    Invoke-WebRequest -Uri $url -OutFile $zipPath -UseBasicParsing
    Expand-Archive -Path $zipPath -DestinationPath $tempDir -Force
    $source = Join-Path $tempDir "wendy-cli-windows-$arch\wendy.exe"
    if (-not (Test-Path $source)) { Fail "Downloaded archive did not contain $source" }
    Copy-Item -Path $source -Destination (Join-Path $installDir 'wendy.exe') -Force
  } finally {
    Remove-Item -Path $tempDir -Recurse -Force -ErrorAction SilentlyContinue
  }

  Add-UserPathEntry $installDir
  Write-Ok "Wendy CLI installed to $installDir\wendy.exe"
}

function Configure-Git {
  if (-not $script:ConfigureGit) {
    Write-Ok 'git identity not changed'
    return
  }

  Write-Info "Configuring git identity for $CurrentUser"
  Update-ProcessPath
  Invoke-External 'git' @('config', '--global', 'user.name', $script:GitName)
  Invoke-External 'git' @('config', '--global', 'user.email', $script:GitEmail)
  Write-Ok 'git identity configured'
}

function Write-Summary {
  $ipAddress = Get-PrimaryIPv4Address

  $publicKeyPath = Join-Path (Join-Path $UserProfile '.ssh') 'id_ed25519.pub'
  $publicKey = if (Test-Path $publicKeyPath) { Get-Content -Path $publicKeyPath -Raw } else { 'not available' }

  if ([string]::IsNullOrWhiteSpace($ipAddress)) { $ipAddress = 'unknown' }

  Write-Bold "`nSetup complete"
  Write-Host @"

Useful connection details:
  Username:        $CurrentUserName
  Computer name:   $env:COMPUTERNAME
  mDNS name:       $env:COMPUTERNAME.local
  Primary IP:      $ipAddress
  SSH by IP:       ssh $CurrentUserName@$ipAddress if SSH login was enabled
  SSH by name:     ssh $CurrentUserName@$env:COMPUTERNAME.local if SSH login was enabled
  Remote Desktop:  RDP to $ipAddress if Remote Desktop is supported/enabled

If the .local name does not resolve from another machine, use the Primary IP.
For password login, use the Windows account password; Windows Hello PINs do
not work over SSH.

Generated SSH public key:
  $($publicKey.Trim())

Open a new terminal for PATH/editor/environment changes to appear.
"@
}

function Main {
  Require-Windows11
  Collect-Configuration
  Confirm-Plan
  Install-OpenSshPackages
  Configure-Ssh
  Configure-SshKeys
  Test-LocalSshEndpoint
  Install-Packages
  Clone-Repository
  Configure-WindowsTerminalDefaultPowerShell
  Configure-SshDefaultPowerShell
  Install-GnuMake
  Configure-Editor
  Configure-Direnv
  Configure-DeveloperMode
  Configure-NetworkDiscovery
  Configure-AutomaticLogin
  Configure-RemoteDesktopAccess
  Configure-PowerSettings
  Install-WendyCli
  Configure-Git
  Write-Summary
}

try {
  Main
} catch {
  Write-Host "`nError: setup failed: $($_.Exception.Message)" -ForegroundColor Red
  exit 1
}
