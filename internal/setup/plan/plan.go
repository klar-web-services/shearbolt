// Package plan holds the setup plan model shared by the core and its plugins: the plan and its
// JSON contract, the CI-neutral job model, the plugin interfaces, answers, diffs, and confined
// repository access.
package plan

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

// Context is what a command sees when it runs.
type Context struct {
	// Dir is the working directory: a clean repo-relative path ("." for the root), or "" when it
	// is unknown. Consumers pass what they get through CleanDir, so an absolute or escaping
	// directory counts as unknown.
	Dir string
	Env map[string]bool // variables set for it: CI YAML env/variables, earlier export/assignments, its own VAR=x prefix
}

// CleanDir returns dir as a clean repo-relative path, or "" when it is unknown: empty, absolute,
// or outside the repository.
func CleanDir(dir string) string {
	if dir == "" || path.IsAbs(dir) {
		return ""
	}
	d := path.Clean(dir)
	if d == ".." || strings.HasPrefix(d, "../") {
		return ""
	}
	return d
}

// Ecosystem finds projects of one kind and recognizes their build commands.
type Ecosystem interface {
	Name() string // "npm", "rust"
	// Find returns this ecosystem's projects (R1), sorted by Dir, and notes for directories
	// that look like projects but cannot be protected.
	Find(r Repo) ([]Project, []Note, error)
	// Recognize classifies one command. nil: not a build of this ecosystem. Rust finds the
	// nearest Cargo.lock root above ctx.Dir (or --manifest-path) itself, and reads its binaries.
	Recognize(r Repo, cmd shell.Command, ctx Context) *Build
	// Prepare returns what a build of a selected project needs. It is called for every build
	// attributed to a selected project: gated, after a push, image builds (Rust uses those to
	// explain Dockerfile builds), and RewriteOnly builds attributed by prefix (R3).
	Prepare(r Repo, p Project, b Build) (Prep, []Note)
}

// CI parses one CI system's files into jobs and renders gates into them.
type CI interface {
	Name() string          // "github", "bitbucket", "gitlab"
	Files(r Repo) []string // existing files of this system, repo-relative, sorted
	// Jobs parses file into editable jobs. It calls classify for every shell command, in order,
	// and classifies non-shell steps itself (GitHub actions, Bitbucket pipes). Jobs it cannot
	// edit are left out and listed in one UNSUPPORTED_EDIT note per file, with the snippet.
	// Snippets in its notes are rendered for plan.Gate{}: Jobs gets no version, so the core
	// replaces them with the snippet for a gate that carries the version.
	Jobs(r Repo, file string, src []byte, classify Classifier) ([]Job, []Note, error)
	// Edit applies the gates of file and the preps of its jobs (keyed by Job.ID: the CI-file
	// Inserts and the Install of every non-RewriteOnly build attributed to a selected project).
	// Each job is edited completely or not at all: a job whose gate, inserts or install cannot be
	// applied is left unchanged, gets UNSUPPORTED_EDIT with the snippet, and is listed in Failed,
	// so the core drops the called-file Inserts its builds would need. An error means the file
	// itself cannot be edited.
	Edit(r Repo, file string, src []byte, jobs []Job, gates []Gate, preps map[string][]Prep) (Edited, error)
	// Snippet renders gate g as the text a user adds by hand (for UNSUPPORTED_EDIT and NO_GATE_PLACED).
	Snippet(r Repo, file string, g Gate) string
}

// Edited is the result of CI.Edit.
type Edited struct {
	Src    []byte   // the edited file; src itself when nothing changes
	Notes  []Note   // notes about the edits, UNSUPPORTED_EDIT for each failed job included
	Failed []string // IDs of the jobs left unchanged because their edit could not be applied
}

// Classifier classifies one shell command in its context.
type Classifier func(cmd shell.Command, ctx Context) Classified

// Classified is what one command does.
type Classified struct {
	Builds []Build
	Push   bool
	Gate   *GateCmd // existing install or scan command
}

// Project is one protectable project (R1).
type Project struct {
	Ecosystem string   `json:"ecosystem"` // "npm" | "rust"
	Dir       string   `json:"dir"`       // "." for the root; also the project's ID
	Name      string   `json:"name"`
	Lockfiles []string `json:"lockfiles"`           // relative to Dir
	Framework string   `json:"framework,omitempty"` // "nextjs"
	Binaries  []string `json:"-"`
	Excludes  []string `json:"-"` // R1 source excludes (nested projects, skipped dirs with lockfiles)
}

// Pos locates a CI command or step.
type Pos struct {
	File    string
	Job     string // Job.ID
	Step    int    // index in Job.Steps
	Command int    // index in Step.Commands; -1 = the step itself (action or pipe)
	// Line is the 1-based line in File where the command starts (for Command -1, where the step
	// starts); 0 when unknown. CI plugins set it: build-project question IDs and the locations
	// of notes about CI commands come from it.
	Line int
}

// BuildKind says what a build produces.
type BuildKind string

// Build kinds.
const (
	BuildImage  BuildKind = "image"
	BuildBinary BuildKind = "binary"
	BuildOther  BuildKind = "other" // npm build script, cross, conditional cargo, other exports, railpack without --name
)

// Build is one recognized build.
type Build struct {
	Kind        BuildKind
	Dir         string        // for attribution (R3)
	Pos         Pos           // the CI command or step that runs it (for Via builds: the calling command)
	Via         string        // file the command was found in when the CI calls it ("" = the CI file)
	Cmd         shell.Command // the recognized command; offsets relative to its script text, or to Via
	RewriteOnly bool          // in a called file but outside the reachable recipes: prefix-attributed for rewriting only, never questioned, gated or targeted
	Image       *Image
	Paths       []string // BuildBinary: binary paths relative to the project dir (nil = unknown)
	Notes       []Note
}

// Image describes an image build.
type Image struct {
	Tool         string   // "docker", "buildx", "railpack", "build-push-action"
	Tags         []string // as written: raw shell words, or the action's tag entries
	Dockerfile   string   // repo-relative, "" for railpack
	Context      string   // repo-relative
	Platforms    int
	Push         bool // --push, or build-push-action push true/expression (gateable: split)
	OutputPush   bool // pushes through -o/--output or `outputs:` (not gateable)
	Load         bool
	Computed     string // GitHub: metadata-action step id that provides the tags
	ComputedRepo string // GitHub: that step's first `images` entry, without `name=` and options (R5)
	// Byte offsets in Build.Cmd's script text where CI plugins edit a docker or buildx command
	// (0 when absent; railpack and action builds leave them 0):
	SubEnd   int      // just after the build subcommand word ("build"), where --load is added
	TagEnd   int      // just after the word holding the first tag's value, where the scan tag goes
	PushArgs [][2]int // start and end of each --push option word, which becomes --load
}

// Gateable reports whether a gate can run between the build and its push.
func (i Image) Gateable() bool { return i.Platforms <= 1 && !i.OutputPush }

// Job is one editable CI job.
type Job struct {
	ID    string // "<file>#<path>", e.g. ".github/workflows/ci.yml#test", "bitbucket-pipelines.yml#branches/main/0", ".gitlab-ci.yml#build-image"
	Label string // for prompts: "ci.yml: test"
	Steps []Step // GitHub: workflow steps. Bitbucket/GitLab: one Step per script item, so pipes keep their order
}

// Step is one workflow step or script item.
type Step struct {
	Name     string
	Dir      string          // working directory, repo-relative, before the step
	Commands []shell.Command // empty for action and pipe steps
	Builds   []Build
	Push     []int // indexes into Commands that push; -1 = the step itself pushes (action, pipe)
	Gates    []GateCmd
	Uses     string // GitHub action reference or Bitbucket pipe
}

// GateCmd is an existing Shearbolt install or scan.
type GateCmd struct {
	Project string // project dir from --config, or working-directory + dir(with.config)
	Install bool   // the install command (Bitbucket/GitLab)
	Pos     Pos
}

// Gate is one gate to insert or update.
type Gate struct {
	Project  Project
	Config   string // repo-relative path of the project's shearbolt.yaml
	Job      string
	At       Pos        // insert after this position; Command -1 = after the whole step
	Before   bool       // insert before At instead (R4 rules 3 and 4)
	Images   []ImageVar // one per image target scanned here
	Targets  []string   // nil = every target in the merged config (R4 rule 8)
	Builds   []Build    // gated builds (the plugin adds tags, --load, splits, converts)
	Existing *GateCmd   // update this gate instead of inserting one
	Version  string     // "dev" = unpinned
}

// ImageVar passes one scanned image to the gate.
type ImageVar struct {
	Var     string // SHEARBOLT_IMAGE, SHEARBOLT_IMAGE_<NAME>
	Build   Build  // which build provides the tag
	ScanTag string // R5: the local shearbolt-<commit> tag setup adds and the gate scans, in the CI's syntax
}

// Prep is what a build needs before it can be gated (Rust: the auditable rewrite and its install).
type Prep struct {
	// Pos is where the build runs (for a Via build, the calling command), set by the core: the
	// Install goes before the first build of a job that needs it.
	Pos     Pos
	Inserts []Insert
	Install *Install // only for builds that are not RewriteOnly
}

// Insert puts Text at Offset: in the script text of the command at Pos (Via == "",
// offset relative to that script) or in the repo file Via.
type Insert struct {
	Pos    Pos
	Via    string
	Offset int
	Text   string // "auditable "
}

// Install is a tool a build needs.
type Install struct {
	Name   string            // GitHub step name: "Install cargo-auditable"
	Uses   string            // GitHub: action for that step: "taiki-e/install-action@v2"
	With   map[string]string // GitHub: its inputs: {"tool": "cargo-auditable"}
	Run    string            // Bitbucket/GitLab: command item: "cargo install cargo-auditable --locked"
	Marker string            // "cargo-auditable": skip when an earlier step/command of the job contains it
}

// Plan is the result of setup: everything it found, asks and would change.
type Plan struct {
	SchemaVersion    int          `json:"schemaVersion"` // 1
	ShearboltVersion string       `json:"shearboltVersion"`
	Projects         []Project    `json:"projects"`
	CI               []CIFile     `json:"ci"`
	Questions        []Question   `json:"questions"`
	Complete         bool         `json:"complete"` // every applicable question has an --answer
	Changes          []FileChange `json:"changes"`  // computed with the answers given, defaults for the rest
	Notes            []Note       `json:"notes"`
	Error            *Note        `json:"error,omitempty"`
}

// CIFile is one CI configuration file setup found.
type CIFile struct {
	System string `json:"system"` // github | bitbucket | gitlab
	File   string `json:"file"`
}

// Question is one decision setup needs.
type Question struct {
	ID           string   `json:"id"`
	Prompt       string   `json:"prompt"`
	Kind         string   `json:"kind"` // "choice": one value; "multi": comma-separated values
	Options      []Option `json:"options"`
	Default      string   `json:"default"`                // in --answer syntax
	Answer       string   `json:"answer,omitempty"`       // as given with --answer
	Project      string   `json:"project,omitempty"`      // applies only if this project is selected
	UnlessChosen []string `json:"unlessChosen,omitempty"` // does not apply if any of these questions resolves to Project
}

// Option is one possible answer.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// FileChange is one file setup creates or modifies.
type FileChange struct {
	Path    string      `json:"path"`    // repo-relative
	Action  string      `json:"action"`  // "create" | "modify"
	Diff    string      `json:"diff"`    // unified diff, a/ and b/ prefixes, /dev/null for create
	Content string      `json:"content"` // the complete new file
	Base    []byte      `json:"-"`       // content the change was computed from; Write refuses if it changed
	Mode    fs.FileMode `json:"-"`
}

// UnpinnedNote is UNPINNED_VERSION for file: a development build rendered an install line
// without a version pin (R9).
func UnpinnedNote(file string) Note {
	return Note{
		Code:    "UNPINNED_VERSION",
		Message: "this is a development build of shearbolt, so the CI install is not pinned; run setup with a released shearbolt to pin it.",
		File:    file,
	}
}

// Note is a message about the plan, or its error.
type Note struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
}

// MarshalJSON writes every list, even an empty one, as a JSON array, so consumers can iterate
// them without null checks. It leaves HTML escaping to the caller's encoder.
func (p Plan) MarshalJSON() ([]byte, error) {
	type plain Plan
	q := plain(p)
	q.Projects = slices.Clone(q.Projects)
	for i := range q.Projects {
		q.Projects[i].Lockfiles = orEmpty(q.Projects[i].Lockfiles)
	}
	q.Questions = slices.Clone(q.Questions)
	for i := range q.Questions {
		q.Questions[i].Options = orEmpty(q.Questions[i].Options)
	}
	q.Projects, q.CI, q.Questions = orEmpty(q.Projects), orEmpty(q.CI), orEmpty(q.Questions)
	q.Changes, q.Notes = orEmpty(q.Changes), orEmpty(q.Notes)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(q); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
