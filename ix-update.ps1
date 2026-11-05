# Keeps core-service\tgdl-server.exe on the newest release of the archive
# build (whothis44/telegram-media-downloader). Run by start.bat before every
# start. It never stops the app from starting: on any problem it keeps the
# installed version. It only ever moves forward: a build newer than the latest
# release (a local fix, say) stays. Each download is checked against the
# release's SHA256SUMS before it is installed; the previous program is kept as
# tgdl-server.exe.previous.
$ErrorActionPreference = 'Stop'
$Api = if ($env:TGDL_RELEASE_API) { $env:TGDL_RELEASE_API } else { 'https://api.github.com' }

# Whether version $a is newer than $b: their numbers, in order (3.4.18-ix.11 is
# newer than 3.4.18-ix.10.2; a local suffix such as -fix does not count).
function Test-Newer([string]$a, [string]$b) {
    $x = @([regex]::Matches($a, '\d+') | ForEach-Object { [int64]$_.Value })
    $y = @([regex]::Matches($b, '\d+') | ForEach-Object { [int64]$_.Value })
    for ($i = 0; $i -lt [Math]::Max($x.Count, $y.Count); $i++) {
        $p = if ($i -lt $x.Count) { $x[$i] } else { -1 }
        $q = if ($i -lt $y.Count) { $y[$i] } else { -1 }
        if ($p -ne $q) { return $p -gt $q }
    }
    return $false
}

$Repo = if ($env:TGDL_RELEASE_REPO) { $env:TGDL_RELEASE_REPO } else { 'whothis44/telegram-media-downloader' }
$Bin = Join-Path $PSScriptRoot 'core-service\tgdl-server.exe'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
try {
    $rel = Invoke-RestMethod -UseBasicParsing -Headers @{ 'User-Agent' = 'tgdl-ix-update' } "$Api/repos/$Repo/releases/latest"
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
if ($have -and -not (Test-Newer $want $have)) {
    Write-Host "tgdl-server $have is not older than the latest release ($want). Keeping it."
    exit 0
}
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
    # Windows' own tar: another one found first on PATH (Git's) reads C:\ as a host name.
    $tar = Join-Path $env:SystemRoot 'System32\tar.exe'
    if (-not (Test-Path -LiteralPath $tar -PathType Leaf)) { $tar = 'tar' }
    & $tar -xzf (Join-Path $tmp $archive) -C $tmp tgdl-server.exe
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
