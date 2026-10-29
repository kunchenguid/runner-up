# runner-up's Hyper-V functions, for Hyper-V places. The agent runs each
# step as one PowerShell script that starts with these.
# Hyper-V through its own WMI classes (root/virtualization/v2). Windows refuses
# the Hyper-V cmdlets to a key login over OpenSSH: they read
# Win32_OperatingSystem, which Windows denies to that sign-in. The Msvm
# classes below are allowed to Hyper-V Administrators.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$script:ns = 'root/virtualization/v2'

function Get-RunnerUpVsms { Get-CimInstance -Namespace $script:ns -ClassName Msvm_VirtualSystemManagementService }

function ConvertTo-RunnerUpText($instance) {
    $ser = [Microsoft.Management.Infrastructure.Serialization.CimSerializer]::Create()
    [Text.Encoding]::Unicode.GetString($ser.Serialize($instance, [Microsoft.Management.Infrastructure.Serialization.InstanceSerializationOptions]::None))
}

# Runs a management method and waits for the job it may start.
function Invoke-RunnerUpMethod($target, [string]$method, [hashtable]$arguments) {
    $r = Invoke-CimMethod -InputObject $target -MethodName $method -Arguments $arguments
    if ($r.ReturnValue -eq 4096) {
        $job = Get-CimInstance -InputObject $r.Job
        while ($job.JobState -in 2, 3, 4) { Start-Sleep -Milliseconds 200; $job = Get-CimInstance -InputObject $job }
        if ($job.JobState -ne 7) {
            $detail = (Invoke-CimMethod -InputObject $job -MethodName GetErrorEx).Errors |
                ForEach-Object { ([xml]$_).INSTANCE.PROPERTY | Where-Object NAME -in 'Message', 'MessageArguments' | ForEach-Object { $_.VALUE; $_.'VALUE.ARRAY'.VALUE } }
            throw "$method failed: $(@($job.ErrorDescription) + @($detail) | Select-Object -Unique)"
        }
    } elseif ($r.ReturnValue -ne 0) {
        throw "$method failed: $($r.ReturnValue)"
    }
    $r
}

# The WMI path Hyper-V takes where one setting names another.
function Get-RunnerUpPath($instance) {
    $i = Get-CimInstance -InputObject $instance
    $class = $i.CimSystemProperties.ClassName
    $keys = if ($class -eq 'Msvm_VirtualEthernetSwitch') { "CreationClassName=`"$class`",Name=`"$($i.Name)`"" }
            else { "InstanceID=`"$($i.InstanceID -replace '\\', '\\')`"" }
    "\\$env:COMPUTERNAME\root\virtualization\v2:$class.$keys"
}

function Get-RunnerUpVM([string]$Name) {
    Get-CimInstance -Namespace $script:ns -ClassName Msvm_ComputerSystem -Filter "ElementName='$($Name -replace "'", "''")' AND Caption='Virtual Machine'"
}

function Get-RunnerUpSettings($vm) {
    Get-CimAssociatedInstance -InputObject $vm -ResultClassName Msvm_VirtualSystemSettingData |
        Where-Object VirtualSystemType -eq 'Microsoft:Hyper-V:System:Realized'
}

# The host's default settings for a kind of device, to fill in and add.
function Get-RunnerUpDefault([string]$class, [string]$subType) {
    $pool = Get-CimInstance -Namespace $script:ns -ClassName Msvm_ResourcePool -Filter "ResourceSubType='$subType' AND Primordial=TRUE" | Select-Object -First 1
    $caps = Get-CimAssociatedInstance -InputObject $pool -ResultClassName Msvm_AllocationCapabilities
    Get-CimAssociatedInstance -InputObject $caps -Association Msvm_SettingsDefineCapabilities -ResultClassName $class |
        Where-Object { $_.InstanceID -like '*Default' } | Select-Object -First 1
}

# Sets values on a copy of the host's settings, read-only marks aside.
function Set-RunnerUpValue($instance, [hashtable]$values) {
    foreach ($k in $values.Keys) { $instance.CimInstanceProperties[$k].Value = $values[$k] }
    $instance
}

function Add-RunnerUpResource($vm, $setting) {
    $r = Invoke-RunnerUpMethod (Get-RunnerUpVsms) AddResourceSettings @{
        AffectedConfiguration = (Get-RunnerUpSettings $vm)
        ResourceSettings = @(ConvertTo-RunnerUpText $setting)
    }
    $r.ResultingResourceSettings | Select-Object -First 1
}

function Set-RunnerUpResource($setting) {
    $null = Invoke-RunnerUpMethod (Get-RunnerUpVsms) ModifyResourceSettings @{ ResourceSettings = @(ConvertTo-RunnerUpText $setting) }
}

# Turns off and deletes every VM named Name; Hyper-V deletes its settings,
# not its disks.
function Remove-RunnerUpVM([string]$Name) {
    foreach ($vm in @(Get-RunnerUpVM $Name)) {
        if ($vm.EnabledState -ne 3) { $null = Invoke-RunnerUpMethod $vm RequestStateChange @{ RequestedState = [uint16]3 } }
        $null = Invoke-RunnerUpMethod (Get-RunnerUpVsms) DestroySystem @{ AffectedSystem = $vm }
    }
}

# A differencing disk over Parent, which nothing changes, grown to at least
# SizeBytes; cloud-init grows the VM's partition to fill it.
function New-RunnerUpDisk([string]$Path, [string]$Parent, [uint64]$SizeBytes) {
    $ims = Get-CimInstance -Namespace $script:ns -ClassName Msvm_ImageManagementService
    $sd = New-CimInstance -Namespace $script:ns -ClassName Msvm_VirtualHardDiskSettingData -ClientOnly -Property @{
        Type = [uint16]4; Format = [uint16]3; Path = $Path; ParentPath = $Parent }
    $null = Invoke-RunnerUpMethod $ims CreateVirtualHardDisk @{ VirtualDiskSettingData = (ConvertTo-RunnerUpText $sd) }
    $info = Invoke-RunnerUpMethod $ims GetVirtualHardDiskSettingData @{ Path = $Path }
    $size = [uint64](([xml]$info.SettingData).INSTANCE.PROPERTY | Where-Object NAME -eq 'MaxInternalSize').VALUE
    if ($size -lt $SizeBytes) {
        $null = Invoke-RunnerUpMethod $ims ResizeVirtualHardDisk @{ Path = $Path; MaxInternalSize = $SizeBytes }
    }
}

# A generation 2 VM with secure boot trusting Ubuntu's signed shim, static
# memory, no checkpoints, its disk and seed disc, one adapter on Switch and
# its second serial port on the named pipe Pipe.
function New-RunnerUpVM {
    param([string]$Name, [string]$Dir, [string]$Vhd, [string]$Dvd, [long]$MemoryMB, [int]$Cpu, [string]$Switch, [string]$Pipe)
    $sw = Get-CimInstance -Namespace $script:ns -ClassName Msvm_VirtualEthernetSwitch -Filter "ElementName='$($Switch -replace "'", "''")'"
    if (-not $sw) { throw "Hyper-V has no switch named $Switch" }
    $vsms = Get-RunnerUpVsms
    $vssd = New-CimInstance -Namespace $script:ns -ClassName Msvm_VirtualSystemSettingData -ClientOnly -Property @{
        ElementName = $Name
        VirtualSystemSubType = 'Microsoft:Hyper-V:SubType:2'
        ConfigurationDataRoot = $Dir; SnapshotDataRoot = $Dir; SwapFileDataRoot = $Dir
        SecureBootEnabled = $true
        SecureBootTemplateId = '272e7447-90a4-4563-a4b9-8e4ab00526ce' # MicrosoftUEFICertificateAuthority
        UserSnapshotType = [uint16]2                                    # checkpoints off
        AutomaticStartupAction = [uint16]2                              # stays off when Windows starts
        AutomaticShutdownAction = [uint16]2                             # turned off when Windows stops
    }
    $r = Invoke-RunnerUpMethod $vsms DefineSystem @{ SystemSettings = (ConvertTo-RunnerUpText $vssd) }
    $vm = Get-CimInstance -InputObject $r.ResultingSystem
    $settings = Get-RunnerUpSettings $vm

    $mem = Get-CimAssociatedInstance -InputObject $settings -ResultClassName Msvm_MemorySettingData
    Set-RunnerUpResource (Set-RunnerUpValue $mem @{ DynamicMemoryEnabled = $false
        VirtualQuantity = [uint64]$MemoryMB; Reservation = [uint64]$MemoryMB; Limit = [uint64]$MemoryMB })
    $cpuSd = Get-CimAssociatedInstance -InputObject $settings -ResultClassName Msvm_ProcessorSettingData
    Set-RunnerUpResource (Set-RunnerUpValue $cpuSd @{ VirtualQuantity = [uint64]$Cpu })

    $scsi = Add-RunnerUpResource $vm (Get-RunnerUpDefault Msvm_ResourceAllocationSettingData 'Microsoft:Hyper-V:Synthetic SCSI Controller')
    $slot = 0
    foreach ($media in @(@('Microsoft:Hyper-V:Synthetic Disk Drive', 'Microsoft:Hyper-V:Virtual Hard Disk', $Vhd),
                         @('Microsoft:Hyper-V:Synthetic DVD Drive', 'Microsoft:Hyper-V:Virtual CD/DVD Disk', $Dvd))) {
        $drive = Get-RunnerUpDefault Msvm_ResourceAllocationSettingData $media[0]
        $drivePath = Add-RunnerUpResource $vm (Set-RunnerUpValue $drive @{ Parent = (Get-RunnerUpPath $scsi); AddressOnParent = "$slot" })
        $disk = Get-RunnerUpDefault Msvm_StorageAllocationSettingData $media[1]
        $null = Add-RunnerUpResource $vm (Set-RunnerUpValue $disk @{ Parent = (Get-RunnerUpPath $drivePath); HostResource = [string[]]@($media[2]) })
        $slot++
    }

    $port = Get-RunnerUpDefault Msvm_SyntheticEthernetPortSettingData 'Microsoft:Hyper-V:Synthetic Ethernet Port'
    $portPath = Add-RunnerUpResource $vm (Set-RunnerUpValue $port @{ ElementName = 'Network Adapter'
        VirtualSystemIdentifiers = [string[]]@([guid]::NewGuid().ToString('B')) })
    $link = Get-RunnerUpDefault Msvm_EthernetPortAllocationSettingData 'Microsoft:Hyper-V:Ethernet Connection'
    $null = Add-RunnerUpResource $vm (Set-RunnerUpValue $link @{ Parent = (Get-RunnerUpPath $portPath); HostResource = [string[]]@(Get-RunnerUpPath $sw) })

    $com = Get-CimAssociatedInstance -InputObject $settings -ResultClassName Msvm_SerialPortSettingData | Where-Object ElementName -eq 'COM 2'
    if (-not $com) { throw "the VM has no second serial port" }
    Set-RunnerUpResource (Set-RunnerUpValue $com @{ Connection = [string[]]@($Pipe) })
    $vm
}

function Start-RunnerUpVM([string]$Name) {
    $null = Invoke-RunnerUpMethod (Get-RunnerUpVM $Name) RequestStateChange @{ RequestedState = [uint16]2 }
}
