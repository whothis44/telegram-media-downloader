# Keeps core-service\tgdl-server.exe on the newest release of the archive
# build (whothis44/telegram-media-downloader). Run by start.bat before every
# start. It never stops the app from starting: on any problem it keeps the
# installed version. Each download is checked against the release's
# SHA256SUMS before it is installed; the previous program is kept as
# tgdl-server.exe.previous.
$ErrorActionPreference = 'Stop'
$Repo = if ($env:TGDL_RELEASE_REPO) { $env:TGDL_RELEASE_REPO } else { 'whothis44/telegram-media-downloader' }
$Bin = Join-Path $PSScriptRoot 'core-service\tgdl-server.exe'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
try {
    $rel = Invoke-RestMethod -UseBasicParsing -Headers @{ 'User-Agent' = 'tgdl-ix-update' } "https://api.github.com/repos/$Repo/releases/latest"
} catch {
    Write-Warning "Could not check for an update ($($_.Exception.Message)). Starting the installed version."
    exit 0
}
$want = ([string]$rel.tag_name).TrimStart('v')
$have = ''
if (Test-Path -LiteralPath $Bin -PathType Leaf) {
    try { $have = ((& $Bin version) -split '\s+')[1] } catch { $have = '' }
}
if (-not $want -or $have -eq $want) { exit 0 }
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$archive = "tgdl-server-v$want-windows-$arch.tar.gz"
$asset = $rel.assets | Where-Object { $_.name -eq $archive } | Select-Object -First 1
$sums = $rel.assets | Where-Object { $_.name -eq 'SHA256SUMS' } | Select-Object -First 1
if (-not $asset -or -not $sums) {
    Write-Warning "Release $want has no $archive. Keeping version $have."
    exit 0
}
$tmp = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "Updating tgdl-server $have -> $want ..."
    Invoke-WebRequest -UseBasicParsing -Uri $asset.browser_download_url -OutFile (Join-Path $tmp $archive)
    Invoke-WebRequest -UseBasicParsing -Uri $sums.browser_download_url -OutFile (Join-Path $tmp 'SHA256SUMS')
    $line = Get-Content (Join-Path $tmp 'SHA256SUMS') | Where-Object { ($_ -split '\s+')[1] -in @($archive, "*$archive") } | Select-Object -First 1
    $expected = if ($line) { ($line -split '\s+')[0].ToLower() } else { '' }
    $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath (Join-Path $tmp $archive)).Hash.ToLower()
    if (-not $expected -or $expected -ne $actual) { throw "Checksum mismatch for $archive; not installed." }
    tar -xzf (Join-Path $tmp $archive) -C $tmp tgdl-server.exe
    if ($LASTEXITCODE -ne 0) { throw "Could not extract $archive." }
    if (Test-Path -LiteralPath $Bin -PathType Leaf) { Copy-Item -Force -LiteralPath $Bin -Destination "$Bin.previous" }
    Move-Item -Force -LiteralPath (Join-Path $tmp 'tgdl-server.exe') -Destination $Bin
    Write-Host "Installed tgdl-server $want."
} catch {
    Write-Warning "$($_.Exception.Message) Keeping version $have."
} finally {
    Remove-Item -Recurse -Force -LiteralPath $tmp -ErrorAction SilentlyContinue
}
exit 0
