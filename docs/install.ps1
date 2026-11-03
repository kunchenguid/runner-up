$ErrorActionPreference = "Stop"

$repo = "kunchenguid/runner-up"
$installDir = "$env:LOCALAPPDATA\runner-up"

$arch = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64" } else { "amd64" }

# Resolve the latest tag from the releases page's redirect, without the
# unauthenticated REST API's rate limit.
$latest = Invoke-WebRequest -Uri "https://github.com/$repo/releases/latest" -Method Head -UseBasicParsing
$latestUrl = if ($latest.BaseResponse.ResponseUri) {
    $latest.BaseResponse.ResponseUri.AbsoluteUri   # Windows PowerShell 5.1
} else {
    $latest.BaseResponse.RequestMessage.RequestUri.AbsoluteUri   # PowerShell 7
}
if ($latestUrl -notmatch "/releases/tag/([^/?#]+)") {
    throw "Failed to determine latest version"
}
$version = $Matches[1]
$versionNum = $version.TrimStart("v")

$filename = "runner-up-v$versionNum-windows-$arch.zip"
$url = "https://github.com/$repo/releases/download/$version/$filename"

$tmpDir = New-TemporaryFile | ForEach-Object { Remove-Item $_; New-Item -ItemType Directory -Path $_ }

Write-Host "Downloading runner-up $version for windows/$arch..."
Invoke-WebRequest -Uri $url -OutFile "$tmpDir\$filename" -UseBasicParsing
Invoke-WebRequest -Uri "https://github.com/$repo/releases/download/$version/checksums.txt" -OutFile "$tmpDir\checksums.txt" -UseBasicParsing

# Install only the archive the release's checksums.txt lists, byte for byte.
$expected = $null
foreach ($line in Get-Content "$tmpDir\checksums.txt") {
    $fields = -split $line
    if ($fields.Count -eq 2 -and $fields[1].TrimStart("*") -eq $filename) {
        $expected = $fields[0]
        break
    }
}
if (-not $expected) {
    Remove-Item -Recurse -Force $tmpDir
    throw "checksums.txt does not list $filename; not installing"
}
$actual = (Get-FileHash -Algorithm SHA256 -Path "$tmpDir\$filename").Hash
if ($actual -ne $expected) {
    Remove-Item -Recurse -Force $tmpDir
    throw "$filename does not match checksums.txt (got $actual, want $expected); not installing"
}

Expand-Archive -Path "$tmpDir\$filename" -DestinationPath $tmpDir -Force

New-Item -ItemType Directory -Path $installDir -Force | Out-Null
Move-Item -Path "$tmpDir\runner-up.exe" -Destination "$installDir\runner-up.exe" -Force

Remove-Item -Recurse -Force $tmpDir

# Add to PATH if not already there
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$installDir*") {
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$installDir", "User")
    Write-Host "Added $installDir to user PATH. Restart your terminal for it to take effect."
}

Write-Host "runner-up $version installed to $installDir\runner-up.exe"
