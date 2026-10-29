# runner-up setup: makes a Windows machine ready for runner-up's places. The
# setup command runs it as an administrator, after these values:
#   $source  this runner-up.exe
#   $user    the account the coordinator signs in as over SSH
#   $key     a public key to let that account sign in with, or ''
#   $ssh     $true to turn on Windows' OpenSSH server
#   $hyperv  $true to turn on Hyper-V and let $user make VMs
# Running it again is harmless and updates the program. Restarting the service
# ends any job it runs.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$programDir = Join-Path $env:ProgramFiles 'runner-up'
$program = Join-Path $programDir 'runner-up.exe'
$stateDir = Join-Path $env:ProgramData 'runner-up'
$groupName = 'runner-up-jobs'
$ruleName = 'runner-up-jobs-internet-only'
$serviceName = 'runner-up'
$system = 'S-1-5-18'
$admins = 'S-1-5-32-544'

$account = Get-LocalUser -Name ($user -replace '^.*\\', '') -ErrorAction SilentlyContinue
if (-not $account) { throw "There is no local account named $user on this machine." }
$userSid = $account.SID.Value
Write-Host "Setting up runner-up for $($account.Name)."

# A fresh descriptor each time: no inherited or extra access survives.
function Set-RunnerUpAcl([string]$path, [object[]]$entries) {
    $item = Get-Item -LiteralPath $path -Force
    if ($item.Attributes -band [IO.FileAttributes]::ReparsePoint) { throw "$path is a link; nothing more changed." }
    $acl = New-Object Security.AccessControl.DirectorySecurity
    $acl.SetAccessRuleProtection($true, $false)
    $acl.SetOwner((New-Object Security.Principal.SecurityIdentifier $admins))
    foreach ($e in $entries) {
        $sid = New-Object Security.Principal.SecurityIdentifier $e[0]
        $acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule($sid, $e[1], 'ContainerInherit, ObjectInherit', 'None', 'Allow')))
    }
    Set-Acl -LiteralPath $path -AclObject $acl
}

# Only SYSTEM and Administrators change the program; everyone may run it,
# job accounts included, whose fork guard it is.
$service = Get-Service -Name $serviceName -ErrorAction SilentlyContinue
if ($service -and $service.Status -ne 'Stopped') {
    # The service ends and cleans up its jobs before it stops.
    Stop-Service -Name $serviceName
    (Get-Service -Name $serviceName).WaitForStatus('Stopped', [TimeSpan]::FromMinutes(4))
}
New-Item -ItemType Directory -Force -Path $programDir | Out-Null
Set-RunnerUpAcl $programDir @(@($system, 'FullControl'), @($admins, 'FullControl'), @('S-1-5-32-545', 'ReadAndExecute'))
if ((Resolve-Path -LiteralPath $source).Path -ne $program) { Copy-Item -LiteralPath $source -Destination $program -Force }
$path = [Environment]::GetEnvironmentVariable('Path', 'Machine')
if (($path -split ';') -notcontains $programDir) {
    [Environment]::SetEnvironmentVariable('Path', ($path.TrimEnd(';') + ';' + $programDir), 'Machine')
}

# Job accounts belong to this group, which signs in only as the service's
# batch logon: never at the screen, over Remote Desktop or from the network.
$group = Get-LocalGroup -Name $groupName -ErrorAction SilentlyContinue
if (-not $group) {
    $group = New-LocalGroup -Name $groupName -Description 'Accounts runner-up makes for one CI job each'
}
$groupSid = $group.SID.Value
foreach ($member in @(Get-LocalGroupMember -Group $group)) {
    if ($member.Name -notmatch '\\rujob-[0-9a-f]{8}$') { throw "$groupName has an unexpected member: $($member.Name)" }
}
if (-not ('RunnerUp.Rights' -as [type])) {
    Add-Type -Namespace RunnerUp -Name Rights -MemberDefinition @'
[StructLayout(LayoutKind.Sequential)] struct LsaString { public ushort Length, MaximumLength; [MarshalAs(UnmanagedType.LPWStr)] public string Buffer; }
[StructLayout(LayoutKind.Sequential)] struct LsaAttributes { public int Length; public IntPtr RootDirectory, ObjectName; public uint Attributes; public IntPtr SecurityDescriptor, SecurityQualityOfService; }
[DllImport("advapi32.dll")] static extern uint LsaOpenPolicy(IntPtr system, ref LsaAttributes attributes, uint access, out IntPtr policy);
[DllImport("advapi32.dll")] static extern uint LsaAddAccountRights(IntPtr policy, byte[] sid, LsaString[] rights, uint count);
[DllImport("advapi32.dll")] static extern uint LsaClose(IntPtr policy);
[DllImport("advapi32.dll")] static extern int LsaNtStatusToWinError(uint status);
public static void Add(string sid, string[] rights) {
    var s = new System.Security.Principal.SecurityIdentifier(sid);
    var b = new byte[s.BinaryLength]; s.GetBinaryForm(b, 0);
    var a = new LsaString[rights.Length];
    for (int i = 0; i < rights.Length; i++) { a[i].Buffer = rights[i]; a[i].Length = (ushort)(rights[i].Length * 2); a[i].MaximumLength = (ushort)(a[i].Length + 2); }
    var attributes = new LsaAttributes(); attributes.Length = Marshal.SizeOf(attributes);
    IntPtr policy;
    uint status = LsaOpenPolicy(IntPtr.Zero, ref attributes, 0x00000800 | 0x00000010, out policy);
    if (status != 0) throw new System.ComponentModel.Win32Exception(LsaNtStatusToWinError(status));
    try {
        status = LsaAddAccountRights(policy, b, a, (uint)rights.Length);
        if (status != 0) throw new System.ComponentModel.Win32Exception(LsaNtStatusToWinError(status));
    } finally { LsaClose(policy); }
}
'@
}
[RunnerUp.Rights]::Add($groupSid, @('SeBatchLogonRight', 'SeDenyInteractiveLogonRight', 'SeDenyRemoteInteractiveLogonRight', 'SeDenyNetworkLogonRight'))

# Internet only: job accounts reach no private, link-local, carrier-grade
# NAT or multicast address. DNS and loopback still work: Windows'
# DNS Client asks the resolver, not the job.
Remove-NetFirewallRule -Name $ruleName -ErrorAction SilentlyContinue
New-NetFirewallRule -Name $ruleName -DisplayName 'runner-up job accounts: internet only' -Direction Outbound -Action Block `
    -LocalUser "D:(A;;CC;;;$groupSid)" -RemoteAddress @(
        '0.0.0.0/8', '10.0.0.0/8', '100.64.0.0/10', '169.254.0.0/16', '172.16.0.0/12', '192.168.0.0/16', '224.0.0.0/4', '255.255.255.255',
        'fc00::/7', 'fe80::/10', 'ff00::/8') | Out-Null

# Long paths; git also gets core.longpaths in each job.
Set-ItemProperty -Path 'HKLM:\SYSTEM\CurrentControlSet\Control\FileSystem' -Name LongPathsEnabled -Type DWord -Value 1

# The service's settings: the one account, besides SYSTEM and
# Administrators, that may ask it for jobs.
New-Item -ItemType Directory -Force -Path $stateDir | Out-Null
Set-RunnerUpAcl $stateDir @(@($system, 'FullControl'), @($admins, 'FullControl'))
[IO.File]::WriteAllText((Join-Path $stateDir 'service.json'), (@{ user = $account.Name } | ConvertTo-Json -Compress))

$binPath = "`"$program`" service"
if (-not $service) {
    New-Service -Name $serviceName -DisplayName 'runner-up' -BinaryPathName $binPath -StartupType Automatic `
        -Description 'Runs each Windows CI job of runner-up as a fresh standard account and deletes it afterwards.' | Out-Null
} else {
    # Not sc.exe: Windows PowerShell drops the quotes inside $binPath when it
    # passes it to a program, which splits the path at its space.
    $svc = Get-CimInstance Win32_Service -Filter "Name='$serviceName'"
    if ($svc.PathName -ne $binPath -or $svc.StartMode -ne 'Auto') {
        $r = Invoke-CimMethod -InputObject $svc -MethodName Change -Arguments @{ PathName = $binPath; StartMode = 'Automatic' }
        if ($r.ReturnValue -ne 0) { throw "Could not update the runner-up service ($($r.ReturnValue))." }
    }
}
& sc.exe failure $serviceName reset= 86400 actions= restart/10000/restart/10000/restart/60000 | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Could not set the runner-up service to restart.' }
Start-Service -Name $serviceName
(Get-Service -Name $serviceName).WaitForStatus('Running', [TimeSpan]::FromMinutes(1))
$deadline = (Get-Date).AddSeconds(60)
while (-not ([IO.Directory]::GetFiles('\\.\pipe\') -contains '\\.\pipe\runner-up')) {
    if ((Get-Date) -gt $deadline) { throw "The runner-up service is running but not listening; see $stateDir\service.log." }
    Start-Sleep -Milliseconds 500
}
Write-Host 'runner-up service running.'

if ($ssh) {
    if ((Get-WindowsCapability -Online -Name 'OpenSSH.Server~~~~0.0.1.0').State -ne 'Installed') {
        Add-WindowsCapability -Online -Name 'OpenSSH.Server~~~~0.0.1.0' | Out-Null
    }
    Set-Service -Name sshd -StartupType Automatic
    Start-Service -Name sshd
    Write-Host 'OpenSSH server running.'
}
if ($key) {
    # Windows' OpenSSH server reads an administrator's keys from one shared
    # file, and anyone else's from their own folder.
    $isAdmin = @(Get-LocalGroupMember -SID $admins | Where-Object { $_.SID.Value -eq $userSid }).Count -gt 0
    if ($isAdmin) {
        $file = Join-Path $env:ProgramData 'ssh\administrators_authorized_keys'
        New-Item -ItemType Directory -Force -Path (Split-Path $file) | Out-Null
    } else {
        $profile = (Get-CimInstance Win32_UserProfile -Filter "SID='$userSid'").LocalPath
        if (-not $profile) { throw "$($account.Name) has never signed in, so it has no folder for its key yet; sign in once and run setup again." }
        $file = Join-Path $profile '.ssh\authorized_keys'
        New-Item -ItemType Directory -Force -Path (Split-Path $file) | Out-Null
    }
    $have = if (Test-Path -LiteralPath $file) { @(Get-Content -LiteralPath $file) } else { @() }
    if ($have -notcontains $key) { Add-Content -LiteralPath $file -Value $key -Encoding ascii }
    $acl = New-Object Security.AccessControl.FileSecurity
    $acl.SetAccessRuleProtection($true, $false)
    $owners = if ($isAdmin) { @($system, $admins) } else { @($system, $admins, $userSid) }
    foreach ($sid in $owners) {
        $acl.AddAccessRule((New-Object Security.AccessControl.FileSystemAccessRule((New-Object Security.Principal.SecurityIdentifier $sid), 'FullControl', 'Allow')))
    }
    Set-Acl -LiteralPath $file -AclObject $acl
    Write-Host "Key added for $($account.Name)."
}
if ($hyperv) {
    $restart = $false
    if ((Get-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V).State -ne 'Enabled') {
        $restart = (Enable-WindowsOptionalFeature -Online -FeatureName Microsoft-Hyper-V-All -All -NoRestart).RestartNeeded
    }
    # Hyper-V Administrators may make and remove VMs without being an
    # administrator.
    if (-not @(Get-LocalGroupMember -SID 'S-1-5-32-578' | Where-Object { $_.SID.Value -eq $userSid })) {
        Add-LocalGroupMember -SID 'S-1-5-32-578' -Member $userSid
    }
    Write-Host 'Hyper-V on.'
    if ($restart) { Write-Host 'Restart this machine to finish turning Hyper-V on.' -ForegroundColor Yellow }
}
Write-Host "runner-up is set up. On the coordinator: runner-up machine add NAME --ssh $($account.Name)@$env:COMPUTERNAME" -ForegroundColor Green
