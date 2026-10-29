# The start script of an account place's job. runner-up's Windows service
# fills in the @...@ values, writes it into the job's own folder and runs it
# there with Windows PowerShell as the job's fresh account, contained in a job
# object. Its standard input holds only the runner's one-time registration,
# which goes into the runner's environment and never into an argument.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
$registration = [Console]::In.ReadLine()
$dir = $PSScriptRoot
$zip = @RUNNER_ZIP@
Add-Type -AssemblyName System.IO.Compression.FileSystem

# Only the service may write the runner's archive; a job still unpacks only
# the bytes its pin names.
if ((Get-FileHash -Algorithm SHA256 -LiteralPath $zip).Hash -ne @RUNNER_SHA256@) {
    throw "runner-up: $zip does not match its pin"
}
$runner = Join-Path $dir 'runner'
[IO.Compression.ZipFile]::ExtractToDirectory($zip, $runner)

# shell: bash runs Git Bash where it is installed, as on GitHub's own Windows
# runners, never the WSL bash that System32 puts on PATH.
$gitBash = Join-Path $env:ProgramFiles 'Git\bin'
if (Test-Path -LiteralPath $gitBash) { $env:PATH = "$gitBash;" + $env:PATH }
# runner-up setup turns long paths on for the machine; git needs its own
# switch to use them.
$env:GIT_CONFIG_COUNT = '1'
$env:GIT_CONFIG_KEY_0 = 'core.longpaths'
$env:GIT_CONFIG_VALUE_0 = 'true'
# Scripts may run in this job's PowerShell processes, the hook and steps
# included, as on GitHub's own Windows runners; this sets no account or
# machine policy.
$env:PSExecutionPolicyPreference = 'Bypass'
@HOOK@
$env:ACTIONS_RUNNER_INPUT_JITCONFIG = $registration
Remove-Variable registration
Set-Location -LiteralPath $runner
& .\run.cmd
exit $LASTEXITCODE
