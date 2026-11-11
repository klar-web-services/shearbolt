# Setup machine mode: the plan JSON

`shearbolt setup` computes a plan from the repository and the answers it is given, and
writes it only with `--apply`. This is the contract the skill drives, at `schemaVersion` 2.

## Commands

| Command | Writes | stdout | Exit |
| --- | --- | --- | --- |
| `shearbolt setup --plan --json [--answer 'ID=VALUE' ...]` | nothing | the plan JSON, or the error plan | 0, or 2 on error |
| `shearbolt setup --apply --json --answer 'ID=VALUE' ...` | the changes | the plan JSON after writing, or the error plan | 0, or 2 on error |

- `--answer` is repeatable, one per question.
- Usage errors are not JSON, even with `--json`: an unknown flag, a stray argument,
  `--plan` together with `--apply`, or `--json` without either. They give empty stdout, a
  message on stderr and exit 2. A plan that cannot be printed does the same; after
  `--apply`, stderr then says the changes were written. So exit 2 with empty stdout
  always means "read stderr".
- `--apply --json` prints its closing lines on stderr, so stdout stays JSON.
- Without `--plan`, `--apply` or `--yes`, setup asks its questions on a terminal and fails
  without one. `--yes` accepts every default without asking. The skill uses neither.

## Plan fields

| Field | Meaning |
| --- | --- |
| `schemaVersion` | `2`. Any other value means the contract changed: stop, and tell the user to update the skill with `shearbolt skill install`, or `shearbolt skill install --project` when it is installed in the project's `.claude/skills`. |
| `shearboltVersion` | The version that computed the plan; `dev` for a development build. |
| `projects[]` | Detected projects: `ecosystem` (`npm`, `rust` or `docker`), `dir` (`.` is the root; also the project's ID in answers), `name`, `lockfiles` (relative to `dir`), optional `compose` (the compose files setup gates, relative to `dir`), optional `framework` (`nextjs`). A `docker` project is the root project (`dir` `.`, named after the repository's folder) and has no lockfiles. It holds the compose files that no npm or Rust project contains. When the repository has no npm or Rust project, it also owns the image builds setup finds in CI (`docker build`, `docker image build`, `docker buildx build`, `docker/build-push-action`, `railpack build --name`) and gets an image target for each. It never has a `source` target. |
| `ci[]` | CI files found: `system` (`github`, `bitbucket` or `gitlab`) and `file`. |
| `questions[]` | Every question setup can ask, computed from the repository alone. See below. |
| `complete` | `true` when every applicable question has an `--answer`. |
| `changes[]` | File changes, computed with the answers given and the defaults for the rest: `path` (repository-relative), `action` (`create` or `modify`), `diff` (unified diff with `a/` and `b/` prefixes, `--- /dev/null` for a create), `content` (the whole new file). |
| `notes[]` | What the user should know: `code`, `message`, and optional `file` and `line`. |
| `error` | Present only on failure: `code`, `message`, optional `file`. |

Order is fixed: projects by `dir`, CI files by path, questions as described below, changes
by path, notes in the order setup produced them. Empty lists are `[]`.

## Questions

| Field | Meaning |
| --- | --- |
| `id` | The question ID used in `--answer`. It never contains `=`. |
| `prompt` | The question, written for the user. |
| `kind` | `choice`: one value. `multi`: one or more values, comma-separated. |
| `options[]` | `value` (what you pass) and `label` (what you show). |
| `default` | The value used when the user accepts the default, already in answer syntax. |
| `answer` | The value given with `--answer`, if any. |
| `project` | If set, the question applies only when this project is selected. |
| `unlessChosen` | Question IDs. The question does not apply if any of them resolves to `project`. |

The questions come in this order:

1. `projects` (`multi`): which detected projects to protect. The options are every project
   `dir`, and the default is all of them. It exists only when there is more than one
   project; otherwise every project is selected.
2. `build-project:<file>:<line>` (`choice`), sorted by file and line. When two such builds
   share a line, the ID ends in `:<line>:<n>`. A build in CI that setup cannot attribute
   to a project by its directory. The options are the `dir` of each npm and Rust project plus `none` (do not
   gate this build). The default is the project of an existing Shearbolt gate that follows
   the build in its job, so a re-run keeps the earlier answer; otherwise the first project.
   The line moves when setup edits the file, so always take IDs from the latest plan.
3. `gate-job:<project dir>` (`choice`, with `project` set), sorted by project. Setup found
   no build for that project, so it asks which job should check its lockfiles. A `docker`
   project gets this question only when it owns no build, that is, when it holds only
   compose files. Its prompt reads "Which job should check the compose images of <label>?"
   and the gate checks its compose images instead. The
   options are the editable job IDs (`<file>#<job>`, labels like `ci.yml: test`). The
   default is the first job, or the job that already holds a gate for that project. For a
   `docker` project the default is the first job that already holds the gate, then the
   first job that pushes or deploys, then the first job. Its
   `unlessChosen` lists every `build-project` question, because assigning an unattributed
   build to the project gives it a build and makes the question moot. A `docker`
   project's `gate-job` has no `unlessChosen`: no build can be assigned to it by a question.

## Which questions apply

A question applies when both of these hold:

1. its `project`, if set, is selected: it is in the chosen `projects` value, or there is
   no `projects` question;
2. none of the questions in its `unlessChosen` list resolves to that `project`.

A question's chosen value is the user's answer, or its default when the user accepts it.
Asked in plan order, every value a question depends on is known before it comes up. Skip
questions that do not apply and pass no answer for them. Setup checks such answers and
then ignores them, so `--apply` never asks for one.

### Example

Two npm projects, `apps/a` and `apps/b`. Job `test` runs `npm test` in `apps/a`, which is
not a build. Job `image` runs `docker build -f docker/Dockerfile -t x:${{ github.sha }}
docker`, then pushes. `docker/` is not a project, so that build is unattributed, and
neither project has a build of its own. Abridged plan:

```json
"questions": [
  {"id": "projects", "kind": "multi", "default": "apps/a,apps/b",
   "options": [{"value": "apps/a", "label": "apps/a (npm)"}, {"value": "apps/b", "label": "apps/b (npm)"}]},
  {"id": "build-project:.github/workflows/ci.yml:14", "kind": "choice", "default": "apps/a",
   "options": [{"value": "apps/a"}, {"value": "apps/b"}, {"value": "none"}]},
  {"id": "gate-job:apps/a", "kind": "choice", "default": ".github/workflows/ci.yml#test",
   "project": "apps/a", "unlessChosen": ["build-project:.github/workflows/ci.yml:14"]},
  {"id": "gate-job:apps/b", "kind": "choice", "default": ".github/workflows/ci.yml#test",
   "project": "apps/b", "unlessChosen": ["build-project:.github/workflows/ci.yml:14"]}
]
```

- The user keeps both projects and assigns the build to `apps/b`. Now `gate-job:apps/b` is
  moot, so skip it. `gate-job:apps/a` still applies, so ask it; the user accepts the
  default. The answers are:

  ```sh
  --answer 'projects=apps/a,apps/b' \
  --answer 'build-project:.github/workflows/ci.yml:14=apps/b' \
  --answer 'gate-job:apps/a=.github/workflows/ci.yml#test'
  ```

  The gate for `apps/b` goes into job `image`, after its build and before its push.
- If the user accepts the `build-project` default (`apps/a`), `gate-job:apps/a` is moot
  instead, and `gate-job:apps/b` is asked.
- If the user answers `none`, the build resolves to no project and both `gate-job`
  questions apply.
- If the user selects only `apps/a`, `gate-job:apps/b` never applies, whatever the build
  answer is.

## `complete`

`complete` is `true` when every applicable question has an `--answer`. The plan is
computed either way: its `changes` use the defaults for unanswered questions. Before
applying, re-run `--plan --json` with all your answers and continue only when `complete`
is `true`. `--apply` checks the same thing and fails with `UNANSWERED` otherwise.

## Answers

- Syntax: `--answer 'ID=VALUE'`, one per question. The first `=` separates the ID from the
  value. Quote each one, because IDs contain `:`, `/` and `#`.
- `choice`: one option `value`.
- `multi`: option values joined by commas, for example `--answer 'projects=apps/web,crates/cli'`.
  Spaces around values are trimmed, order does not matter, and duplicates are dropped.
- `BAD_ANSWER` (exit 2, with `--plan` and `--apply` alike):
  - an answer without `=`, or with an empty ID;
  - a question answered twice;
  - an unknown question ID (the message lists the question IDs);
  - a value that is not an option (the message lists the valid values);
  - an empty `multi` answer, or an empty value inside one.

  The first two are rejected before setup reads the repository, so their error plan has
  empty `questions`: keep working from the first plan's questions.
- `UNANSWERED` (exit 2, `--apply` only): applicable questions have no answer. The message
  lists their IDs. The error plan keeps `questions` and `notes`, so you can ask them.

## Errors

An error plan has `error` set and `changes: []`. It keeps `projects`, `ci` and
`questions` as far as setup computed them. Nothing was written, except possibly with
`WRITE`.

| Code | Cause | What to tell the user |
| --- | --- | --- |
| `NO_PROJECT` | One of two messages. **Nothing found:** no npm or Rust project with a lockfile Grype can read, no compose file that pulls images, and no Docker image build in the CI files (setup does not look inside scripts the CI calls). **Nothing gateable:** CI builds Docker images, but none of them can be gated, and there is no npm or Rust project or compose file. | Pass on the message. For nothing found, it names what setup protects and includes any `BUN_LOCKB`, `NO_CARGO_LOCK`, `COMPOSE_NO_IMAGES` or `COMPOSE_NOT_READ` advice. Setup supports npm, pnpm, Yarn and Bun (text `bun.lock`) projects, Cargo projects with a `Cargo.lock`, compose files that pull at least one image, and images that CI builds. For nothing gateable, the message lists each build's `MULTI_PLATFORM`, `PUSH_IN_BUILD`, `BUILD_AFTER_PUSH` or `BUILD_AFTER_GATE` note: change those builds as the notes say, then run setup again. |
| `NO_CI` | No `.github/workflows/*.yml` or `*.yaml`, `bitbucket-pipelines.yml` or `.gitlab-ci.yml`. | Shearbolt is a CI gate, so there is nothing to add it to. Pass on the message: run setup from the repository root, or add a pipeline that builds the project first; meanwhile `shearbolt scan --source .` checks the lockfiles locally. |
| `PARSE` | A CI file is not valid YAML. The message names the file and line. When setup found no npm, Rust or compose project, this comes before `NO_PROJECT` (the same holds for a CI file that cannot be read, `READ`), because the file might hold an image build. | Fix that YAML, then run setup again. |
| `READ` | The repository could not be read (an I/O error). | Show the message. |
| `BAD_ANSWER` | An answer is invalid (see Answers). | Correct the answer. |
| `UNANSWERED` | `--apply` without an answer for every applicable question. | Ask the listed questions, then apply again. |
| `WRITE` | `--apply` could not write a file. | Files are written one at a time, so earlier ones may already be written. Run `git status` and say which. |

## Notes

Each note's `message` already names the file, job or command concerned. Explain it in
the user's terms, using the meanings below. Codes marked *(Bitbucket and GitLab)* occur
only in Bitbucket Pipelines and GitLab CI files, and codes marked *(GitHub)* only in GitHub
Actions workflows.

Eight notes are informational: `COMPOSE_ADDED`, `COMPOSE_NOT_GATED`, `COMPOSE_NO_IMAGES`,
`COMPOSE_NOT_READ`, `COMPOSE_VARIABLE`, `BUILD_NOT_GATED`, `NO_DEPLOY_SEEN` and
`EARLY_STOP`. None is a reason to say Shearbolt is "not set up": setup can be complete with
them present.

### Gaps: something runs unchecked until the user acts

Every note here except the informational ones listed in the intro above counts against "set up".

- `UNSUPPORTED_EDIT`: setup will not change something automatically, such as a job or
  step layout it cannot edit safely, a `shearbolt.yaml` it cannot parse, or builds in one
  job that run under different `if:` conditions. That job or file is left as it was, and
  the message gives the reason plus the exact snippet or targets to add by hand. Show it
  in full. Until the user adds it, that job is not gated. A re-run reports the same note.
  Causes that come from early stops and split steps:
  - *(Bitbucket and GitLab)* the job can stop early before the gate, through an `exit`
    that can succeed or an `exec`: in the script before the gate, in GitLab's
    `before_script`, or in an in-repo file the job sources (a sourced file setup cannot
    follow counts too). That ends the job successfully, skips the gate, and later jobs
    still run. Setup refuses only when the gate checks more than the images built after
    that point, for example a source or compose target. Move the early stop into the job's
    `rules:` or condition, or add the gate by hand where it runs on every path. This also
    happens when setup runs again on a job that an earlier version gated. That job is
    then left as it is, so its install pin is no longer updated. Setup also refuses some
    jobs that cannot really stop early: a sourced file created at run time
    (`source venv/bin/activate`), `(cd sub && exit 0)` in a subshell, an `exit` inside a
    function, or GitLab's `inherit: default: false`, which makes every `before_script`
    count. Then add the snippet by hand.
  - *(GitHub)* a step setup splits can stop early, and its own `if:` mixes `${{ }}` into
    a bare expression. Combined with setup's check that the build ran, it would always be
    true. Write the `if:` as one expression, without `${{ }}` inside it.
  - *(GitHub)* the part of a split step before the build writes `$GITHUB_ENV` or
    `$GITHUB_PATH` (any mention of either name counts). After a split, those writes would
    change the environment of the rest of the step.
  - *(GitHub)* a command before the build of a split step may assign a variable in a way
    setup cannot copy to the rest of the step: `${NAME:=…}`, arithmetic such as
    `(( n++ ))`, `let`, `getopts` or `printf -v`.
- `NO_GATE_PLACED`: setup found no job it can edit (for example, every GitLab job uses
  `extends:` or every Bitbucket step uses `<<:`). The project's `shearbolt.yaml` is still
  written, but CI does not run Shearbolt yet. The message holds the snippet for each CI
  file: add it to the job that builds the project, after the build and before any push.
- `BUN_LOCKB`: the directory uses Bun's binary `bun.lockb`, which Grype cannot read, so it
  is not protected. Run `bun install --save-text-lockfile` there, commit `bun.lock`, and
  run setup again.
- `NO_CARGO_LOCK`: a `Cargo.toml` without `Cargo.lock`, so its dependencies cannot be
  checked and it is not protected. Run `cargo generate-lockfile` there, commit
  `Cargo.lock`, and run setup again.
- `PUSH_BEFORE_GATE`: the job pushes or deploys before the project's first build, ahead of
  the gate, so that push is never checked. The message lists the pushes. Move them after
  the build and the gate.
- `BUILD_AFTER_PUSH`: a build runs after a push or deploy in the same job, or inside a
  script or Makefile that also pushes. The gate runs before that push, so it cannot scan
  this build's output. Build before pushing, or build and push in separate steps, then
  run setup again. In a `docker` project, the message may say instead that no gate is
  placed in that job (see "Jobs a `docker` project cannot scan" below).
- `BUILD_AFTER_GATE`: a build runs after an existing Shearbolt gate in the job, so that
  gate does not scan it. Move the gate after the build, or remove it and run setup again.
- `GATE_AFTER_PUSH`: an existing Shearbolt gate runs after a push or deploy, so it cannot
  stop it. Setup adds a new gate before the push and leaves the old one alone. Remove the
  old gate once the new one is in place. In a job where a `docker` project has nothing to
  scan, setup adds no new gate, and the message asks the user to remove the old one.
- `COMMANDS_BEFORE_GATE`: commands or steps between the project's gated builds run before
  the gate, which follows the last gated build. The message lists them. If one of them
  deploys or publishes, move it after the gate.
- `PUSH_IN_BUILD`: an image build pushes straight from the build (`--output` or `outputs:`
  with `type=registry` or `push=true`), so no gate can sit between building and pushing,
  and that image is not scanned. The message says what the gate, which runs before it,
  still checks: the source lockfiles, or, in a `docker` project, its compose images or
  only the images built before it. Or it says that no gate is placed in that job. Build
  with `--load` (or `load: true`), push in a later step, and run setup again.
- `PUSH_IN_AFTER_SCRIPT` *(Bitbucket and GitLab)*: a push in GitLab's `after_script` or
  Bitbucket's `after-script`. Those run even when the script fails, so the push would run
  even when the gate blocks. Setup does not edit it. Move the push into the main script,
  after the gate.
- `INCLUDE_NOT_READ` *(Bitbucket and GitLab)*: the file pulls in pipelines setup does not
  read (GitLab `include:`, Bitbucket `import:`). Jobs defined there may build or push
  without a gate. The message holds the snippet: add it to each of those jobs that builds,
  after the build and before any push.

- `COMPOSE_NOT_GATED`: an existing `shearbolt.yaml` has no compose target for a compose
  file that pulls images. Setup leaves the config alone, and a compose target the user
  removed stays removed. The message names the target to add by hand if the file ships; then run
  `shearbolt setup` again, so every gate scans it. Also given, once per compose file, when
  `shearbolt.yaml` cannot be edited automatically (`UNSUPPORTED_EDIT`). After the user
  removes a target by hand, running setup again makes the gates stop naming it.
- `COMPOSE_NOT_READ`: a compose file setup could not read (invalid, or it uses `include`,
  `extends` or a second YAML document), so it is not gated. The message gives the reason.
  Fix the file and run setup again, or add a compose target by hand once it can be read.
- `COMPOSE_VARIABLE`: a gated compose file needs variables that have no default. The
  scan fails for those services (`COMPOSE_VARIABLE`) unless the CI job sets them. Set them
  in the job that runs the gate, or add the service to the target's `exclude` if CI
  builds that image and gates it another way. Never exclude a service for the user.
- `BUILD_NOT_GATED`: the repository has no npm or Rust project, so a binary build in CI
  (a release `cargo build`) belongs to no project and the gate does not scan what it
  produces. Image builds never get this note: the `docker` project owns them. Setup
  cannot clear this note.
- `NO_DEPLOY_SEEN`: a project of any ecosystem with compose or image targets, and setup
  saw no push or deploy step in the CI jobs it could read. CI that builds an image and
  never pushes it usually means a platform builds and deploys on its own. If the platform
  deploys by itself, for example on every push, a failed `shearbolt` check does not stop
  it. The user can require the check before merging to the deployed branch, or deploy
  from CI after the gate.

### Partial coverage: an artifact is not scanned, the rest still is

- `NO_BUILD_FOUND`: setup found no build step for the project, so the gate in the chosen
  job checks only its lockfiles. If CI does build an image or binary for it somewhere,
  that artifact is not scanned. For a `docker` project with only compose files, the gate
  checks its compose images instead of lockfiles.
- `MULTI_PLATFORM`: an image built for more than one platform cannot be loaded into the
  local Docker daemon, so its image is not scanned (multi-platform image gating is not in
  v1). The message says whether the gate runs before the build or after it, and what it
  still checks: the source lockfiles, or, in a `docker` project, its compose images or
  only the images built before it. Or it says that no gate is placed in that job.
- `RAILPACK_NO_NAME`: a `railpack build` without `--name`, so its image has no name to
  scan. Add `--name <image>:<tag>` and run setup again. Until then the gate checks the
  source lockfiles.
- `RUST_DOCKERFILE`: a `cargo build` inside a Dockerfile is not rewritten to
  `cargo auditable build`, so the binary in the image carries no crate list. The source
  target still checks `Cargo.lock`.
- `RUST_CROSS`: `cross` builds run in a container and are not rewritten, so they get no
  binary target. The source target checks `Cargo.lock`.
- `RUST_PATH_UNKNOWN`: the output path of a cargo build is not certain (the message says
  why, for example a variable target directory, `CARGO_TARGET_DIR`, or a
  `.cargo/config.toml` setting), so it gets no binary target. The source target still
  checks `Cargo.lock`.
- `RUST_CONDITIONAL_BUILD`: a cargo build inside a shell conditional or loop. It was
  rewritten to `cargo auditable build` but gets no binary target. The source target
  checks `Cargo.lock`.
- `IMAGE_REF_FIXED`: an image target in an existing `shearbolt.yaml` names a fixed image
  reference, so the gate scans that reference instead of the image the job just built.
  Change it to the `${SHEARBOLT_IMAGE:-<name>:local}` form the message shows, and run
  setup again.
- `TARGET_EXISTS`: an existing `shearbolt.yaml` already has a target with that name. With
  a different value, setup leaves the existing target alone and does not add its own:
  check with the user that the existing target still points at what CI builds, or that
  artifact may go unscanned. With a different kind, setup adds its target under a new
  name such as `<name>-2`.
- `DUPLICATE_PROJECT`: one directory holds both an npm and a Rust project (for example a
  napi-rs package). Setup protects it as the npm project, whose source target still checks
  every lockfile there, `Cargo.lock` included. Its cargo builds are not rewritten and get
  no binary target.

**Jobs a `docker` project cannot scan.** A `docker` project has no lockfiles. In a job
where none of its image builds can be scanned (multi-platform, pushing through outputs, or
after a push) and its `shearbolt.yaml` has no compose target, setup places no gate: the
gate would scan images that job never built. A compose file counts only while its target
is in `shearbolt.yaml`, so after the user removes a test compose target, a re-run can take
the gate out of such a job. `MULTI_PLATFORM`, `PUSH_IN_BUILD` and `BUILD_AFTER_PUSH` then
say that no gate is placed there. If that job already holds a Shearbolt gate, setup leaves
it alone and does not edit it, but that gate now scans images the job does not build, or
names a target `shearbolt.yaml` no longer has, and fails on every run. The build notes, or
`GATE_AFTER_PUSH` for a gate after a push, ask the user to remove that gate and run setup
again.

### Changes setup made that the user should know about

- `COMPOSE_ADDED`: setup created compose targets in a new `shearbolt.yaml`, one per
  compose file that pulls images, and lists them. Compose files that only run locally or
  in tests do not ship, so ask the user whether each listed file does. For one that does
  not, they remove its target (setup does not add it back) or, for a file that otherwise
  ships, exclude single services. After any such hand edit, `shearbolt setup` must run
  again, so every gate scans the target (or stops naming it). Removing the target of a
  test compose file can also let the gate skip on early stops (`EARLY_STOP`). A `docker`
  project that owns no image build needs at least one target: if none of its compose
  files ship, Shearbolt is the wrong tool for the repository.
- `COMPOSE_NO_IMAGES`: a compose file pulls no image (only build services, or only
  overrides), so it is not gated.

- `TAG_ADDED`: setup adds a local tag `<repository>:shearbolt-<commit>` to each gated image
  build, and the gate scans that tag. It is never pushed. This makes the gate scan exactly
  the image this job built: if the Docker daemon is unreachable, the scan fails instead of
  reading an older image from the registry.
- `ATTESTATIONS_DROPPED`: builds that pushed directly (`--push`, or `docker/build-push-action`
  with `push`) now load the image locally, and a `docker push` of the original tags runs
  after the gate. Pushing with `docker push` drops the provenance and SBOM attestations
  BuildKit attaches when it pushes itself. Tell users who rely on those attestations.
- `PUSH_IF_WRAPPED`: a push step after the gate had an `if:` using `always()`,
  `failure()` or `cancelled()`, which would run it even when the gate fails. Setup changed
  it to `${{ success() && (...) }}`, so a failed gate stops the push.
- `EARLY_STOP` *(GitHub)*: a run step that setup splits at its build can stop before the
  build, through an `exit` that can succeed (no argument, `0`, or a value that is not a
  literal number), an `exec` that replaces the shell, or an in-repo file it sources that
  does either. The note is on the line of the first such command. Setup adds a marker line
  after the build, `echo shearbolt-reached=true >> "$GITHUB_OUTPUT"`, and gives the step an
  id if it has none. The rest of the step, which moves to a new step after the gate, runs
  only when the marker is set, that is, when the build ran. The message has one of three
  forms:
  - **The gate is skipped on early stops.** It checks only images built in that step, and
    no later step in the job pushes or deploys unchecked, so when the build did not run
    there is nothing to check and nothing ships. If the user adds a target to
    `shearbolt.yaml` by hand, setup must run again, so the gate stops skipping.
  - **The gate keeps running.** On an early stop it still runs and fails when the image
    was not built, so the run turns red although nothing ships. This fails closed. The
    message says why: the gate also checks other targets (named, or every target when
    setup could not read `shearbolt.yaml`), it also follows builds in other steps, or a
    later step pushes or deploys. It then advises removing targets that do not ship and
    running setup again, or moving the early stop into the job's `if:`. Three other
    reasons are about the gate itself, each with its own advice: the gate does not
    directly follow the step, it is a `shearbolt scan` command in a run step rather than
    the Action, or its `if:` cannot be combined with the check that the build ran.
    SKILL.md section 3.7 says what to offer.
  - **A stale condition was removed.** On a re-run, an existing gate's `if:` checked a
    step's marker, but the step directly before the gate no longer has that marker line
    and id (a step was inserted between them, or the id or the line changed). Setup
    removed that check, so the gate now runs on every run. Where the `if:` shares its
    line with the step's `-`, setup writes `if: success()` instead, which means the same.
- `CACHE_NOT_ADDED` *(Bitbucket and GitLab)*: setup could not add the CI cache for
  Shearbolt's scanner and database: the GitLab job inherits a global or `default` cache, it
  already has GitLab's maximum of 4 caches, or the cache list is in flow style. The
  message has the snippet. Without the cache each run downloads them again; that is
  slower but still safe.
- `UNPINNED_VERSION` *(Bitbucket and GitLab)*: a development build of shearbolt ran setup,
  so the install line in the pipeline is not pinned to a version. Run setup with a
  released shearbolt to pin it.

## Setup never commits

Setup writes files in the working tree only. It never commits, pushes or opens a pull
request. A second run on its own output changes nothing. On a repository that is already
set up it only updates the values it manages. It never rewrites an existing
`shearbolt.yaml`: it appends missing targets and leaves `policy` and `exceptions` alone.

If setup rewrote `cargo build` to `cargo auditable build` in a Makefile, justfile or
script that the CI calls, running it locally now needs
`cargo install cargo-auditable --locked`. Tell the user.
