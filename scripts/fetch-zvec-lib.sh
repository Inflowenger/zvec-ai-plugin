#!/bin/sh
# Download the zvec C-API shared library this plugin loads at run time.
#
# zvec is a C++ engine with Go bindings, so the one thing `go build` cannot
# produce is the engine itself. This fetches the prebuilt library for the host
# platform and puts it in ./lib/<goos>_<goarch>/, which is one of the places the
# plugin looks by default — so nothing has to be configured afterwards.
#
#   ./scripts/fetch-zvec-lib.sh                 # host platform, pinned version
#   ./scripts/fetch-zvec-lib.sh -v v0.8.0       # a particular release
#   ./scripts/fetch-zvec-lib.sh -o linux -a arm64 -d ./lib   # cross-fetch
#
# Needs curl (or wget) and tar; unzip as well on Windows/MSYS.
set -eu

# VERSION is pinned to the zvec-go release the plugin is built against. The
# loader refuses a library whose C-API version does not match the binding, so
# this is not a knob to turn idly — raise it with the go.mod dependency.
VERSION="${ZVEC_VERSION:-v0.8.0}"
REPO="zvec-ai/zvec-go"
DEST=""
GOOS_OVERRIDE=""
GOARCH_OVERRIDE=""
LIBC="auto"

usage() {
	cat <<USAGE
Usage: $0 [-v version] [-d dest] [-o goos] [-a goarch] [-l glibc|musl|auto]

  -v  release to download (default: $VERSION)
  -d  destination directory (default: ./lib beside go.mod)
  -o  target OS: linux, darwin, windows (default: this host)
  -a  target arch: amd64, arm64 (default: this host)
  -l  Linux libc flavour (default: auto-detected)
USAGE
}

while [ $# -gt 0 ]; do
	case "$1" in
	-v | --version)
		VERSION="$2"
		shift 2
		;;
	-d | --dest)
		DEST="$2"
		shift 2
		;;
	-o | --os)
		GOOS_OVERRIDE="$2"
		shift 2
		;;
	-a | --arch)
		GOARCH_OVERRIDE="$2"
		shift 2
		;;
	-l | --libc)
		LIBC="$2"
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		echo "unknown option: $1" >&2
		usage >&2
		exit 2
		;;
	esac
done

# --------------------------------------------------------------- platform --

detect_os() {
	case "$(uname -s)" in
	Linux) echo linux ;;
	Darwin) echo darwin ;;
	MINGW* | MSYS* | CYGWIN* | Windows_NT) echo windows ;;
	*) echo "unsupported operating system: $(uname -s)" >&2; exit 1 ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) echo amd64 ;;
	arm64 | aarch64) echo arm64 ;;
	*) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
	esac
}

GOOS="${GOOS_OVERRIDE:-$(detect_os)}"
GOARCH="${GOARCH_OVERRIDE:-$(detect_arch)}"

# musl-based distributions (Alpine and friends) need their own build; the glibc
# one loads and then fails on a missing symbol, which is a confusing way to find
# this out.
is_musl() {
	[ "$GOOS" = linux ] || return 1
	case "$LIBC" in
	musl) return 0 ;;
	glibc) return 1 ;;
	esac
	case "$GOARCH" in
	amd64) machine=x86_64 ;;
	arm64) machine=aarch64 ;;
	*) machine="" ;;
	esac
	[ -n "$machine" ] && [ -e "/lib/ld-musl-$machine.so.1" ] && return 0
	grep -qi alpine /etc/os-release 2>/dev/null && return 0
	return 1
}

# The release assets name architectures the way the C++ project does, which is
# not the way Go does.
case "$GOARCH" in
amd64) asset_arch=x64 ;;
arm64) asset_arch=arm64 ;;
esac

echo "zvec engine $VERSION for ${GOOS}/${GOARCH}"

case "$GOOS" in
linux)
	if is_musl; then
		ASSET="zvec-libs-linux-musl-$asset_arch.tar.gz"
		echo "  musl libc detected; using the musl build."
	else
		ASSET="zvec-libs-linux-$asset_arch.tar.gz"
	fi
	LIBNAME="libzvec_c_api.so"
	;;
darwin)
	ASSET="zvec-libs-darwin-$asset_arch.tar.gz"
	LIBNAME="libzvec_c_api.dylib"
	;;
windows)
	ASSET="zvec-libs-windows-$asset_arch.zip"
	LIBNAME="zvec_c_api.dll"
	;;
esac

# ------------------------------------------------------------ destination --

if [ -z "$DEST" ]; then
	root="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
	DEST="$root/lib"
fi
mkdir -p "$DEST"

URL="https://github.com/$REPO/releases/download/$VERSION/$ASSET"
TARGET="$DEST/${GOOS}_${GOARCH}/$LIBNAME"

if [ -f "$TARGET" ]; then
	echo "Already present: $TARGET"
	echo "Delete it to re-download."
	exit 0
fi

echo "  from: $URL"
echo "  into: $DEST"

tmp="$(mktemp -d)"
# shellcheck disable=SC2064 # the path is fixed now, which is what we want
trap "rm -rf '$tmp'" EXIT INT TERM

archive="$tmp/$ASSET"
# A progress bar is worth having for 16 MB on a terminal, and only noise in a
# build log, so it follows wherever the output is going.
if [ -t 2 ]; then
	progress="--progress-bar"
else
	progress="--silent"
fi

if command -v curl >/dev/null 2>&1; then
	curl -fSL $progress --retry 3 -o "$archive" "$URL"
elif command -v wget >/dev/null 2>&1; then
	wget -q --tries=3 -O "$archive" "$URL"
else
	echo "need curl or wget to download the library" >&2
	exit 1
fi

# The archive already holds <goos>_<goarch>/<library> and include/, so it
# unpacks straight into lib/ in the layout the loader searches.
case "$ASSET" in
*.zip)
	if command -v unzip >/dev/null 2>&1; then
		unzip -oq "$archive" -d "$DEST"
	else
		echo "need unzip to extract $ASSET" >&2
		exit 1
	fi
	;;
*)
	tar -xzf "$archive" -C "$DEST"
	;;
esac

if [ ! -f "$TARGET" ]; then
	echo "the archive did not contain $TARGET — the release layout may have changed" >&2
	exit 1
fi

echo
echo "Installed $TARGET"
echo
echo "The plugin finds it there automatically when run from the project"
echo "directory, or from lib/ beside the binary. To put it anywhere else, set"
echo "ZVEC_LIBRARY_PATH to the file."
