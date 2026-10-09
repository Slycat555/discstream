#!/bin/sh
# Builds discstream-x86_64.AppImage: a static Go binary plus a static ffmpeg
# (with libbluray + libdvdnav) so it runs on any x86_64 Linux distribution.
set -eu

cd "$(dirname "$0")"
ROOT=$PWD
CACHE=$ROOT/.build-cache
VERSION=${VERSION:-1.0.1}
FFMPEG_URL=${FFMPEG_URL:-https://github.com/BtbN/FFmpeg-Builds/releases/download/latest/ffmpeg-n8.1-latest-linux64-gpl-8.1.tar.xz}
APPIMAGETOOL_URL=https://github.com/AppImage/appimagetool/releases/download/continuous/appimagetool-x86_64.AppImage
mkdir -p "$CACHE"

# --- toolchain -------------------------------------------------------------
if command -v go >/dev/null 2>&1; then
	GO=go
else
	if [ ! -x "$CACHE/go/bin/go" ]; then
		v=$(curl -fsSL 'https://go.dev/dl/?mode=json' | grep -o '"version": *"go[0-9.]*"' | head -1 | grep -o 'go[0-9.]*')
		echo "downloading $v"
		curl -fsSL "https://go.dev/dl/$v.linux-amd64.tar.gz" | tar xz -C "$CACHE"
	fi
	GO=$CACHE/go/bin/go
fi

if [ ! -x "$CACHE/ff/bin/ffmpeg" ]; then
	echo "downloading static ffmpeg"
	rm -rf "$CACHE/ff" && mkdir -p "$CACHE/ff"
	curl -fsSL "$FFMPEG_URL" | tar xJ -C "$CACHE/ff" --strip-components=1
fi

if [ ! -x "$CACHE/appimagetool" ]; then
	echo "downloading appimagetool"
	curl -fsSL -o "$CACHE/appimagetool" "$APPIMAGETOOL_URL"
	chmod +x "$CACHE/appimagetool"
fi

# --- AppDir ----------------------------------------------------------------
APPDIR=$CACHE/AppDir
rm -rf "$APPDIR"
mkdir -p "$APPDIR/usr/bin" "$APPDIR/usr/share/licenses/ffmpeg"

CGO_ENABLED=0 "$GO" build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o "$APPDIR/usr/bin/discstream" .
cp "$CACHE/ff/bin/ffmpeg" "$CACHE/ff/bin/ffprobe" "$APPDIR/usr/bin/"
cp "$CACHE/ff/LICENSE.txt" "$APPDIR/usr/share/licenses/ffmpeg/" 2>/dev/null || true

cat >"$APPDIR/AppRun" <<'EOF'
#!/bin/sh
HERE=$(dirname "$(readlink -f "$0")")
export APPDIR="${APPDIR:-$HERE}"
export PATH="$HERE/usr/bin:$PATH"
exec "$HERE/usr/bin/discstream" "$@"
EOF
chmod +x "$APPDIR/AppRun"

cat >"$APPDIR/discstream.desktop" <<'EOF'
[Desktop Entry]
Type=Application
Name=Disc Stream
Comment=Stream the disc in your optical drive to the local network
Exec=discstream
Icon=discstream
Terminal=false
Categories=AudioVideo;Video;
EOF

cp icon.png "$APPDIR/discstream.png"

# --- package ---------------------------------------------------------------
mkdir -p "$ROOT/dist"
OUT=$ROOT/dist/discstream-$VERSION.AppImage
# Build beside the target and rename, so it works while the old one is running.
ARCH=x86_64 APPIMAGE_EXTRACT_AND_RUN=1 "$CACHE/appimagetool" --no-appstream "$APPDIR" "$OUT.new"
mv -f "$OUT.new" "$OUT"
echo "built $OUT"
