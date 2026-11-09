#!/bin/sh
# Pinned Grype vulnerability-database snapshot for Shearbolt's own tests (plan 3, D3).
#
#   scripts/db-snapshot.sh fetch DIR   download the pinned archive into DIR unless a verified copy
#                                      is there, verify it, and print its absolute path
#
# The snapshot is built and stored by the test-db workflow (.github/workflows/test-db.yml); see
# "Refreshing the snapshot" in testdata/README.md.
#
# The pin is testdata/db-snapshot.sha256, one line "<sha256>  <asset>", so sha256sum -c works.
# Needs gh and sha256sum (or shasum). Only the path is printed on stdout; messages go to stderr.
set -eu
unset CDPATH # an exported CDPATH makes cd print the directory and can redirect it

release=test-db

say() { printf '%s\n' "$*" >&2; }
fail() { say "db-snapshot: $*"; exit 1; }
usage() { say "usage: scripts/db-snapshot.sh fetch DIR"; exit 2; }

# safe_name reports whether $1 is a valid asset name: [A-Za-z0-9._-], ending in .tar.zst.
safe_name() {
	case $1 in
	'' | *[!A-Za-z0-9._-]*) return 1 ;;
	?*.tar.zst) return 0 ;;
	*) return 1 ;;
	esac
}

# hex64 reports whether $1 is a lowercase SHA-256 hex digest.
hex64() {
	case $1 in *[!0-9a-f]* | '') return 1 ;; esac
	[ "${#1}" -eq 64 ]
}

fetch() {
	pin="$root/testdata/db-snapshot.sha256"
	[ -f "$pin" ] || fail "no pin at $pin; build a snapshot with the test-db workflow first"
	read -r want asset rest <"$pin" || fail "cannot read $pin"
	if [ -n "${rest:-}" ] || ! hex64 "$want" || ! safe_name "$asset"; then
		fail "malformed pin in $pin"
	fi
	mkdir -p "$1"
	dir=$(cd "$1" && pwd -P) # relative to the caller's directory
	file="$dir/$asset"
	if [ -f "$file" ] && [ "$(sha256 "$file")" = "$want" ]; then
		printf '%s\n' "$file"
		return
	fi
	say "Downloading $asset from the $release release ..."
	cd "$root" # gh finds the repository from the working directory
	gh release download "$release" --pattern "$asset" --dir "$dir" --clobber >&2 ||
		fail "could not download $asset from the $release release"
	if [ ! -f "$file" ] || [ "$(sha256 "$file")" != "$want" ]; then
		rm -f "$file"
		fail "snapshot $asset failed checksum verification"
	fi
	printf '%s\n' "$file"
}

case ${1:-} in
fetch) if [ $# -ne 2 ] || [ -z "$2" ]; then usage; fi ;;
*) usage ;;
esac

if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum <"$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 <"$1" | awk '{print $1}'; }
else
	fail "sha256sum or shasum is required"
fi

root=$(cd "$(dirname "$0")/.." && pwd -P)
fetch "$2"
