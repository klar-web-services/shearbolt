# Shearbolt

Shearbolt stops a deployment when what you are about to ship contains a vulnerability that is exploited in the wild, likely to be exploited, or already fixed upstream.

## Protect a project

```bash
curl -fsSL https://tools.klar.ws/shearbolt/install.sh | sh
cd your-project && shearbolt setup
```

`shearbolt: command not found`? The installer put it in `~/.local/bin`, which may not be on your `PATH` yet (the installer says so). Run `export PATH="$HOME/.local/bin:$PATH"`, add that line to your shell profile, then run setup again.

Setup finds your npm or Rust project and your CI (GitHub Actions, Bitbucket Pipelines, GitLab CI), shows its changes as a diff, and adds a scan after your build and before any push or deploy. Review with `git status` and `git diff`, commit the new and changed files, and your next pipeline is gated.

With Claude Code: `shearbolt skill install`, then type `/shearbolt setup`.

Shearbolt runs on Linux and macOS (amd64 and arm64). Windows is not supported yet.

### Install options

The installer downloads the release for your platform with `curl` (or `wget` when curl is missing), checks it against the release's SHA-256 checksums, and installs nothing if the check fails. These environment variables change what it does:

| Variable | Default | Use it to |
|---|---|---|
| `SHEARBOLT_VERSION` | the latest release | Pin a version, for example `1.0.0`. |
| `SHEARBOLT_INSTALL_DIR` | `~/.local/bin` | Install somewhere else. The installer tells you when the folder is not on your `PATH`. |
| `SHEARBOLT_BASE_URL` | `https://tools.klar.ws/shearbolt` | Download from a mirror with the same layout (`latest`, then `v<version>/checksums.txt` and the archives). |

```bash
curl -fsSL https://tools.klar.ws/shearbolt/install.sh | SHEARBOLT_VERSION=1.0.0 sh
```

### What setup changes

Run setup from your repository root: it treats the current directory as the root and looks for your CI files there. It shows every change as a diff and asks `Apply these changes? [y/N]` before it writes anything. It never commits; new files such as `shearbolt.yaml` show only in `git status`.

| Where | What setup does |
|---|---|
| `shearbolt.yaml` | Creates one in each project: a `source` target for the lockfiles, plus a target for each image or Rust binary your CI builds. |
| GitHub Actions | Adds a `shearbolt` step (`uses: klar-web-services/shearbolt@v1`) after the build. A `docker/build-push-action` step that pushes is split: it builds with `push: false` and `load: true`, `shearbolt` scans, then a new step runs `docker push` for the same tags, so you push exactly what was scanned. Other repositories can use the Action only while this repository is public ([note](#supported-projects-and-ci)). |
| Bitbucket Pipelines, GitLab CI | Adds two commands to the step or job that builds, between the build and the push: install a pinned Shearbolt, then run `shearbolt scan --ci`. A combined `docker build … && docker push …` line is split around them. Adds a CI cache for the vulnerability database. |
| Image builds | Adds a local tag, `<image>:shearbolt-<commit>`, and passes it to the gate. The gate scans that tag, and it is never pushed. |
| Rust builds | Changes `cargo build --release` to `cargo auditable build --release`, also in the Makefiles, justfiles and scripts your CI calls, and adds a step that installs cargo-auditable. |
| `.gitignore`, `.dockerignore` | Adds `.shearbolt/`, the folder for local scan reports (and the cache on GitLab), so it is never committed or copied into an image. |

- In a monorepo, run setup once from the repository root. It asks which projects to protect, and each one gets its own `shearbolt.yaml`.
- If your CI has no build step setup recognizes, it asks which job should run the gate, and gates the lockfiles there.
- What setup can't change safely, it leaves alone and explains in a note. Examples: a multi-platform image build (the gate goes before it and checks the lockfiles), a `cargo build` inside a Dockerfile, or a GitLab job that inherits its script through `extends`. When a job stays ungated, the note includes the exact lines to add by hand.
- Pushing with `docker push` drops BuildKit's provenance and SBOM attestations. Setup's notes say so when it applies.
- Running setup again is safe. It never overwrites an existing `shearbolt.yaml` (it only adds missing targets), and a second run changes nothing.
- Without a terminal, use `shearbolt setup --yes` to accept the defaults, or `--plan --json` and `--apply --answer` (see [CLI](#cli)).

## What a blocked build looks like

In GitHub Actions this report is the job summary. On every CI and on your machine, it is `summary.md` in the [evidence folder](#evidence-folder). This one is real: a project that pins lodash 4.17.20, scanned with the pinned vulnerability database that Shearbolt's own tests use.

<!-- generated: see testdata/README.md "README excerpt" -->
> ### ❌ Blocked: 4 vulnerabilities must be fixed before deploying
>
> Project `my-app` · 1 target · vulnerability data built 2026-11-08 06:19 UTC
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

The bullet is the quickest way out: one upgrade clears all four. Each row says why it blocks. The job log shows the same report as plain text, including:

```text
To unblock:
  upgrade lodash to 4.18.0 to clear all 4
```

That upgrade line appears for npm packages and Rust crates when one version clears every blocking finding of the package.

## What blocks by default

| A finding that is | Default |
|---|---|
| On CISA's [Known Exploited Vulnerabilities](https://www.cisa.gov/known-exploited-vulnerabilities-catalog) list (KEV) | Blocks, fix or not |
| Likely to be exploited: an [EPSS](https://www.first.org/epss/) score of 10% or more | Blocks, fix or not |
| Fixed in a newer version, at any severity | Blocks |
| Anything else | Warning: listed in the report, doesn't block |

KEV and EPSS mark the vulnerabilities that attackers use or are likely to use, so they block even without a fix: you decide about them before you deploy, not after. A vulnerability that is already fixed upstream costs only an upgrade, so it blocks until you take it, while everything else stays visible as a warning without stopping your deploys.

A blocking finding lists every reason that applies. Separately, a scan that cannot complete exits `2` and is never a pass: for example when Grype fails, a target is missing in CI, or the vulnerability data is older than 48 hours (`maxDatabaseAgeHours`).

To change the policy, add a `policy:` block to `shearbolt.yaml` with just the keys you change. The others keep their defaults.

```yaml
policy:
  blockFixable: high   # block fixable vulnerabilities only from high severity up
```

| Key | Default | What it does |
|---|---|---|
| `blockKnownExploited` | `true` | Block findings on the KEV list (including through a related vulnerability), fix or not. |
| `blockEpssAtLeast` | `0.10` | Block findings whose highest EPSS score is at least this, fix or not. Above 0, at most 1. |
| `blockFixable` | `any` | Block findings that have a fix and at least this severity: `any`, `low`, `medium`, `high`, `critical` or `never`. An unknown severity counts as meeting any level. |
| `maxDatabaseAgeHours` | `48` | Fail the scan (exit `2`) when the vulnerability data is older than this. |
| `maxExceptionDays` | `90` | How far ahead an exception may expire, in days. |

## Accepting a risk

When you accept a blocking vulnerability, for example because the package is only used by build tooling and never shipped, add an exception to `shearbolt.yaml`:

```yaml
exceptions:
  - id: CVE-2026-12345              # the vulnerability ID, or one of its aliases
    package: brace-expansion@2.0.2  # exact name@version
    target: web                     # optional: only in this target
    reason: Only used by build tooling, never shipped
    approvedBy: alice
    expires: 2027-01-31             # at most 90 days ahead; valid through the end of this day, UTC
```

- `package` is an exact `name@version`, with no wildcards or ranges. Scoped npm names keep their `@`, as in `@babel/traverse@7.22.5`. After an upgrade the exception no longer matches.
- `expires` is at most `maxExceptionDays` (90 by default) after the day of the scan. A later date makes the config invalid, and every scan exits `2`.
- Every field is required except `target`, which must name a target in the config.
- Exceptions are reviewed like code: commit them with the change. If exceptions and policy changes should need a security owner's approval, add `shearbolt.yaml` to your CODEOWNERS file.

In the report, an excepted finding does not block. It is listed under **Accepted by exceptions**, with who approved it, until when, and why. Once the exception expires, it is ignored: the policy judges the finding again, and a warning names the expired exception.

## Run it locally

```bash
shearbolt scan                                   # the targets in ./shearbolt.yaml
shearbolt scan --config apps/web/shearbolt.yaml  # another project's config
shearbolt scan --target source                   # only this target (repeatable)
```

A local scan uses the same config and the same pinned Grype as CI, so you can find and fix most blocks before you pay for a pipeline run.

- Source lockfiles are always scanned.
- An image, binary or directory you have not built yet is skipped, with a hint on how to build it, and the report lists it. Images are read from your local Docker when they are there, otherwise from the registry. Setup's configs look for `<name>:local` on your machine (`image: ${SHEARBOLT_IMAGE:-<name>:local}`), so `docker build -t <name>:local .` includes the image.
- If every target is skipped, the scan is an error. In CI, a missing target is always an error, never a skip.

Ad-hoc flags scan just what they name, with the default policy, and ignore `shearbolt.yaml`:

```bash
shearbolt scan --source .                     # the lockfiles in this tree
shearbolt scan --image node:22-alpine         # a container image
shearbolt scan --file target/release/my-cli   # a binary
shearbolt scan --dir dist/                    # everything in a folder
```

Each flag can be repeated. Combining them with `--config` is an error.

The first scan downloads the pinned Grype release, verified by its SHA-256, and Grype's vulnerability database (about 200 MB, which takes about 3.4 GB once imported) into `~/.cache/shearbolt`. Scans need network access.

| Exit code | Meaning |
|---|---|
| `0` | Passed. |
| `1` | Blocked by policy. The report says what to upgrade. |
| `2` | Error: an invalid config or usage, a scanner failure, or an incomplete scan. Nothing is approved. |

## Supported projects and CI

| | Supported |
|---|---|
| npm | npm, pnpm, Yarn and Bun lockfiles (`package-lock.json`, `npm-shrinkwrap.json`, `pnpm-lock.yaml`, `yarn.lock`, text `bun.lock`); Next.js apps |
| Rust | Cargo projects with a `Cargo.lock`; release binaries built with cargo-auditable |
| CI | GitHub Actions, Bitbucket Pipelines, GitLab CI |
| Targets | Container images (local Docker or registry), binaries and other single files, directories, source trees |

Scanning works for anything Grype understands; supported means setup support and tests.

- Grype cannot read Bun's binary `bun.lockb`. Run `bun install --save-text-lockfile` and commit `bun.lock`; setup tells you when this applies.
- A Rust binary built without cargo-auditable carries no crate list. With `lockfileFallback: Cargo.lock` (setup adds it), Shearbolt checks that lockfile instead and warns: "binary is not auditable; rebuild with cargo-auditable".

The GitHub Action (`uses: klar-web-services/shearbolt@v1`) can be used by other repositories only while this repository is public.

## Reference

### `shearbolt.yaml`

```yaml
version: 1
project: my-app
targets:
  - name: web
    image: ${SHEARBOLT_IMAGE:-my-app:local}  # local Docker image, or a registry reference or digest
  - name: cli
    file: target/release/my-cli
    lockfileFallback: Cargo.lock             # used only if the binary has no embedded crate list
  - name: source
    source: .                                # the lockfiles in this tree
    exclude: [examples/]                     # optional: folders to leave out
```

| Field | Required | Meaning |
|---|---|---|
| `version` | yes | Always `1`. |
| `project` | yes | The project's name in reports. |
| `targets` | yes | What to scan, at least one. Each target has a `name` (letters, digits, `.`, `_` and `-`) and exactly one of the kinds below. |
| `policy` | no | See [What blocks by default](#what-blocks-by-default). |
| `exceptions` | no | See [Accepting a risk](#accepting-a-risk). |

| Target kind | Scans | When nothing is found |
|---|---|---|
| `image` | A container image: from the local Docker daemon when it is there, otherwise from the registry. | Error |
| `dir` | Everything in a directory, such as a built application bundle. | Error |
| `file` | One file: a binary or an archive. | Error, unless `lockfileFallback` applies |
| `source` | The lockfiles and manifests in a source tree, always without `node_modules/`, `.git/`, `target/` and `.shearbolt/`. `exclude` lists more folders to leave out, as paths relative to the tree (not globs). | Error when the tree has no lockfile, or Grype cannot read one |

- `${VAR}` and `${VAR:-default}` in target fields come from the environment. A `${VAR}` without a default that is unset or empty is an error. Setup's image targets use this: the CI sets `SHEARBOLT_IMAGE` to the tag it just built.
- After expansion, an `image` reference may contain only letters, digits and `._/:@-`, and must start with a letter or digit.
- Paths are relative to the config file.
- Unknown fields, duplicate target names and invalid values are errors. The message names the file and, for YAML mistakes, the line.

### CLI

| Command | What it does |
|---|---|
| `shearbolt setup` | Adds the gate to this repository's CI. Run it from the repository root. |
| `shearbolt scan` | Scans the targets in `./shearbolt.yaml`, or ad-hoc targets. |
| `shearbolt skill install [--global]` | Installs the [agent skill](#agent-skill-claude-code) for Claude Code. |
| `shearbolt version` | Prints the Shearbolt version and the pinned Grype version. |

`setup`, `scan` and `skill install` print their flags with `-h`. `scan` exits `0`, `1` or `2` as above; the other commands exit `0` on success and `2` on failure.

`shearbolt setup` flags:

| Flag | Effect |
|---|---|
| none | Asks the open questions on the terminal, shows the diffs and notes, then asks before writing. Needs a terminal. |
| `--yes` | Accepts the defaults of unanswered questions and writes without asking. |
| `--plan` | Prints the plan and writes nothing. |
| `--apply` | Writes the plan. Every open question needs an `--answer`, unless `--yes` is given. |
| `--answer id=value` | Answers a question (repeatable). `--plan --json` lists the questions and their options. |
| `--json` | With `--plan` or `--apply`: prints the plan, or the error, as JSON. |

`shearbolt scan` flags:

| Flag | Effect |
|---|---|
| `--config PATH` | The config file (default `./shearbolt.yaml`). |
| `--target NAME` | Scans only this target from the config (repeatable). |
| `--output-dir DIR` | The evidence folder (default `.shearbolt/runs/<run-id>`). It must be new or empty. |
| `--json` | Prints only `run.json` to stdout; logs go to stderr. If the config cannot be used, stdout stays empty, the message goes to stderr, and the exit code is `2`. |
| `--ci` | Turns on [CI mode](#ci-mode). |
| `--image REF`, `--dir PATH`, `--file PATH`, `--source PATH` | Ad-hoc targets (repeatable). They use the default policy and do not read `shearbolt.yaml`. |

Each Shearbolt release pins one Grype version (`shearbolt version` shows it) and downloads it, checked against its SHA-256, into the cache. A `grype` on your `PATH` is never used, and a project's `.grype.yaml` or `GRYPE_*` environment variables cannot suppress findings.

### CI mode

`scan` runs in CI mode when the `CI` environment variable is set to anything but `false` or `0`, or when `--ci` is given. GitHub Actions, Bitbucket Pipelines and GitLab CI all set `CI`, and the steps setup adds pass `--ci` as well. In CI mode a missing target is an error instead of a skip: an image that cannot be found, or a binary or directory that does not exist. A scan that did not see your artifact never passes.

When `GITHUB_STEP_SUMMARY` is set, as in GitHub Actions, `scan` appends its report to the job summary, configuration errors included.

### Evidence folder

Every scan writes an evidence folder, `.shearbolt/runs/<run-id>/` in the current directory unless you pass `--output-dir`:

| File | Contents |
|---|---|
| `run.json` | The full result: versions, the database build time, each target with its identity (image ID and digests, or file SHA-256) or why it was skipped, every finding with severity, CVSS, EPSS, KEV, fix versions, locations and the decision with its reasons, the exceptions, warnings, errors, verdict and exit code. |
| `summary.md` | The report shown [above](#what-a-blocked-build-looks-like). |
| `raw/<target>.grype.json` | Grype's unmodified output. |
| `raw/<target>.cdx.json` | A CycloneDX SBOM from the same Grype run. |

An error never hides findings: everything collected before it is still written. The GitHub Action uploads the folder as an artifact, even when the scan fails.

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
| `working-directory` | `.` | Where to run, relative to the repository root. A job's `defaults.run.working-directory` does not apply to the Action. |
| `targets` | every target | Space-separated target names, for a job that builds only some of the config's artifacts. Setup sets it only when it is needed. |
| `artifact-name` | unique per step | Name of the uploaded evidence artifact. |

| Output | Meaning |
|---|---|
| `exit-code` | `0` passed, `1` blocked by policy, `2` error or incomplete scan. |
| `artifact-name` | Name of the uploaded evidence artifact. |
| `evidence-dir` | Folder with `run.json`, `summary.md` and the raw Grype output. |

The Action installs the Shearbolt release named in its `VERSION` file with the same `install.sh`, checksum included, and checks that the binary reports exactly that version. It restores a cache of Grype and its database (a new one each day), runs `shearbolt scan --ci`, which writes the job summary, and uploads the evidence folder even on failure. The job fails on exit `1` or `2`. It runs on Linux and macOS runners.

Versions:

- `@v1` follows the newest 1.x release.
- `@v1.2.3` runs exactly that release.
- Any other ref, such as `@main` or a commit SHA, runs the checksum-verified release binary named in that commit's `VERSION` file, never unreleased code.

### Bitbucket Pipelines and GitLab CI

Setup adds two items to the Bitbucket step or GitLab job that builds, after the build and before the push. These are the lines it generates for its test fixtures (`testdata/setup/bitbucket` and `testdata/setup/gitlab`).

Bitbucket Pipelines:

```yaml
- (curl -fsSL https://tools.klar.ws/shearbolt/install.sh || wget -qO- https://tools.klar.ws/shearbolt/install.sh) | SHEARBOLT_VERSION=1.2.3 sh
- SHEARBOLT_IMAGE=$IMAGE:shearbolt-$BITBUCKET_COMMIT "$HOME/.local/bin/shearbolt" scan --ci --config "$BITBUCKET_CLONE_DIR/shearbolt.yaml"
```

GitLab CI:

```yaml
- (curl -fsSL https://tools.klar.ws/shearbolt/install.sh || wget -qO- https://tools.klar.ws/shearbolt/install.sh) | SHEARBOLT_VERSION=1.2.3 sh
- SHEARBOLT_IMAGE="$CI_REGISTRY_IMAGE:shearbolt-$CI_COMMIT_SHA" "$HOME/.local/bin/shearbolt" scan --ci --config "$CI_PROJECT_DIR/shearbolt.yaml"
```

- The install falls back to `wget`, because Alpine-based images such as `docker:` have no curl.
- `SHEARBOLT_VERSION` is the version of shearbolt that ran setup (`1.2.3` in the fixtures). Run setup again with a newer release to move the pin.
- `--config` always names the config through the clone directory: `"$BITBUCKET_CLONE_DIR/<dir>/shearbolt.yaml"` or `"$CI_PROJECT_DIR/<dir>/shearbolt.yaml"` for a project in a subfolder. An earlier `cd` cannot change which config is used.
- The `SHEARBOLT_IMAGE=` prefix passes the tag the job just built. `--target` flags appear when the job builds only some of the config's targets.
- The scan runs in the same step or job as the build, so it sees the same Docker daemon.

On Bitbucket, setup also defines a cache and lists `- shearbolt` in the step's `caches`:

```yaml
definitions:
  caches:
    shearbolt: ~/.cache/shearbolt
```

GitLab only caches paths inside the project, so the job gets its own cache folder:

```yaml
build-image:
  variables:
    SHEARBOLT_CACHE_DIR: $CI_PROJECT_DIR/.shearbolt/cache
  cache:
    - key: shearbolt
      paths:
        - .shearbolt/cache
```

### Environment variables

| Variable | Read by | Effect |
|---|---|---|
| `SHEARBOLT_CACHE_DIR` | `scan` | Where Grype and its vulnerability database live. Default: `$XDG_CACHE_HOME/shearbolt`, else `~/.cache/shearbolt`. Scans that share a cache run one at a time. |
| `XDG_CACHE_HOME` | `scan` | When `SHEARBOLT_CACHE_DIR` is not set, the cache is `$XDG_CACHE_HOME/shearbolt`. |
| `SHEARBOLT_IMAGE`, `SHEARBOLT_IMAGE_<NAME>` | setup's `shearbolt.yaml` | The image tag to scan. Setup writes `image: ${SHEARBOLT_IMAGE:-<name>:local}` for a project's first image (and `SHEARBOLT_IMAGE_<NAME>` for further images built in the same job). The CI step sets it to the tag the job just built; when it is unset, as on your machine, `<name>:local` is scanned. |
| `CI` | `scan` | Turns on [CI mode](#ci-mode). |
| `GITHUB_STEP_SUMMARY` | `scan` | When set, as in GitHub Actions, the report is appended to this file (the job summary). |
| `NO_COLOR` | `scan` | Turns off colors in the terminal report. |
| `SHEARBOLT_DB_ARCHIVE` | `scan` | For tests: imports this Grype database archive instead of updating the database. `maxDatabaseAgeHours` still applies to it, and `run.json` and `summary.md` record its use. |
| `SHEARBOLT_VERSION`, `SHEARBOLT_INSTALL_DIR`, `SHEARBOLT_BASE_URL` | `install.sh` | See [Install options](#install-options). |

### Agent skill (Claude Code)

`shearbolt skill install` writes the skill to `.claude/skills/shearbolt/` in the current directory. Commit that folder to share it with your team. `shearbolt skill install --global` writes it to `~/.claude/skills/shearbolt/` instead, for all your projects. Running the command again updates a skill that shearbolt installed, and leaves a different skill named `shearbolt` alone.

| In Claude Code | What the agent does |
|---|---|
| `/shearbolt setup` | Runs `shearbolt setup --plan --json`, asks you only the open questions, asks before applying, shows the diff and tells you what to commit. It never commits. |
| `/shearbolt fix` | Runs `shearbolt scan --json`, explains each block, proposes the upgrade (a dependency bump, a lockfile update or override, a newer base image), applies it only when you agree, and scans again to confirm. It never adds an exception or changes the policy on its own. |

The skill needs `shearbolt` on your `PATH`.

### How Shearbolt is released

A release candidate is published only after these checks pass:

1. **Gate:** the previous Shearbolt release scans the candidate binaries and the repository. The first release was gated by the pinned Grype instead.
2. **Behaviour tests:** the candidate binary must give the expected exit codes against the pinned vulnerability database: vulnerable `1`, clean `0`, broken config `2`, KEV-listed `1`, expired exception `1`, in-date exception `0`. A stand-in that always exits `0` must fail them.
3. **Install:** `install.sh` installs the candidate with curl on Linux and macOS, and with BusyBox wget on Alpine.

Then the binaries go to tools.klar.ws and GitHub Releases with a `checksums.txt`, which `install.sh` and the Action verify. `v1` moves to a release only after a live install and the Action have been checked against it. The Linux binaries are static, so they run on glibc and musl (Alpine) alike.

### Uninstall

```bash
rm ~/.local/bin/shearbolt
rm -rf ~/.cache/shearbolt
```

If you set `SHEARBOLT_INSTALL_DIR` or `SHEARBOLT_CACHE_DIR`, remove those paths instead; with `XDG_CACHE_HOME` set, the cache is `$XDG_CACHE_HOME/shearbolt`. Also delete `.claude/skills/shearbolt/` or `~/.claude/skills/shearbolt/` if you installed the skill, and the `.shearbolt/` folder in each project. To remove the gate from a project, delete its `shearbolt.yaml` and the steps setup added.
