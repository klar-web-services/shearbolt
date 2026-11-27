<picture>
  <source media="(prefers-color-scheme: dark)" srcset="assets/shearbolt_logo_icon_dark_mode.svg">
  <img src="assets/shearbolt_logo_icon_bright_mode.svg" alt="Shearbolt logo" height="72">
</picture>

# Shearbolt

Shearbolt stops a deployment when what you are about to ship contains a vulnerability that is exploited in the wild, likely to be exploited, or already fixed upstream.

## Protect a project

```bash
curl -fsSL https://tools.klar.ws/shearbolt/install.sh | sh
```

```bash
cd your-project && shearbolt setup
```

Setup adds a scan to your CI (GitHub Actions, Bitbucket Pipelines, GitLab CI), after the build and before any push or deploy. It shows every change as a diff and asks before it writes. Commit the result, and your next pipeline is gated.

- `shearbolt: command not found`? Add `~/.local/bin` to your `PATH`.
- Added a workflow or a build later? Run setup again.
- Nobody pushes for weeks? Say yes to the daily run: setup adds a scheduled pipeline that builds and scans once a day and deploys nothing (GitHub Actions, Bitbucket Pipelines).
- With Claude Code or Codex: `shearbolt skill install`, then `/shearbolt setup` (in Codex `$shearbolt setup`).
- `shearbolt update` installs the latest release.

Shearbolt runs on Linux and macOS (amd64 and arm64).

## Zosia (optional)

[Zosia](https://zosiasec.com) collects the reports from all your repositories and notifies you when one goes red. Enable it with:

```bash
shearbolt zosia enable
```

Follow the on-screen instructions. To stop sending to Zosia:

```bash
shearbolt zosia disable
```

## What a blocked build looks like

The report is the job summary in GitHub Actions, and `summary.md` in `.shearbolt/runs/<run-id>/` everywhere.

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

## What blocks

| A finding that is | Default | Policy key |
|---|---|---|
| On CISA's [Known Exploited Vulnerabilities](https://www.cisa.gov/known-exploited-vulnerabilities-catalog) list | Blocks, fix or not | `blockKnownExploited: true` |
| Likely to be exploited: an [EPSS](https://www.first.org/epss/) score of 10% or more | Blocks, fix or not | `blockEpssAtLeast: 0.10` |
| Fixed in a newer version | Blocks | `blockFixable: any` (or `low`, `medium`, `high`, `critical`, `never`) |
| Anything else | Warning | |

Change the policy in `shearbolt.yaml`, or in Zosia for a repository that is connected to it:

```yaml
policy:
  blockFixable: high
```

To accept a risk, add an exception:

```yaml
exceptions:
  - id: CVE-2026-12345
    package: brace-expansion@2.0.2
    reason: Only used by build tooling, never shipped
    approvedBy: alice
    expires: 2027-01-31   # at most 90 days ahead
```

## Run it locally

```bash
shearbolt scan                         # the targets in ./shearbolt.yaml
shearbolt scan --image node:22-alpine  # ad hoc (also --source, --file, --dir)
```

Exit codes: `0` passed, `1` blocked, `2` error. `shearbolt -h` lists every command.

## Supported

### Projects

| | |
|---|---|
| npm | npm, pnpm, Yarn and Bun lockfiles |
| Rust | Cargo projects; binaries built with cargo-auditable |
| Go | Go modules; binaries built with `go build -o <file>` |
| Docker | Images your CI builds from a Dockerfile, and images your Docker Compose files pull |

### CI providers

- GitHub Actions
- Bitbucket Pipelines
- GitLab CI

## Writing the gate by hand

Setup writes all of this for you. `shearbolt.yaml` lists what to scan:

```yaml
version: 1
project: my-app
targets:
  - name: web
    image: ${SHEARBOLT_IMAGE:-my-app:local}
  - name: cli
    file: target/release/my-cli
  - name: stack
    compose: docker-compose.yml
  - name: source
    source: .               # the lockfiles in this tree
    exclude: [examples/]
```

GitHub Actions, after the build:

```yaml
- name: shearbolt
  uses: klar-web-services/shearbolt@v1
  with:
    working-directory: apps/web                  # optional
    zosia-secret: ${{ secrets.ZOSIA_SECRET }}    # optional
```

Bitbucket Pipelines and GitLab CI, between the build and the push:

```yaml
- (curl -fsSL https://tools.klar.ws/shearbolt/install.sh || wget -qO- https://tools.klar.ws/shearbolt/install.sh) | SHEARBOLT_VERSION=1.13.0 sh
- SHEARBOLT_IMAGE="$IMAGE" "$HOME/.local/bin/shearbolt" scan --ci
```

## Uninstall

```bash
rm ~/.local/bin/shearbolt
rm -rf ~/.cache/shearbolt
```

## License

MIT, see [LICENSE](LICENSE).
