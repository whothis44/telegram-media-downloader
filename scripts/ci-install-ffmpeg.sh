#!/usr/bin/env bash
# Puts ffmpeg and ffprobe on the PATH of a GitHub Actions job (ix.12.5).
#
# Ubuntu's package mirror has crawled (about 50 KB/s on 2026-11-06) and timed
# out whole jobs. A static build downloaded from GitHub's own release storage
# comes first, another static build second, and the mirror only last.
set -u
if command -v ffmpeg >/dev/null && command -v ffprobe >/dev/null; then
  ffmpeg -hide_banner -version | head -1
  exit 0
fi
dir="${RUNNER_TEMP:-/tmp}/ffmpeg"
for url in \
  https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-n8.1-latest-linux64-gpl-8.1.tar.xz \
  https://johnvansickle.com/ffmpeg/releases/ffmpeg-release-amd64-static.tar.xz; do
  rm -rf "$dir" && mkdir -p "$dir"
  echo "trying $url"
  if curl -fsSL --retry 2 --connect-timeout 20 --max-time 240 "$url" -o "$dir/ffmpeg.tar.xz" \
    && tar -xJf "$dir/ffmpeg.tar.xz" -C "$dir" --strip-components=1; then
    bin="$dir"
    [ -x "$dir/bin/ffmpeg" ] && bin="$dir/bin"
    if [ -x "$bin/ffmpeg" ] && [ -x "$bin/ffprobe" ]; then
      echo "$bin" >> "${GITHUB_PATH:-/dev/null}"
      "$bin/ffmpeg" -hide_banner -version | head -1
      exit 0
    fi
  fi
done
echo "static builds unavailable: the package mirror"
sudo apt-get -o Acquire::Retries=1 -o Acquire::http::Timeout=30 -o Acquire::https::Timeout=30 update
sudo apt-get install -y --no-install-recommends ffmpeg
