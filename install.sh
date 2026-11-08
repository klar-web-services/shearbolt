#!/bin/sh
# Installer for shearbolt: curl -fsSL https://tools.klar.ws/shearbolt/install.sh | sh
# Follows the tools.klar.ws installer contract, plus a wget fallback for images without curl.
# Optional environment variables:
#   SHEARBOLT_VERSION      version to install, e.g. 1.2.3 (default: latest)
#   SHEARBOLT_INSTALL_DIR  target directory (default: ~/.local/bin)
#   SHEARBOLT_BASE_URL     download location (default: https://tools.klar.ws/shearbolt)
set -eu

# Progress goes to stdout and errors to stderr, so a CI step can drop the hints and keep errors.
say() { printf '%s\n' "$*"; }
fail() { printf 'shearbolt installer: %s\n' "$*" >&2; exit 1; }

main() {
	base=${SHEARBOLT_BASE_URL:-https://tools.klar.ws/shearbolt}
	base=${base%/}
	if [ -n "${SHEARBOLT_INSTALL_DIR:-}" ]; then
		install_dir=$SHEARBOLT_INSTALL_DIR
	elif [ -n "${HOME:-}" ]; then
		install_dir="$HOME/.local/bin"
	else
		fail "set SHEARBOLT_INSTALL_DIR or HOME"
	fi
	version=${SHEARBOLT_VERSION:-}
	from=SHEARBOLT_VERSION

	if command -v curl >/dev/null 2>&1; then
		fetch() { curl -fsSL "$1" -o "$2"; }
	elif command -v wget >/dev/null 2>&1; then
		fetch() { wget -q -O "$2" "$1"; }
	else
		fail "curl or wget is required"
	fi

	case "$(uname -s)" in
	Linux) os=unknown-linux-gnu ;;
	Darwin) os=apple-darwin ;;
	*) fail "unsupported operating system: $(uname -s) (Shearbolt supports Linux and macOS)" ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch=x86_64 ;;
	arm64 | aarch64) arch=aarch64 ;;
	*) fail "unsupported architecture: $(uname -m)" ;;
	esac

	tmp=$(mktemp -d "${TMPDIR:-/tmp}/shearbolt-install.XXXXXX")
	new=
	# An interrupt exits, so the EXIT trap cleans up and nothing after it runs.
	trap 'rm -rf "$tmp"; [ -z "$new" ] || rm -f "$new"' EXIT
	trap 'exit 1' HUP INT TERM

	if [ -z "$version" ]; then
		fetch "$base/latest" "$tmp/latest" || fail "could not determine the latest version from $base/latest"
		version=$(cat "$tmp/latest")
		from="$base/latest"
	fi
	version=$(printf '%s' "$version" | tr -d '[:space:]')
	version=${version#v}
	# The value itself is not printed: it may come from the server.
	case $version in '' | *[!0-9A-Za-z._-]*) fail "invalid version from $from" ;; esac

	asset="shearbolt-$arch-$os.tar.gz"
	url="$base/v$version"
	fetch "$url/checksums.txt" "$tmp/checksums.txt" || fail "shearbolt $version not found at $url"
	expected=$(awk -v a="$asset" '$2 == a || $2 == "*" a { print $1; exit }' "$tmp/checksums.txt")
	[ -n "$expected" ] || fail "no build of shearbolt $version for $arch-$os"
	fetch "$url/$asset" "$tmp/$asset" || fail "download of $asset failed"
	# Hash and unpack from stdin: GNU sha256sum escapes a file name containing a backslash,
	# and GNU tar reads host:path as a remote archive.
	if command -v sha256sum >/dev/null 2>&1; then
		actual=$(sha256sum <"$tmp/$asset" | awk '{print $1}')
	elif command -v shasum >/dev/null 2>&1; then
		actual=$(shasum -a 256 <"$tmp/$asset" | awk '{print $1}')
	else
		fail "sha256sum or shasum is required"
	fi
	[ "$actual" = "$expected" ] || fail "checksum verification failed for $asset; nothing was installed"

	mkdir "$tmp/x"
	tar -xzf - -C "$tmp/x" <"$tmp/$asset" || fail "could not unpack $asset"
	src=$(find "$tmp/x" -type f -name shearbolt | head -n 1)
	[ -n "$src" ] || fail "shearbolt not found in $asset"
	mkdir -p "$install_dir"
	# mv would move the binary into a directory of that name and still report success.
	[ ! -d "$install_dir/shearbolt" ] || fail "could not write $install_dir/shearbolt: it is a directory"
	new="$install_dir/.shearbolt.new.$$"
	if ! install -m 0755 "$src" "$new" || ! mv -f "$new" "$install_dir/shearbolt"; then
		rm -f "$new"
		fail "could not write $install_dir/shearbolt"
	fi
	new=
	say "Installed shearbolt $version to $install_dir/shearbolt"
	case ":$PATH:" in *":$install_dir:"*) ;; *) say "Add $install_dir to your PATH." ;; esac
	say "Next: run 'shearbolt setup' in your project."
}

main "$@"
