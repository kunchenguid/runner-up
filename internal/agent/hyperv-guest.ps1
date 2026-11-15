# runner-up's program inside a Windows guest of a Hyper-V place. The agent
# fills in the @...@ values and writes it, as guest.ps1, onto the VM's seed
# disc beside its own runner-up.exe.
#
# The image runs it at every sign-in of its standard user runner, in that
# user's desktop session, from the root of the CD drive that holds it:
#
#   powershell.exe -NoProfile -ExecutionPolicy Bypass -WindowStyle Hidden -File X:\guest.ps1
#
# It talks to the agent over the VM's second serial port (COM2): it says it
# is ready until the agent sends the runner's one-time registration on one
# line. It downloads GitHub's runner, checks it against its pin, runs it for
# one job in the user's profile, copies the runner's output to the port and
# turns the VM off at the end, whatever happened. The registration goes into
# the runner's environment only, never into a file, an argument or this
# program's own environment.
#
# Dot-sourced, it only defines its settings and functions, for its tests.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$RunnerUrl = @RUNNER_URL@
$RunnerSHA256 = @RUNNER_SHA256@
# The disc it runs from, which holds runner-up.exe, and the user's profile.
$Disc = $PSScriptRoot
$GuestHome = $env:USERPROFILE
$Ready = 'runner-up guest: ready'

# The fork guard's hook file in $dir and the file's content, both empty when
# the place allows forks' pull requests, as runner-up decides it for every
# place (launch.go).
function Get-RunnerUpHook([string]$dir) {
    @(@HOOK@, @GUARD@)
}

# runner-up's variables for the runner: the job's size and, unless the place
# allows forks' pull requests, the fork guard as the job-started hook.
function Set-RunnerUpLaunch([string]$dir) {
@LAUNCH@
}

function Open-RunnerUpPort {
    $port = New-Object System.IO.Ports.SerialPort 'COM2', 115200
    $port.Encoding = New-Object System.Text.UTF8Encoding $false
    $port.Open()
    $port
}

function Send-RunnerUpLine($port, [string]$text) { $port.Write($text + "`n") }

# Says it is ready every two seconds until the agent's first line comes, and
# returns that line.
function Read-RunnerUpRegistration($port) {
    $got = ''
    $deadline = [DateTime]::UtcNow.AddMinutes(10)
    while ([DateTime]::UtcNow -lt $deadline) {
        Send-RunnerUpLine $port $Ready
        $until = [DateTime]::UtcNow.AddSeconds(2)
        do {
            $got += $port.ReadExisting()
            $end = $got.IndexOf("`n")
            if ($end -ge 0) { return $got.Substring(0, $end).Trim() }
            Start-Sleep -Milliseconds 100
        } while ([DateTime]::UtcNow -lt $until)
    }
    throw 'no registration from the agent'
}

# Downloads the runner and unpacks it only when its bytes match the pin, puts
# the fork guard beside a copy of runner-up's program and sets the runner's
# variables. Returns the runner's folder.
function Install-RunnerUpRunner {
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    $zip = Join-Path $GuestHome 'actions-runner.zip'
    Invoke-WebRequest -UseBasicParsing -Uri $RunnerUrl -OutFile $zip
    if ((Get-FileHash -Algorithm SHA256 -LiteralPath $zip).Hash -ne $RunnerSHA256) {
        Remove-Item -LiteralPath $zip
        throw "the runner's download does not match its pin"
    }
    $runner = Join-Path $GuestHome 'actions-runner'
    if (Test-Path -LiteralPath $runner) { Remove-Item -LiteralPath $runner -Recurse -Force }
    Add-Type -AssemblyName System.IO.Compression.FileSystem
    [IO.Compression.ZipFile]::ExtractToDirectory($zip, $runner)
    Remove-Item -LiteralPath $zip

    $dir = Join-Path $GuestHome 'runner-up'
    $null = New-Item -ItemType Directory -Force -Path $dir
    $hook, $guard = Get-RunnerUpHook $dir
    if ($hook) {
        Copy-Item -LiteralPath (Join-Path $Disc 'runner-up.exe') -Destination $dir
        [IO.File]::WriteAllText($hook, $guard)
    }
    Set-RunnerUpLaunch $dir
    # shell: bash runs Git Bash where it is installed, as on GitHub's own
    # Windows runners, never the WSL bash that System32 puts on PATH.
    $gitBash = Join-Path $env:ProgramFiles 'Git\bin'
    if (Test-Path -LiteralPath $gitBash) { $env:PATH = "$gitBash;" + $env:PATH }
    # Scripts may run in the job's PowerShell processes, the hook and steps
    # included, as on GitHub's own Windows runners; this sets no account or
    # machine policy.
    $env:PSExecutionPolicyPreference = 'Bypass'
    $runner
}

# Runs the runner in its folder with the registration in the runner's
# environment alone, copies its output to the port line by line and returns
# its exit status.
function Start-RunnerUpRunner($port, [string]$runner, [string]$registration) {
    $start = New-Object System.Diagnostics.ProcessStartInfo
    $start.FileName = Join-Path $env:SystemRoot 'System32\cmd.exe'
    $start.Arguments = '/d /c run.cmd 2>&1'
    $start.WorkingDirectory = $runner
    $start.UseShellExecute = $false
    $start.RedirectStandardInput = $true
    $start.RedirectStandardOutput = $true
    # The runner writes in the console's code page, which it shares with
    # this program.
    $start.StandardOutputEncoding = [Console]::OutputEncoding
    $start.Environment['ACTIONS_RUNNER_INPUT_JITCONFIG'] = $registration
    $p = [Diagnostics.Process]::Start($start)
    $p.StandardInput.Close()
    $out = $p.StandardOutput
    $line = $out.ReadLineAsync()
    while ($true) {
        if ($line.Wait(1000)) {
            if ($null -eq $line.Result) { break }
            Send-RunnerUpLine $port $line.Result
            $line = $out.ReadLineAsync()
        } elseif ($p.HasExited) {
            # A process the runner left behind holds its output open.
            break
        }
    }
    $p.WaitForExit()
    $p.ExitCode
}

# Turns the VM off, which ends the serial port's pipe on the machine: the
# agent's sign that the job is over.
function Stop-RunnerUpGuest { & shutdown.exe /s /t 0 /f }

function Invoke-RunnerUpGuest {
    $port = $null
    try {
        $port = Open-RunnerUpPort
        $registration = Read-RunnerUpRegistration $port
        $runner = Install-RunnerUpRunner
        Send-RunnerUpLine $port 'runner-up guest: starting the runner'
        $code = Start-RunnerUpRunner $port $runner $registration
        Remove-Variable registration
        Send-RunnerUpLine $port "runner-up guest: the runner ended with $code"
    } catch {
        if ($port) { Send-RunnerUpLine $port "runner-up guest: failed: $($_.Exception.Message)" }
    } finally {
        Stop-RunnerUpGuest
    }
}

if ($MyInvocation.InvocationName -ne '.') { Invoke-RunnerUpGuest }
