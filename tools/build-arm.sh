#!/bin/sh
set -eu
cd "$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"

GO=${GO:-go}
UV=${UV:-uv}
PYTHON=${PYTHON:-python3}
JOBS=${JOBS:-2}
MAKE=${MAKE:-make}
case "${1:-}" in setup | build) action=$1 ;; *)
	echo 'Usage: sh tools/build-arm.sh setup|build' >&2
	exit 2
	;;
esac
case "$JOBS" in '' | *[!0-9]*)
	echo 'JOBS must be an integer from 1 to 32.' >&2
	exit 2
	;;
esac
[ "$JOBS" -ge 1 ] && [ "$JOBS" -le 32 ] || {
	echo 'JOBS must be from 1 to 32.' >&2
	exit 2
}
for tool in "$GO" "$UV" "$PYTHON" "$MAKE" pkg-config curl tar; do
	command -v "$tool" >/dev/null || {
		echo "Required command missing: $tool" >&2
		exit 1
	}
done

cache="$PWD/.cache/arm-opus"
prefix="$cache/install"
archive="$cache/opus-1.6.1.tar.gz"
sha=6ffcb593207be92584df15b32466ed64bbec99109f007c82205f0194572411a1
mkdir -p "$cache"
work=$(mktemp -d "$cache/work.XXXXXX")
output=
trap 'rm -rf "$work"; if [ -n "$output" ]; then rm -f "$output"; fi' EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for compiler in cc ar ranlib; do
	wrapper="$PWD/.cache/arm-$compiler"
	case "$compiler" in cc) flags=' -target arm-linux-gnueabihf.2.30 -mcpu=cortex_a7' ;; *) flags= ;; esac
	printf '#!/bin/sh\nexec "${UV:-uv}" tool run --from ziglang==0.17.0 python-zig %s%s "$@"\n' "$compiler" "$flags" >"$wrapper"
	chmod 755 "$wrapper"
done
export UV
[ "$("$UV" tool run --from ziglang==0.17.0 python-zig version)" = 0.17.0 ] || {
	echo 'Unexpected Zig version.' >&2
	exit 1
}

# The absolute prefix is embedded in opus.pc; a moved cache must be rebuilt.
key="zig=0.17.0 target=arm-linux-gnueabihf.2.30 cpu=cortex_a7 opus=$sha static,no-extra,no-doc,no-intrinsics,no-rtcd,no-asm prefix=$prefix"
if [ ! -f "$prefix/.build-key" ] || [ "$(cat "$prefix/.build-key")" != "$key" ] ||
	[ ! -f "$prefix/lib/libopus.a" ] || [ ! -f "$prefix/include/opus/opus.h" ] || [ ! -f "$prefix/lib/pkgconfig/opus.pc" ]; then
	if [ ! -f "$archive" ]; then
		curl --fail --location --proto '=https' --proto-redir '=https' --tlsv1.2 \
			--output "$work/opus.tar.gz" https://downloads.xiph.org/releases/opus/opus-1.6.1.tar.gz
		mv "$work/opus.tar.gz" "$archive"
	fi
	"$PYTHON" -c 'import hashlib, pathlib, sys; actual = hashlib.sha256(pathlib.Path(sys.argv[1]).read_bytes()).hexdigest(); sys.exit(0 if actual == sys.argv[2] else "Opus archive checksum mismatch; remove the archive and rerun setup.")' "$archive" "$sha"
	tar -xzf "$archive" -C "$work"
	rm -rf "$prefix"
	(
		cd "$work/opus-1.6.1"
		CC="$OLDPWD/.cache/arm-cc" AR="$OLDPWD/.cache/arm-ar" RANLIB="$OLDPWD/.cache/arm-ranlib" \
			./configure --host=arm-linux-gnueabihf --prefix="$prefix" \
			--disable-shared --enable-static --disable-extra-programs --disable-doc \
			--disable-intrinsics --disable-rtcd --disable-asm
		"$MAKE" -j "$JOBS"
		"$MAKE" install
	)
	printf '%s\n' "$key" >"$prefix/.build-key"
fi

export PKG_CONFIG_LIBDIR="$prefix/lib/pkgconfig" PKG_CONFIG_PATH= PKG_CONFIG_SYSROOT_DIR=
[ "$(pkg-config --modversion opus)" = 1.6.1 ] || {
	echo 'ARM Opus cache version mismatch.' >&2
	exit 1
}
if [ "$action" = build ]; then
	mkdir -p build
	output=$(mktemp "$PWD/build/.yandex-station-chatgpt-voice.XXXXXX")
	CGO_ENABLED=1 GOOS=linux GOARCH=arm GOARM=7 CC="$PWD/.cache/arm-cc" \
		CGO_CFLAGS="$(pkg-config --cflags opus)" CGO_LDFLAGS="$(pkg-config --static --libs opus)" \
		"$GO" build -mod=readonly -tags=nolibopusfile -trimpath -buildvcs=false \
		-ldflags='-s -w' -o "$output" ./cmd/yandex-station-chatgpt-voice
	mv -f "$output" build/yandex-station-chatgpt-voice
	output=
	printf '%s\n' 'Built build/yandex-station-chatgpt-voice for Linux ARMv7.'
fi
