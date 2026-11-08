# Test fixtures

These fixtures feed the integration tests in `internal/integration` (real Grype). `setup/` holds the `shearbolt setup` fixtures; see "Setup fixtures" at the end.

Almost all of those tests scan with the pinned vulnerability database snapshot, `db-snapshot.sha256` (`<sha256>  <asset>`, an unmodified Grype database archive stored on the `test-db` pre-release), so their results do not change from day to day. Only `TestLiveDatabaseSmoke` uses today's database. Fetch the snapshot and run them from the repository root:

```bash
export SHEARBOLT_TEST_DB_ARCHIVE=$(scripts/db-snapshot.sh fetch ~/.cache/shearbolt-db-snapshot)
go test -tags integration -count=1 ./internal/integration/ -v -timeout 30m
```

`scripts/db-snapshot.sh refresh` (maintainers only) publishes a newer upstream archive and rewrites the pin; refresh only when the pinned Grype changes or a fixture needs newer data, then update the expectations.

The lockfiles and Rust binaries are **deliberately vulnerable test fixtures** (lodash 4.17.20, smallvec 1.6.0, jquery 3.4.1 in `npm-kev`, a CISA KEV case; the yarn-workspace lockfile also pulls in vulnerable express, qs and body-parser), so their security alerts should be dismissed too.
Dismiss security alerts (Dependabot and similar) for them, and do not "fix" their versions: the tests expect these exact findings.

Lockfiles were generated with the real package managers, never by hand. Regenerate from the repository root:

```bash
mkdir -p testdata/projects && cd testdata/projects
mkdir -p npm-vulnerable pnpm-vulnerable yarn-vulnerable bun-vulnerable npm-clean no-lockfile grype-ignore yarn-dev-only bun-dev-only nested-yarn/app yarn-workspace/packages/a nested-yarn-workspace/nest/packages/a
for d in npm-vulnerable pnpm-vulnerable yarn-vulnerable bun-vulnerable grype-ignore; do
  printf '{"name":"%s","version":"1.0.0","private":true,"dependencies":{"lodash":"4.17.20"}}\n' "$d" > "$d/package.json"
done
printf '{"name":"npm-clean","version":"1.0.0","private":true,"dependencies":{"is-number":"7.0.0"}}\n' > npm-clean/package.json
(cd npm-vulnerable && npm install --package-lock-only --ignore-scripts)
(cd npm-clean && npm install --package-lock-only --ignore-scripts)
(cd pnpm-vulnerable && pnpm install --lockfile-only --ignore-scripts)
(cd yarn-vulnerable && npx -y yarn@1.22.22 install --ignore-scripts --no-bin-links && rm -rf node_modules)
(cd bun-vulnerable && bun install --lockfile-only)
# devDependencies-only projects: Grype skips devDependencies for yarn and bun, so these lockfiles yield no packages
for d in yarn-dev-only bun-dev-only; do
  printf '{"name":"%s","version":"1.0.0","private":true,"devDependencies":{"is-number":"7.0.0"}}\n' "$d" > "$d/package.json"
done
(cd yarn-dev-only && npx -y yarn@1.22.22 install --ignore-scripts --no-bin-links && rm -rf node_modules)
(cd bun-dev-only && bun install --lockfile-only)
# nested-yarn: an independent yarn project in a subfolder. The root package.json lists lodash as a
# devDependency, which Syft would apply to app/yarn.lock too, hiding the lodash that app ships
printf '{"name":"nested-yarn","version":"1.0.0","private":true,"devDependencies":{"lodash":"4.17.20"}}\n' > nested-yarn/package.json
printf '{"name":"app","version":"1.0.0","private":true,"dependencies":{"lodash":"4.17.20","is-number":"7.0.0"}}\n' > nested-yarn/app/package.json
(cd nested-yarn/app && npx -y yarn@1.22.22 install --ignore-scripts --no-bin-links && rm -rf node_modules)
# yarn-workspace: a yarn 1 workspace root. Syft drops every package reachable from the root's devDependencies
# (body-parser), even those the member ships through express (qs, body-parser), so Shearbolt scans the lockfile as a file
printf '{"name":"yarn-workspace","version":"1.0.0","private":true,"workspaces":["packages/*"],"devDependencies":{"body-parser":"1.19.0"}}\n' > yarn-workspace/package.json
printf '{"name":"a","version":"1.0.0","dependencies":{"express":"4.17.1"}}\n' > yarn-workspace/packages/a/package.json
(cd yarn-workspace && npx -y yarn@1.22.22 install --ignore-scripts --no-bin-links && rm -rf node_modules packages/a/node_modules)
# nested-yarn-workspace: the same problem for a workspace root in a subfolder (nest/)
printf '{"name":"nested-yarn-workspace","version":"1.0.0","private":true,"devDependencies":{"lodash":"4.17.20"}}\n' > nested-yarn-workspace/package.json
printf '{"name":"nest","version":"1.0.0","private":true,"workspaces":["packages/*"],"devDependencies":{"lodash":"4.17.20"}}\n' > nested-yarn-workspace/nest/package.json
printf '{"name":"a","version":"1.0.0","dependencies":{"lodash":"4.17.20"}}\n' > nested-yarn-workspace/nest/packages/a/package.json
(cd nested-yarn-workspace/nest && npx -y yarn@1.22.22 install --ignore-scripts --no-bin-links && rm -rf node_modules packages/a/node_modules)
cp npm-vulnerable/package-lock.json grype-ignore/package-lock.json
# then set "name" to "grype-ignore" in grype-ignore/package-lock.json (two places)
printf 'ignore:\n  - package:\n      name: lodash\n' > grype-ignore/.grype.yaml
printf 'This project has no lockfile.\n' > no-lockfile/README.md
for d in npm-vulnerable npm-clean; do
  printf 'version: 1\nproject: %s\ntargets:\n  - name: repo\n    source: .\n' "$d" > "$d/shearbolt.yaml"
done
# the "supported" matrix (spec section 13): clean and broken inputs per ecosystem, plus a KEV case
mkdir -p pnpm-clean yarn-clean bun-clean npm-kev pnpm-broken yarn-broken bun-broken cargo-clean/src cargo-broken
for d in pnpm-clean yarn-clean bun-clean pnpm-broken yarn-broken bun-broken; do
  printf '{"name":"%s","version":"1.0.0","private":true,"dependencies":{"is-number":"7.0.0"}}\n' "$d" > "$d/package.json"
done
printf '{"name":"npm-kev","version":"1.0.0","private":true,"dependencies":{"jquery":"3.4.1"}}\n' > npm-kev/package.json
(cd pnpm-clean && pnpm install --lockfile-only --ignore-scripts)
(cd yarn-clean && npx -y yarn@1.22.22 install --ignore-scripts --no-bin-links && rm -rf node_modules)
(cd bun-clean && bun install --lockfile-only)
(cd npm-kev && npm install --package-lock-only --ignore-scripts)
printf 'lockfileVersion: [this is not yaml\n' > pnpm-broken/pnpm-lock.yaml
printf 'this is not a yarn lockfile {{{\n' > yarn-broken/yarn.lock
printf '{ this is not jsonc\n' > bun-broken/bun.lock
printf '[package]\nname = "cargo-clean"\nversion = "0.1.0"\nedition = "2021"\n\n[dependencies]\ncfg-if = "=1.0.0"\n' > cargo-clean/Cargo.toml
printf 'fn main() {}\n' > cargo-clean/src/main.rs
(cd cargo-clean && cargo generate-lockfile)
printf '[package]\nname = "cargo-broken"\nversion = "0.1.0"\nedition = "2021"\n' > cargo-broken/Cargo.toml
printf '[[package]\nname = this is not toml\n' > cargo-broken/Cargo.lock
```

The 2026-11-08 fixtures were generated with pnpm 11.8.0, yarn 1.22.22, bun 1.4.4, npm 11.19.1 and cargo 1.94.0.

`projects/broken-lockfile` is hand-written on purpose: its `package-lock.json` is invalid JSON (`{ this is not json`),
and its inert `.github/workflows/ci.yml` gives Grype a package (`actions/checkout`) to find anyway.
The test checks that the unread lockfile is still an error instead of a pass.
The other `-broken` fixtures (`pnpm-broken`, `yarn-broken`, `bun-broken`, `cargo-broken`) are hand-written on purpose too: each has a lockfile that its package manager could never produce, and each must give `LOCKFILE_UNREAD`.

The Rust binaries in `rust/bin` are built once and committed from `rust/demo`:

```bash
cargo install cargo-auditable --locked
cd testdata/rust/demo
cargo generate-lockfile
mkdir -p ../bin
cargo auditable build --release && cp target/release/demo ../bin/demo-auditable-linux-amd64
cargo clean
cargo build --release && cp target/release/demo ../bin/demo-plain-linux-amd64
cargo clean
```

## Setup fixtures (`testdata/setup`)

Each folder is one repository for `shearbolt setup`:

- `before/` is the repository;
- `after/` holds only the files setup creates or modifies;
- `answers.txt` holds answers as `id=value` lines (empty means the defaults);
- `expect.json` lists the expected question IDs, in order, and note codes.

`internal/setup` `TestGolden` compares setup's result with them. `go test ./internal/setup/ -run TestGolden -update` rewrites `after/` and `expect.json`; review that diff line by line. `internal/cli` `TestSetupModesAgree` runs every fixture through `--plan --json`, `--apply`, interactive mode and `--apply --yes`, which must agree. `monorepo/plan.json` pins the `--plan --json` output (`TestSetupPlanJSONGolden`, also with `-update`).

Two fixtures are scanned after setup (`TestSetupConfigScans` with a fake Grype, and `TestSetupNextJSConfigScans` and `TestSetupRustConfigScans` on the snapshot), so their lockfiles are real:

- `nextjs-build-push/before/package-lock.json` was generated with npm 11.19.1 on 2026-11-08 from its `package.json`:

  ```bash
  cd testdata/setup/nextjs-build-push/before
  printf '{"name":"storefront","private":true,"scripts":{"build":"next build"},"dependencies":{"next":"15.0.0","react":"18.3.1","react-dom":"18.3.1"}}\n' > package.json
  npm install --package-lock-only --ignore-scripts
  ```

  next 15.0.0 accepts only React 18 or a React 19 release candidate as its peer, so React is pinned to 18.3.1.
  This lockfile is deliberately vulnerable too (next 15.0.0, postcss 8.4.31, sharp 0.33.5): dismiss its alerts and do not upgrade it, because the test expects these findings.
- `rust-make/before` holds copies of `rust/demo`'s `Cargo.toml`, `Cargo.lock` and `src/main.rs`.

Every other setup fixture holds placeholder lockfiles (`{}`, an empty file, or a minimal hand-written `Cargo.lock`), because setup only checks that a lockfile exists and they are never scanned. This is a deliberate exception to the "never by hand" rule above. The versions in their `package.json` files are never installed.

`.gitignore` files inside fixtures must not match committed fixture files, because Git applies them to this repository too. Today they ignore only `node_modules/`, `.next/`, `/target` and, in `after/`, `.shearbolt/`, none of which is committed.

## README excerpt

The "What a blocked build looks like" excerpt in the top-level `README.md` is a real `summary.md` from the pinned snapshot: `npm-vulnerable` scanned as project `my-app`, with the snapshot policy (`maxDatabaseAgeHours: 1000000`, because the snapshot is soon older than the default 48 hours). Regenerate it from the repository root:

```bash
archive=$(scripts/db-snapshot.sh fetch ~/.cache/shearbolt-db-snapshot)
tmp=$(mktemp -d)
printf 'version: 1\nproject: my-app\ntargets:\n  - name: repo\n    source: %s\npolicy:\n  maxDatabaseAgeHours: 1000000\n' \
  "$PWD/testdata/projects/npm-vulnerable" > "$tmp/shearbolt.yaml"
SHEARBOLT_DB_ARCHIVE="$archive" SHEARBOLT_CACHE_DIR="$HOME/.cache/shearbolt-readme" \
  go run ./cmd/shearbolt scan --config "$tmp/shearbolt.yaml" --output-dir "$tmp/out"   # exits 1: blocked
# summary.md without the SHEARBOLT_DB_ARCHIVE note and the footer, as a Markdown quote
grep -v -e '^Vulnerability data was loaded from `SHEARBOLT_DB_ARCHIVE`' -e '^<sub>' "$tmp/out/summary.md" |
  awk 'NF { while (n > 0) { print ""; n-- } print; next } { n++ }' |
  sed -e 's/^/> /' -e 's/^> $/>/' > "$tmp/excerpt.md"
# compare with the quote under the <!-- generated: ... --> line in README.md
awk '/^<!-- generated: see testdata\/README.md "README excerpt" -->$/ { on = 1; next } on && !/^>/ { exit } on' README.md |
  diff - "$tmp/excerpt.md" && echo "README excerpt is current"
```

If the diff shows changes (a new snapshot, or a report format change), replace the quote in `README.md` with `$tmp/excerpt.md`, and update the text around it if the numbers changed. The first run imports the snapshot into `~/.cache/shearbolt-readme` (about 3.4 GB, about 70 seconds); later runs reuse it. On the 2026-11-08 snapshot the scan blocks on four lodash 4.17.20 findings, and one upgrade, to 4.18.0, clears them all.
