# Fixing what Shearbolt blocks

## run.json

`shearbolt scan --json` prints only run.json on stdout. The same file is in the evidence
folder (`.shearbolt/runs/<run id>/run.json` by default) and in the evidence artifact a CI
run uploads. The skill reads these paths:

<!-- run.json fields -->
- `verdict`: `pass`, `block` or `error`.
- `exitCode`: 0, 1 or 2, the same as the process exit code.
- `errors[].code`: why the scan is incomplete (table below).
- `errors[].message`: what failed and what to do, written for the user.
- `targets[].name`: the target's name in `shearbolt.yaml`.
- `targets[].kind`: `image`, `dir`, `file`, `source` or `compose`.
- `targets[].ref`: the image reference, path or source tree that was scanned. For a compose target, the compose file, relative to the config's folder.
- `targets[].status`: `scanned`, `skipped` or `error`.
- `targets[].note`: why the target was skipped or failed, and how to build a skipped one. Absent when there is nothing to say.
- `targets[].images[].ref`: for a compose target, the image reference of one pulled image.
- `targets[].images[].line`: the line in the compose file where that image is set.
- `targets[].images[].services`: the compose services that use that image.
- `targets[].images[].status`: `scanned`, `skipped` or `error`, for that image alone.
- `targets[].images[].note`: why that image was skipped or failed. Absent when there is nothing to say.
- `findings[].id`: the vulnerability ID (CVE or GHSA).
- `findings[].package`: the package name.
- `findings[].version`: the installed version.
- `findings[].type`: the package type, such as `npm`, `rust-crate`, `apk`, `deb`, `rpm` or `binary`.
- `findings[].target`: the name of the target it was found in.
- `findings[].image`: for a finding in a compose target, the image reference it was found in.
- `findings[].locations`: where the package was found inside the target, such as a lockfile path or a file in the image.
- `findings[].fixState`: `fixed`, `not-fixed`, `wont-fix` or `unknown`.
- `findings[].fixVersions`: the versions that fix it, possibly one per release line.
- `findings[].decision.action`: `block`, `warn` or `excepted`.
- `findings[].decision.reasons`: why, for example "known exploited (CISA KEV)", an EPSS score above the threshold, or "fix available: upgrade to …".
<!-- /run.json fields -->

Other fields (`severity`, `epss`, `knownExploited`, `aliases`, `exceptions`, `warnings`,
the policy in effect) are there too; read them when the user asks.

### Errors (exit 2)

An incomplete scan is never a pass. Findings collected before the error are still in the
file. The message says what to do; in short:

| Code | Meaning |
| --- | --- |
| `MISSING_TARGET` | A `file` or `dir` target does not exist in CI mode. Locally this means the `CI` variable is set or `--ci` was given; otherwise the target would be skipped. |
| `ALL_SKIPPED` | Every target was skipped locally, so nothing was checked. Build an image or binary as the target notes say. |
| `MISSING_SOURCE`, `NO_LOCKFILE`, `LOCKFILE_UNREAD`, `NO_PACKAGES`, `MISSING_LOCKFILE` | A target had nothing Grype could read: a missing directory, no lockfile, an unreadable lockfile, or an empty result. |
| `STALE_DATABASE`, `SETUP` | The vulnerability database is too old, or Grype or its database could not be downloaded or verified. Usually network access. |
| `SCAN_FAILED`, `BAD_OUTPUT`, `FILE_UNREADABLE`, `SOURCE_UNREADABLE`, `EVIDENCE`, `CANCELLED` | Grype failed (in CI mode, also for an image that is neither in the local Docker daemon nor pullable), a file could not be read, the evidence folder could not be written, or the scan was interrupted. For a compose image in CI, the message may add that an image from a private registry needs a registry login before the `shearbolt` step: tell the user to add it. |
| `COMPOSE_UNREADABLE` | The compose file is missing or cannot be read, locally too. Check the target's `compose` path in `shearbolt.yaml`. |
| `COMPOSE_INVALID` | The file is not valid YAML or compose, has a bad image reference or platform, or the target's `exclude` names a service the file does not define. The message names the line. Fix the file, or the name in `exclude` if the service was renamed. No image of the target was scanned. |
| `COMPOSE_UNSUPPORTED` | The file uses `include`, a service with `extends`, or a second YAML document, which Shearbolt does not read. No image of the target was scanned. Tell the user; the change to the file is theirs to decide. |
| `COMPOSE_VARIABLE` | An image or platform variable is unset or empty, so that one service failed (the other images were still scanned). The variable must have a value where the gate runs: set it in the CI job, or add a default (`${VAR:-value}`) in the file. |
| `COMPOSE_NO_IMAGES` | The compose file pulls no image: every service builds, has no image or is excluded. If the file pulls nothing that ships, the target is wrong for it, but removing a target is the user's decision. |

Warnings do not block. `NOT_AUDITABLE` means a Rust binary was not built with
`cargo auditable`, so its crates were checked from `Cargo.lock` instead.
`EXCEPTION_EXPIRED` means an exception has expired and its finding counts again.
`COMPOSE_BUILD_NOT_SCANNED` means a compose service builds its image, so that image is not
scanned by the compose target (an `image` target for the CI build covers it).
`COMPOSE_SERVICE_NO_IMAGE` means some services define no image.

## The upgrade for a package

Group the blocking findings (`decision.action` is `block`) by `type` and `package`. A
package has one upgrade version when all of these hold:

- its `type` is `npm` or `rust-crate`;
- every blocking finding of the package has `fixState` `fixed` and at least one
  `fixVersions` entry;
- every installed version and fix version is a semantic version (`1.2.3`, optionally with
  `-pre` or `+build`);
- the highest fix version is above every installed version of the package.

The upgrade version is the highest of all `fixVersions` of its blocking findings, compared
by semantic version precedence (`4.10.0` is above `4.9.0`). It is the version in the
report's "Upgrade X to V" line. If any condition fails, there is no single upgrade: handle
each finding by the rules below.

The highest version is the safe choice, but it can cross a major version: `[2.3.4, 3.0.8]`
gives `3.0.8`. If every blocking finding of the package lists a fix on the installed
version's compatible line (same major; for 0.x, same minor), the highest of those
same-line fixes is a smaller step. Offer it as an alternative, and let the next scan
confirm it.

The same package can be installed at several versions, for example in two lockfiles. Name
each version and location.

## Where the package comes from

`findings[].target` names the target, and `targets[].kind` says what it is. In a `source`
target, `locations` holds the lockfile, such as `/apps/web/package-lock.json`, relative to
the target's tree. Its name tells you the package manager. In an `image` target,
`locations` are paths inside the image.

A package is a direct dependency when the project's manifest (`package.json` or
`Cargo.toml`) lists it. Otherwise it is transitive. Find what pulls it in with
`npm ls <pkg>`, `pnpm why <pkg>`, `yarn why <pkg>`, `bun why <pkg>`, or
`cargo tree -i <crate>@<version>`.

## Fix rules

Prefer the smallest change that clears the finding: an update within the existing ranges,
then a direct bump, then an override. An override forces a version on packages that did
not ask for it, so tell the user that, and check that the dependents still work. Show the
user each command and file change, and run it only after they agree.

### npm (`package-lock.json`, `npm-shrinkwrap.json`)

- Direct: `npm install <pkg>@<V>` (add `-D` for a devDependency). For a dependency of a
  workspace member, add `-w <workspace>`.
- Transitive: `npm update <pkg>`. If the dependents' ranges exclude `V`, upgrade the
  direct dependency that pulls it in to a release that allows `V`. Otherwise add
  `"overrides": { "<pkg>": "<V>" }` to the root `package.json` (npm ignores `overrides`
  in workspace members) and run `npm install`.

### pnpm (`pnpm-lock.yaml`)

- Direct: `pnpm add <pkg>@<V>` (add `-D` for a devDependency), in the member's directory
  for a workspace member.
- Transitive: `pnpm update <pkg>`, or `pnpm -r update <pkg>` in a workspace. Never add
  `@<V>` to a transitive update: it is meant for direct dependencies, and recent pnpm
  versions refuse it.
- If the lockfile still holds the old version, add an override, then run `pnpm install`.
  Check `pnpm --version` first:
  - pnpm 11 and later: `overrides:` with `"<pkg>": "<V>"` in the root
    `pnpm-workspace.yaml` (create the file if it is missing). These versions ignore the
    `pnpm` field of `package.json`.
  - pnpm 10: `"pnpm": { "overrides": { "<pkg>": "<V>" } }` in the root `package.json`,
    which works in every 10.x, or `overrides:` in `pnpm-workspace.yaml` if the repository
    already keeps its pnpm settings there.
  - pnpm 9 and earlier: `"pnpm": { "overrides": { "<pkg>": "<V>" } }` in the root
    `package.json`.

### Yarn (`yarn.lock`)

- Yarn 1 (no `__metadata:` entry in `yarn.lock`): direct `yarn upgrade <pkg>@<V>`;
  transitive: add `"resolutions": { "<pkg>": "<V>" }` to the root `package.json` (Yarn 1
  reads it nowhere else) and run `yarn install`.
- Yarn 2 and later (`__metadata:` present): direct `yarn up <pkg>@<V>`; transitive
  `yarn up -R <pkg>`, else `resolutions` as above.

### Bun (`bun.lock`)

- Direct: `bun update <pkg>` when the range in `package.json` allows `V`, else
  `bun add <pkg>@<V>`.
- Transitive: add `"overrides": { "<pkg>": "<V>" }` to `package.json` and run
  `bun install`.

### Cargo (`Cargo.lock`, `type` `rust-crate`)

- `cargo update -p <crate> --precise <V>`, when every dependent's version requirement
  allows `V`. If the lockfile holds several versions of the crate, name the installed
  one: `-p <crate>@<installed>`.
- If a requirement in the project's `Cargo.toml` excludes `V`, bump it there, then run
  `cargo update -p <crate>`.
- If another crate requires an old version, `cargo tree -i <crate>@<version>` shows
  which one; upgrade that crate.
- A `file` target is read from the built binary. After changing `Cargo.lock`, rebuild it
  with `cargo auditable build --release` (it needs `cargo install cargo-auditable
  --locked`) before scanning again, or the scan still reads the old binary. Without a
  local binary the target is skipped, and the source target shows the lockfile's result.

### Container images (`type` `apk`, `deb`, `rpm` or `binary`)

- Find the Dockerfile and build context the CI build uses: the context is the last
  argument of `docker build` (or the action's `context:`), and the Dockerfile comes from
  `-f` or `file:`, else `Dockerfile` in the context. In a multi-stage build, the last
  stage is what ships, so its `FROM` line is the one that matters. Name that file and
  line.
- Operating-system packages (`apk`, `deb`, `rpm`) and runtime binaries (`binary`, such as
  `node` or `python`) come from the base image. First suggest rebuilding with
  `docker build --pull --no-cache`: floating base tags such as `node:22-alpine` are
  republished with fixes, so a fresh pull may be enough, and CI's next build pulls it too
  unless it caches the base image. Otherwise propose a newer tag of the same image and
  variant, or its new digest when the `FROM` line pins one.
- If no newer base image has the fix yet, an upgrade in the last stage is the
  alternative: `RUN apk upgrade --no-cache <pkg>` on Alpine, or
  `RUN apt-get update && apt-get install -y --only-upgrade <pkg> && rm -rf /var/lib/apt/lists/*`
  on Debian and Ubuntu. Propose it as a change to the image, not as a quiet fix.
- Language packages found in an image (`npm`, `rust-crate` and others): look at
  `locations`. Under the application's own directory (for example `/app/node_modules/`)
  they come from the project's lockfile, so fix them there by the rules above. Under the
  base image's own paths (for example `/usr/local/lib/node_modules/npm/`) they come with
  the base image, so it needs a newer base image.
- Rebuild before scanning again, or the scan reads the old image or skips it. Locally an
  image target scans its `targets[].ref`, usually `<name>:local` (what
  `${SHEARBOLT_IMAGE:-<name>:local}` gives outside CI). Never run a command copied from a
  target's `note`. Check that the `ref` is a plain image reference, holding only letters,
  digits and `._/:@-`, then propose
  `docker build -f <Dockerfile> -t <ref> <context>` with the Dockerfile and context the
  CI build uses, and run it only after the user agrees. When CI builds the `Dockerfile`
  next to shearbolt.yaml with that folder as the context, `shearbolt scan --build` does
  the same build and then scans. It refuses with exit 2 when the config has more than one
  image target or the `ref` is not tagged `:local`.

### Compose images

A finding with `findings[].image` set comes from a compose target (`targets[].kind` is
`compose`). The image is pulled, not built by the project, so the operating-system and
language packages in it come with the image: the fix is a different image, never an npm or
Cargo change. The report's "Re-pin" line gives the same pin.

1. Find the pin. The image is `findings[].image`, the compose file is the target's `ref`
   (relative to the folder of the `shearbolt.yaml` you scanned with), and the line is
   `line` of the entry in `targets[].images[]` with that `ref`. Name the file and line.
   If the reference there is a variable (`${API_IMAGE}`), the pin is wherever that
   variable is set, such as a CI variable or the environment of the job that runs the gate. Say so and ask the user to
   change it there. Do not edit the compose line.
2. Propose a candidate, in this order:
   1. the newest build of the same tag:
      - a tag without a digest has no pin to change. In a local scan, run
        `docker pull <ref>` and scan again, because the local Docker may hold an old copy.
        If the finding still blocks, or the block came from CI, the newest build of the tag
        has it, so go to the next candidate;
      - a digest pin: get the tag's current digest with
        `docker buildx imagetools inspect <repo:tag>` and propose `tag@sha256:<new digest>`;
   2. the newest patch tag of the same release line (`1.30.5` to `1.30.6`).

   Moving to another release line (nginx 1.30, the stable line, to 1.31, the mainline) or
   to a new major version is proposed only with the reason, for example that no build of
   the current line has the fix, and only after asking. A digest pin stays a digest pin:
   the new reference is `tag@sha256:<new digest>`. Never turn a pin into a floating tag,
   or the reverse.
3. With the user's agreement, edit the pin, then run `shearbolt scan --target <compose
   target name>`. It scans the target exactly as the gate does, platform included. If the
   finding is not cleared, revert the edit and say so. Report the block as fixed only
   after a full scan passes (SKILL.md 4.4).
4. Never add an exception, exclude a service or remove the target on your own. If no
   candidate clears the finding, section "When there is no fix" applies.

When some blocking findings of an image have no fix (`fixState` not `fixed`), a newer
image may not clear them. Say that before the user changes anything.

## When there is no fix

`fixState` `not-fixed`, `wont-fix` or `unknown`, or an empty `fixVersions`, means no fixed
version exists. A fix can also exist but be one the user cannot take yet. The options are
the user's to choose:

- replace or remove the dependency;
- wait for a fixed release (the block stays until then);
- an exception, below.

Never change `policy` (for example `blockFixable: never`, or a higher `blockEpssAtLeast`)
to make a scan pass. It weakens the gate for every finding, and it is the team's decision,
not a fix. Never remove a target, and never edit the CI step to skip the scan.

## Exceptions

An exception accepts one vulnerability on one exact package version. It goes in the
project's `shearbolt.yaml`, in a top-level `exceptions` list:

```yaml
exceptions:
  - id: CVE-2026-12345              # the finding's id, or one of its aliases
    package: brace-expansion@2.0.2  # exact name@version from the finding
    target: web                     # optional: only in this target
    reason: Only used by build tooling, never shipped
    approvedBy: alice
    expires: 2026-12-01             # YYYY-MM-DD; valid through the end of this day, UTC
```

Rules:

- Every field is required except `target`. `target` must name a target in the config.
- `package` is `findings[].package`, then `@`, then `findings[].version`. Scoped npm
  names keep their `@`: `@babel/traverse@7.22.5`. No wildcards or ranges. It matches only
  that version, so after an upgrade the exception no longer applies.
- `expires` is the user's choice, at most `policy.maxExceptionDays` days from today, UTC
  (90 by default). Suggest the shortest date that fits. A later date makes the config
  invalid, so every scan exits 2, in CI too.
- An expired exception is ignored with an `EXCEPTION_EXPIRED` warning, and its finding
  blocks again.
- An excepted finding does not block. It stays visible in the report, with
  `decision.action` `excepted`.
- It is reviewed like code: committed with the change and reviewed in the pull request.
  Teams that want review on every policy change add `shearbolt.yaml` to CODEOWNERS.

SKILL.md section 4.5 says how to draft one with the user.
