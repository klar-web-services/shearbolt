#!/bin/sh
# Pinned Grype vulnerability-database snapshot for Shearbolt's own tests (plan 3, D3).
#
#   scripts/db-snapshot.sh fetch DIR   download the pinned archive into DIR unless a verified copy
#                                      is there, verify it, and print its absolute path
#   scripts/db-snapshot.sh refresh     maintainers: publish today's upstream archive as an asset of
#                                      the test-db pre-release and rewrite the pin
#
# The pin is testdata/db-snapshot.sha256, one line "<sha256>  <asset>", so sha256sum -c works.
# Needs curl, gh and sha256sum (or shasum); refresh also needs git and gh write access. refresh
# downloads about 200 MB into TMPDIR. Only the path is printed on stdout; messages go to stderr.
set -eu
unset CDPATH # an exported CDPATH makes cd print the directory and can redirect it

release=test-db
latest_json=https://grype.anchore.io/databases/v6/latest.json
databases=https://grype.anchore.io/databases/v6

say() { printf '%s\n' "$*" >&2; }
fail() { say "db-snapshot: $*"; exit 1; }
usage() { say "usage: scripts/db-snapshot.sh fetch DIR | refresh"; exit 2; }

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
	[ -f "$pin" ] || fail "no pin at $pin; run scripts/db-snapshot.sh refresh first"
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

# json_string prints the string value of key $1 in the JSON file $2.
json_string() {
	sed -n 's/.*"'"$1"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' "$2" | head -n 1
}

# ensure_tag makes sure origin has the test-db tag, creating it on a new orphan commit with the
# empty tree if it is missing, and checks on every run that it still points at such a commit
# (D3, D7): no tag may point into the code history.
ensure_tag() {
	empty=$(git hash-object -t tree -w /dev/null)
	if git ls-remote --exit-code --tags origin "refs/tags/$release" >/dev/null; then
		:
	else
		rc=$?
		[ "$rc" -eq 2 ] || fail "could not list the tags on origin"
		orphan=$(GIT_AUTHOR_NAME=FKWS GIT_AUTHOR_EMAIL=klar@klar.ws GIT_COMMITTER_NAME=FKWS GIT_COMMITTER_EMAIL=klar@klar.ws \
			git commit-tree "$empty" -m "Test vulnerability database snapshots (no code)")
		git push origin "$orphan:refs/tags/$release"
	fi
	git fetch --quiet --no-tags origin "refs/tags/$release" || fail "could not fetch the $release tag"
	commit=$(git rev-parse --verify --quiet 'FETCH_HEAD^{commit}') || fail "the $release tag does not point at a commit"
	[ "$(git rev-parse "$commit^{tree}")" = "$empty" ] ||
		fail "the $release tag points at a commit whose tree is not empty; not using it"
	[ "$(git rev-list --parents -n 1 "$commit")" = "$commit" ] ||
		fail "the $release tag points at a commit with a parent; it must be an orphan"
}

refresh() {
	cd "$root"
	tmp=$(mktemp -d "${TMPDIR:-/tmp}/db-snapshot.XXXXXX")
	trap 'rm -rf "$tmp"' EXIT
	trap 'exit 1' HUP INT TERM

	curl -fsSL "$latest_json" -o "$tmp/latest.json" || fail "could not read $latest_json"
	path=$(json_string path "$tmp/latest.json")
	checksum=$(json_string checksum "$tmp/latest.json")
	want=${checksum#sha256:}
	if [ "$want" = "$checksum" ] || ! hex64 "$want"; then
		fail "unexpected checksum in $latest_json: $checksum"
	fi
	case $path in
	'' | *[!A-Za-z0-9._:/-]* | *..*) fail "unexpected path in $latest_json: $path" ;;
	esac
	name=${path##*/}

	# vulnerability-db_<schema>_<YYYY-MM-DD>T<hh:mm:ss>Z_<n>.tar.zst -> grype-db-<schema>-<YYYYMMDD>T<hhmmss>Z.tar.zst
	d='[0-9]'
	safe=$(printf '%s\n' "$name" | sed -n "s/^vulnerability-db_\(v[0-9][0-9.]*\)_\($d$d$d$d\)-\($d$d\)-\($d$d\)T\($d$d\):\($d$d\):\($d$d\)Z_[0-9]*\.tar\.zst\$/grype-db-\1-\2\3\4T\5\6\7Z.tar.zst/p")
	safe_name "$safe" || fail "cannot derive an asset name from $name"

	say "Downloading $name (about 200 MB) ..."
	curl -fsSL "$databases/$path" -o "$tmp/$name" || fail "could not download $databases/$path"
	[ "$(sha256 "$tmp/$name")" = "$want" ] || fail "$name does not match the checksum in $latest_json"
	cp "$tmp/$name" "$tmp/$safe"
	[ "$(sha256 "$tmp/$safe")" = "$want" ] || fail "the copy $safe does not match"

	ensure_tag
	if ! gh release view "$release" >/dev/null 2>&1; then
		gh release create "$release" --verify-tag --prerelease --latest=false \
			--title "Test vulnerability database snapshots" \
			--notes "Unmodified Grype database archives pinned by testdata/db-snapshot.sha256. Used only by Shearbolt's own tests." >&2
	fi

	if gh release view "$release" --json assets --jq '.assets[].name' | grep -qxF -- "$safe"; then
		# Same name means the same upstream build; never replace it, old commits may pin it.
		mkdir "$tmp/existing"
		gh release download "$release" --pattern "$safe" --dir "$tmp/existing" >&2
		[ "$(sha256 "$tmp/existing/$safe")" = "$want" ] ||
			fail "the $release release already holds a different $safe; not replacing it"
		say "The $release release already holds $safe with the same checksum."
	else
		gh release upload "$release" "$tmp/$safe" >&2
	fi
	gh release view "$release" --json assets --jq '.assets[].name' | grep -qxF -- "$safe" ||
		fail "$safe is not among the stored asset names of the $release release; the pin is unchanged"

	pin="$root/testdata/db-snapshot.sha256"
	printf '%s  %s\n' "$want" "$safe" >"$pin.tmp"
	mv -f "$pin.tmp" "$pin"
	say "Pinned $safe in testdata/db-snapshot.sha256. Next:"
	say "  1. export SHEARBOLT_TEST_DB_ARCHIVE=\$(scripts/db-snapshot.sh fetch ~/.cache/shearbolt-db-snapshot)"
	say "  2. run the integration and behaviour tests on the new snapshot; update expectations if advisories changed"
	say "  3. commit testdata/db-snapshot.sha256"
}

case ${1:-} in
fetch) if [ $# -ne 2 ] || [ -z "$2" ]; then usage; fi ;;
refresh) if [ $# -ne 1 ]; then usage; fi ;;
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
if [ "$1" = fetch ]; then
	fetch "$2"
else
	refresh
fi
