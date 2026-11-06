# Keeps core-service\tgdl-server.exe on the newest release of the archive
# build (whothis44/telegram-media-downloader). Run by start.bat before every
# start. It never stops the app from starting: on any problem it keeps the
# installed version. It only ever moves forward: a build newer than the latest
# release (a local fix, say) stays. Each download is checked against the
# release's SHA256SUMS before it is installed; the previous program is kept as
# tgdl-server.exe.previous.
# From ix.12.7 a release's Windows archive also carries this script: once the
# new program is installed, the script replaces itself with that copy (checked
# by the same SHA256SUMS, and only if it parses), keeping the old one as
# ix-update.ps1.previous. It runs at start, before the app, so nothing is in use.
$ErrorActionPreference = 'Stop'
$Api = if ($env:TGDL_RELEASE_API) { $env:TGDL_RELEASE_API } else { 'https://api.github.com' }
$Web = if ($env:TGDL_RELEASE_WEB) { $env:TGDL_RELEASE_WEB } else { 'https://github.com' }

# The latest release's tag from the release page, which redirects to it:
# GitHub's API allows 60 requests an hour per address without an account, and
# that allowance is shared with everything else on the network (ix.12.7).
function Get-LatestTag([string]$repo) {
    $req = [Net.HttpWebRequest]::Create("$Web/$repo/releases/latest")
    $req.Method = 'HEAD'
    $req.AllowAutoRedirect = $false
    $req.UserAgent = 'tgdl-ix-update'
    $req.Timeout = 15000
    $resp = $req.GetResponse()
    try { $loc = [string]$resp.Headers['Location'] } finally { $resp.Close() }
    if ($loc -match '/releases/tag/([^/?#]+)$') { return [Uri]::UnescapeDataString($Matches[1]) }
    throw 'the release page named no release'
}

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
$assets = $null
try {
    $rel = Invoke-RestMethod -UseBasicParsing -Headers @{ 'User-Agent' = 'tgdl-ix-update' } "$Api/repos/$Repo/releases/latest"
    $tag = [string]$rel.tag_name
    $assets = $rel.assets
} catch {
    $apiError = $_.Exception.Message
    try {
        $tag = Get-LatestTag $Repo
    } catch {
        Write-Warning "Could not check for an update ($apiError; the release page: $($_.Exception.Message)). Starting the installed version."
        exit 0
    }
}
$want = $tag.TrimStart('v')
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
if ($assets) {
    $assetUrl = ($assets | Where-Object { $_.name -eq $archive } | Select-Object -First 1).browser_download_url
    $sumsUrl = ($assets | Where-Object { $_.name -eq 'SHA256SUMS' } | Select-Object -First 1).browser_download_url
    if (-not $assetUrl -or -not $sumsUrl) {
        Write-Warning "Release $want has no $archive. Keeping version $have."
        exit 0
    }
} else {
    # Found through the release page: the files are at their usual addresses
    # (a missing one fails the download below, and nothing is installed).
    $assetUrl = "$Web/$Repo/releases/download/$tag/$archive"
    $sumsUrl = "$Web/$Repo/releases/download/$tag/SHA256SUMS"
}
$tmp = Join-Path ([IO.Path]::GetTempPath()) ([IO.Path]::GetRandomFileName())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "Updating tgdl-server $have -> $want ..."
    Invoke-WebRequest -UseBasicParsing -Uri $assetUrl -OutFile (Join-Path $tmp $archive)
    Invoke-WebRequest -UseBasicParsing -Uri $sumsUrl -OutFile (Join-Path $tmp 'SHA256SUMS')
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
    # This script, from the same checked archive (older releases have none).
    $self = $PSCommandPath
    $newSelf = Join-Path $tmp 'ix-update.ps1'
    try { & $tar -xzf (Join-Path $tmp $archive) -C $tmp ix-update.ps1 2>$null } catch {}
    if ($self -and (Test-Path -LiteralPath $newSelf -PathType Leaf)) {
        try {
            $parseErrors = $null
            [void][Management.Automation.Language.Parser]::ParseFile($newSelf, [ref]$null, [ref]$parseErrors)
            if ($parseErrors) { throw 'it does not parse' }
            if ((Get-FileHash -Algorithm SHA256 -LiteralPath $newSelf).Hash -ne (Get-FileHash -Algorithm SHA256 -LiteralPath $self).Hash) {
                Copy-Item -Force -LiteralPath $self -Destination "$self.previous"
                Copy-Item -Force -LiteralPath $newSelf -Destination $self
                Write-Host "Updated ix-update.ps1 from release $want."
            }
        } catch {
            Write-Warning "Kept this ix-update.ps1: the one in release $want was not used ($($_.Exception.Message))."
        }
    }
} catch {
    Write-Warning "$($_.Exception.Message) Keeping version $have."
} finally {
    Remove-Item -Recurse -Force -LiteralPath $tmp -ErrorAction SilentlyContinue
}
exit 0
