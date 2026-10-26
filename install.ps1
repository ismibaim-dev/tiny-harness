# tiny-harness installer for Windows. In PowerShell (no admin needed):
#   irm https://raw.githubusercontent.com/ismibaim-dev/tiny-harness/main/install.ps1 | iex
# Downloads the latest release to %LOCALAPPDATA%\Programs\tiny-harness, checks its SHA-256 and adds
# that folder to your user PATH. Run it again to update. Pin a version with $env:TINY_HARNESS_VERSION = "v0.3.1".
# Runs in a script block so nothing it sets leaks into your PowerShell session.
& {
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue' # the progress bar makes downloads very slow in Windows PowerShell 5.1
    [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

    $repo = 'ismibaim-dev/tiny-harness'
    if ($env:TINY_HARNESS_VERSION) { $base = "https://github.com/$repo/releases/download/$env:TINY_HARNESS_VERSION" }
    else { $base = "https://github.com/$repo/releases/latest/download" }
    if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { $arch = 'arm64' } else { $arch = 'amd64' }
    $asset = "tiny-harness-windows-$arch.exe"
    $dir = Join-Path $env:LOCALAPPDATA 'Programs\tiny-harness'
    $exe = Join-Path $dir 'tiny-harness.exe'
    $tmp = "$exe.download"
    $sums = Join-Path $dir 'SHA256SUMS'

    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    Write-Host "Downloading $asset ..."
    Invoke-WebRequest -UseBasicParsing -Uri "$base/$asset" -OutFile $tmp
    Invoke-WebRequest -UseBasicParsing -Uri "$base/SHA256SUMS" -OutFile $sums

    $line = Get-Content $sums | Where-Object { ($_ -split '\s+')[-1] -eq $asset }
    if (-not $line) { throw "SHA256SUMS has no entry for $asset" }
    $want = ($line -split '\s+')[0]
    $got = (Get-FileHash -Algorithm SHA256 -Path $tmp).Hash
    if ($got -ne $want) {
        Remove-Item $tmp
        throw "checksum mismatch for $asset (expected $want, got $got); nothing was installed"
    }

    # A running exe can be renamed but not overwritten, so updating works while it runs.
    Remove-Item "$exe.old" -ErrorAction SilentlyContinue
    if (Test-Path $exe) { Move-Item -Force $exe "$exe.old" }
    Move-Item $tmp $exe
    Remove-Item "$exe.old" -ErrorAction SilentlyContinue

    # Add to the user PATH via the registry so %VARIABLES% already in it stay unexpanded.
    $key = Get-Item 'HKCU:\Environment'
    $path = $key.GetValue('Path', '', 'DoNotExpandEnvironmentNames')
    if (($path -split ';') -notcontains $dir) {
        $newPath = if ($path) { "$($path.TrimEnd(';'));$dir" } else { $dir }
        Set-ItemProperty -Path 'HKCU:\Environment' -Name Path -Value $newPath -Type ExpandString
        # Setting any user variable this way broadcasts the change, so new terminals see the new PATH.
        [Environment]::SetEnvironmentVariable('TINY_HARNESS_INSTALL', '1', 'User')
        [Environment]::SetEnvironmentVariable('TINY_HARNESS_INSTALL', $null, 'User')
        Write-Host "Added $dir to your user PATH."
    }
    if (($env:Path -split ';') -notcontains $dir) { $env:Path = "$env:Path;$dir" }

    Write-Host ""
    & $exe -version
    Write-Host "Installed to $exe"
    Write-Host ""
    Write-Host "Next: set an API key, open a NEW PowerShell window, then start it in a project folder:"
    Write-Host '  setx GEMINI_API_KEY "your-key"'
    Write-Host '  tiny-harness -workdir C:\path\to\your\project'
    Write-Host "Full steps: https://github.com/$repo#install-windows-step-by-step"
}
