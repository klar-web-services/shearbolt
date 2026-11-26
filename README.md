# Shearbolt

Shearbolt stops a deployment when what you are about to ship contains a vulnerability that is exploited in the wild, likely to be exploited, or already fixed upstream.

## Protect a project

```bash
curl -fsSL https://tools.klar.ws/shearbolt/install.sh | sh
cd your-project && shearbolt setup
```

Setup finds your npm, Rust or Go project, your Docker Compose files, the images your CI builds, and your CI (GitHub Actions, Bitbucket Pipelines, GitLab CI). It shows its changes as a diff, asks before it writes, and adds a scan after your build and before any push or deploy. It never commits: review with `git diff`, commit, and your next pipeline is gated.

- `shearbolt: command not found`? The installer put it in `~/.local/bin`. Add that folder to your `PATH`.
- With Claude Code or Codex: `shearbolt skill install`, then `/shearbolt setup` (in Codex `$shearbolt setup`). `/shearbolt fix` explains a blocked build and proposes the upgrade.
- Without a terminal: `shearbolt setup --yes` accepts the defaults.
- In a monorepo, run setup once from the repository root. Each project gets its own `shearbolt.yaml`.
- What setup cannot change safely, it leaves alone and explains in a note, with the lines to add by hand.
- Run setup again after you add a workflow or a build. It gates the new part and leaves the gates that are there as they are.
- `shearbolt update` installs the latest release.

Shearbolt runs on Linux and macOS (amd64 and arm64). Windows is not supported yet.

### What setup changes

| Where | What setup does |
|---|---|
| `shearbolt.yaml` | Creates one in each project. It lists what to scan: the lockfiles, and each image, binary and Docker Compose file. |
| GitHub Actions | Adds a `shearbolt` step (`uses: klar-web-services/shearbolt@v1`) after the build. A `docker/build-push-action` step that pushes is split into build, scan and push, so you push exactly what was scanned. |
| Bitbucket Pipelines, GitLab CI | Adds two commands between the build and the push: install a pinned Shearbolt, then run `shearbolt scan --ci`. Adds a CI cache for the vulnerability database. |
| Rust builds | Changes `cargo build --release` to `cargo auditable build --release`, so the binary carries its crate list. |
| `.gitignore`, `.dockerignore` | Adds `.shearbolt/`, the folder for local scan reports. |

## What a blocked build looks like

In GitHub Actions this report is the job summary. Everywhere, it is `summary.md` in the [evidence folder](#evidence-folder). This one is from a project that pins lodash 4.17.20:

<!-- generated: see testdata/README.md "README excerpt" -->
> ### ❌ Blocked: 4 vulnerabilities must be fixed before deploying
>
> Project `my-app` · 1 target · vulnerability data built 2026-11-09 04:47 UTC
>
> #### Must fix before deploying
>
> - Upgrade lodash to 4.18.0 to clear all 4 of its blocking vulnerabilities.
>
> | Package | Installed → fixed | Vulnerability | Why it blocks | Severity | Where |
> |---|---|---|---|---|---|
> | lodash | 4.17.20 → 4.17.21 | [GHSA-29mw-wpgm-hmr9](https://nvd.nist.gov/vuln/detail/CVE-2020-28500) | fix available: upgrade to 4.17.21 | medium | repo: /package-lock.json |
> | lodash | 4.17.20 → 4.17.21 | [GHSA-35jh-r3h4-6jhm](https://nvd.nist.gov/vuln/detail/CVE-2021-23337) | exploit likelihood 21.3% (EPSS, threshold 10%); fix available: upgrade to 4.17.21 | high | repo: /package-lock.json |
> | lodash | 4.17.20 → 4.18.0 | [GHSA-r5fr-rjxr-66jc](https://github.com/lodash/lodash/security/advisories/GHSA-r5fr-rjxr-66jc) | fix available: upgrade to 4.18.0 | high | repo: /package-lock.json |
> | lodash | 4.17.20 → 4.17.23 | [GHSA-xxjr-mmjv-4gpg](https://github.com/lodash/lodash/security/advisories/GHSA-xxjr-mmjv-4gpg) | fix available: upgrade to 4.17.23 | medium | repo: /package-lock.json |

Each row says why it blocks. The bullet is the quickest way out: one upgrade clears all four.

## What blocks by default

| A finding that is | Default | Policy key |
|---|---|---|
| On CISA's [Known Exploited Vulnerabilities](https://www.cisa.gov/known-exploited-vulnerabilities-catalog) list (KEV) | Blocks, fix or not | `blockKnownExploited: true` |
| Likely to be exploited: an [EPSS](https://www.first.org/epss/) score of 10% or more | Blocks, fix or not | `blockEpssAtLeast: 0.10` |
| Fixed in a newer version, at any severity | Blocks | `blockFixable: any` (or `low`, `medium`, `high`, `critical`, `never`) |
| Anything else | Warning: listed in the report, doesn't block | |

A scan that cannot complete exits `2` and is never a pass, for example when a target is missing in CI or the vulnerability data is older than 48 hours (`maxDatabaseAgeHours`).

To change the policy, add a `policy:` block to `shearbolt.yaml` with just the keys you change:

```yaml
policy:
  blockFixable: high   # block fixable vulnerabilities only from high severity up
```

## Accepting a risk

When you accept a blocking vulnerability, add an exception to `shearbolt.yaml`:

```yaml
exceptions:
  - id: CVE-2026-12345              # the vulnerability ID, or one of its aliases
    package: brace-expansion@2.0.2  # exact name@version
    target: web                     # optional: only in this target
    reason: Only used by build tooling, never shipped
    approvedBy: alice
    expires: 2027-01-31             # at most 90 days ahead (maxExceptionDays)
```

The finding then stops blocking and is listed under **Accepted by exceptions** in the report. After an upgrade or the expiry date the exception no longer applies, and the policy judges the finding again. To require a security owner's approval, add `shearbolt.yaml` to your CODEOWNERS file.

## Run it locally

```bash
shearbolt scan                         # the targets in ./shearbolt.yaml
shearbolt scan --target source         # only this target
shearbolt scan --build                 # rebuild the <name>:local image first
shearbolt scan --image node:22-alpine  # ad hoc, without shearbolt.yaml (also --source, --file, --dir)
```

A local scan uses the same config and the same pinned Grype as CI. An image or binary you have not built yet is skipped with a hint; in CI a missing target is an error. The first scan downloads Grype and its vulnerability database into `~/.cache/shearbolt` (about 3.4 GB on disk).

Exit codes: `0` passed, `1` blocked by policy, `2` error or incomplete scan. `shearbolt -h` lists the commands, and each one prints its flags with `-h`.

## Zosia (optional)

[Zosia](https://zosiasec.com) is a dashboard for every repository Shearbolt gates. Shearbolt works the same without it.

```bash
shearbolt zosia enable
```

Run it in a repository that setup has gated. It shows a link and a code, and you approve the code where you are logged in to Zosia. Zosia then makes a secret. On GitHub, Shearbolt stores it as the repository secret `ZOSIA_SECRET` through the GitHub CLI and adds it to the gate steps in your workflows. On GitLab and Bitbucket it shows the secret once, for you to add as the CI variable `ZOSIA_SECRET`.

From then on:

- Every CI scan sends its result to Zosia, encrypted. Sending never changes the gate's result, and local scans are never sent.
- Zosia holds the policy. It starts with the one in your `shearbolt.yaml`, and you change it in the repository's settings in Zosia, without a commit. A scan that cannot reach Zosia or has no secret (a local scan, a pull request from a fork) uses the file's `policy:` block.
- Exceptions stay in `shearbolt.yaml`.
- A project with `send: false` under `zosia:` in its `shearbolt.yaml` sends nothing. `enable` offers this when your plan has no room for every project.

`shearbolt zosia disable` removes the secret from the CI files again; delete the secret itself in your CI. `shearbolt zosia -h` has the details.

## Supported projects and CI

| | Supported |
|---|---|
| npm | npm, pnpm, Yarn and Bun lockfiles (`bun.lock` as text, not `bun.lockb`) |
| Rust | Cargo projects with a `Cargo.lock`; release binaries built with cargo-auditable |
| Go | Go modules; binaries built with `go build -o <file>` |
| Docker images | Images that CI builds from a Dockerfile (`docker build`, `docker buildx build`, `docker/build-push-action`, `railpack build`) |
| Docker Compose | The images your compose files pull |
| CI | GitHub Actions, Bitbucket Pipelines, GitLab CI |

Scanning works for anything Grype understands; supported means that setup handles it and tests cover it.

## Reference

### `shearbolt.yaml`

```yaml
version: 1
project: my-app
targets:
  - name: web
    image: ${SHEARBOLT_IMAGE:-my-app:local}  # CI sets SHEARBOLT_IMAGE to the tag it just built
  - name: cli
    file: target/release/my-cli
    lockfileFallback: Cargo.lock             # used only if the binary has no embedded crate list
  - name: stack
    compose: docker-compose.yml
    exclude: [app]                           # optional: services to leave out
  - name: source
    source: .
    exclude: [examples/]                     # optional: folders to leave out
```

| Target kind | Scans |
|---|---|
| `image` | A container image, from the local Docker daemon or the registry. |
| `file` | One file: a binary or an archive. |
| `dir` | Everything in a directory, such as a built application bundle. |
| `compose` | Each image a Docker Compose file pulls. Services that build their image are not scanned. |
| `source` | The lockfiles in a source tree. |

Paths are relative to the config file. `${VAR}` and `${VAR:-default}` come from the environment. Unknown fields and invalid values are errors that name the file and line. `policy`, `exceptions` and `zosia` are optional and described above.

### GitHub Action

```yaml
- name: shearbolt
  uses: klar-web-services/shearbolt@v1
  with:
    working-directory: apps/web  # optional, for monorepos
```

| Input | Default | Meaning |
|---|---|---|
| `config` | `shearbolt.yaml` | Path to the config, relative to `working-directory`. |
| `working-directory` | `.` | Where to run, relative to the repository root. |
| `targets` | every target | Space-separated target names, for a job that builds only some of them. |
| `artifact-name` | unique per step | Name of the uploaded evidence artifact. |
| `zosia-secret` | empty | The [Zosia](#zosia-optional) secret, usually `${{ secrets.ZOSIA_SECRET }}`. |

The job fails when the scan blocks or cannot complete. `@v1` follows the newest 1.x release, and `@v1.2.3` runs exactly that release.

### Bitbucket Pipelines and GitLab CI

Two commands in the step or job that builds, after the build and before the push:

```yaml
- (curl -fsSL https://tools.klar.ws/shearbolt/install.sh || wget -qO- https://tools.klar.ws/shearbolt/install.sh) | SHEARBOLT_VERSION=1.13.0 sh
- SHEARBOLT_IMAGE="$CI_REGISTRY_IMAGE:$CI_COMMIT_SHA" "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"
```

On Bitbucket the clone directory is `$BITBUCKET_CLONE_DIR`. Cache `~/.cache/shearbolt` so the vulnerability database is not downloaded on every run. GitLab only caches paths inside the project: set `SHEARBOLT_CACHE_DIR` to one.

### Evidence folder

Every scan writes `.shearbolt/runs/<run-id>/` (or `--output-dir`): `run.json` with the full result, `summary.md` with the report, and for each target Grype's raw output and a CycloneDX SBOM under `raw/`. The GitHub Action uploads the folder as an artifact, also when the scan fails.

### Install options

The installer checks the download against the release's SHA-256 checksums. `SHEARBOLT_VERSION` pins a version and `SHEARBOLT_INSTALL_DIR` installs somewhere other than `~/.local/bin`:

```bash
curl -fsSL https://tools.klar.ws/shearbolt/install.sh | SHEARBOLT_VERSION=1.13.0 sh
```

### Uninstall

```bash
rm ~/.local/bin/shearbolt
rm -rf ~/.cache/shearbolt
```

To remove the gate from a project, delete its `shearbolt.yaml`, its `.shearbolt/` folder and the steps setup added.

## License

MIT, see [LICENSE](LICENSE).
