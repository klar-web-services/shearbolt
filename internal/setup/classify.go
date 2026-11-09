package setup

import (
	"bytes"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

// classifier implements the R2 recognizers for shell commands. CI plugins classify their own
// non-shell steps (GitHub actions, Bitbucket pipes).
type classifier struct {
	r     plan.Repo
	ecos  []plan.Ecosystem
	files map[string][]byte // called files read so far; nil for one that cannot be read
}

// newClassifier returns the Classifier for one run. Called files are read through r.
//
// The builds it returns carry no Pos: the CI plugin sets it, for builds found in called files
// too (they take the calling command's position). Their notes carry no location unless the build
// is in a called file, where File is that file and Line the build's line.
//
// For a Via build, Cmd (its Start, End, Op, ContAfter and Depth) describes the command in the
// called file, not the CI command; callers use the calling command for every split and insert
// decision. Classified.Push is false when a command's only push is its own image build
// (--push, -o type=registry): callers derive that push from Image.Push and Image.OutputPush.
func newClassifier(r plan.Repo, ecos []plan.Ecosystem) plan.Classifier {
	c := &classifier{r: r, ecos: ecos, files: map[string][]byte{}}
	return c.classify
}

// classify classifies one CI command (first match wins): an image build, an ecosystem build, an
// existing Shearbolt gate, a call to an in-repo file (followed recursively; one that cannot be
// followed counts as a push), or a push, also inside `bash -c '...'`.
func (c *classifier) classify(cmd shell.Command, ctx plan.Context) plan.Classified {
	ctx.Dir = plan.CleanDir(ctx.Dir)
	args := cmd.ArgWords()
	if b := imageBuild(cmd, args, ctx, nil); b != nil {
		return plan.Classified{Builds: []plan.Build{*b}}
	}
	if b := c.recognize(cmd, ctx); b != nil {
		return plan.Classified{Builds: []plan.Build{*b}}
	}
	if g := gateCmd(cmd, args, ctx); g != nil {
		return plan.Classified{Gate: g} // before calls: ./shearbolt scan is not a script to follow
	}
	if t, isCall := callOf(c, cmd, ctx); isCall {
		var builds []plan.Build
		push := true // a call that cannot be resolved
		if t != nil {
			builds, push = c.follow(t, cmd, ctx, 1, nil)
		}
		if push && t != nil {
			// The gate can only go before or after the whole command, and after it would follow
			// the push: these builds count as after the first push (R2, R4 rule 1).
			for i := range builds {
				b := &builds[i]
				if b.RewriteOnly {
					continue
				}
				b.Notes = append(b.Notes, plan.Note{
					Code: "BUILD_AFTER_PUSH",
					Message: fmt.Sprintf("this build runs inside %s, which also pushes or deploys, so the gate can only run before that command and checks the source lockfiles; build and push in separate CI steps to scan what is built",
						t.file),
					File: b.Via,
					Line: c.line(b.Via, b.Cmd.Start),
				})
			}
		}
		return plan.Classified{Builds: builds, Push: push}
	}
	if isPush(args, nil) || c.inlinePush(cmd, args, 0, nil, nil) {
		return plan.Classified{Push: true}
	}
	return plan.Classified{}
}

// recognize asks the ecosystems, in order, whether cmd is one of their builds.
func (c *classifier) recognize(cmd shell.Command, ctx plan.Context) *plan.Build {
	for _, e := range c.ecos {
		if b := e.Recognize(c.r, cmd, ctx); b != nil {
			return b
		}
	}
	return nil
}

// read returns a repo file's content, or nil when it cannot be read.
func (c *classifier) read(file string) []byte {
	if b, ok := c.files[file]; ok {
		return b
	}
	b, err := c.r.Read(file)
	if err != nil {
		b = nil
	}
	c.files[file] = b
	return b
}

// line returns the 1-based line of offset in file, or 0 when it cannot be read.
func (c *classifier) line(file string, offset int) int {
	src := c.read(file)
	if src == nil || offset > len(src) {
		return 0
	}
	return 1 + bytes.Count(src[:offset], []byte("\n"))
}

func variable(s string) bool { return strings.ContainsAny(s, "$`") }

func escapes(p string) bool { return p == ".." || strings.HasPrefix(p, "../") }

// relPath joins p to dir, or returns false when the result is not a certain path inside the
// repository (a variable, an absolute or home path, or one that leaves the repository).
func relPath(dir, p string) (string, bool) {
	if dir == "" || p == "" || variable(p) || path.IsAbs(p) || strings.HasPrefix(p, "~") {
		return "", false
	}
	j := path.Join(dir, p)
	if escapes(j) {
		return "", false
	}
	return j, true
}

// ---- options ----

// opt is one parsed command-line option.
type opt struct {
	name     string     // "-t", "--tag"
	value    shell.Word // its value; for --name=value and -nvalue a word holding only the value
	hasValue bool
	word     shell.Word // the option's own word
	end      int        // the offset after the last word the option consumed
}

// parseOpts splits args into options and positional words. An option in withValue takes the next
// word unless it is written --name=value; a short option whose letter is in attached may also be
// written -nvalue or -n=value. "--" ends the options. Any other option is a boolean.
func parseOpts(args []shell.Word, withValue []string, attached string) ([]opt, []shell.Word) {
	var opts []opt
	var pos []shell.Word
	for i := 0; i < len(args); i++ {
		w := args[i]
		v := w.Value
		switch {
		case v == "--":
			return opts, append(pos, args[i+1:]...)
		case strings.HasPrefix(v, "--"):
			name, val, eq := strings.Cut(v, "=")
			switch {
			case eq:
				opts = append(opts, opt{name: name, value: valueWord(w, name+"=", val), hasValue: true, word: w, end: wordEnd(w)})
			case slices.Contains(withValue, name) && i+1 < len(args):
				i++
				opts = append(opts, opt{name: name, value: args[i], hasValue: true, word: w, end: wordEnd(args[i])})
			default:
				opts = append(opts, opt{name: name, word: w, end: wordEnd(w)})
			}
		case len(v) > 1 && v[0] == '-':
			name := v[:2]
			switch {
			case len(v) == 2 && slices.Contains(withValue, name) && i+1 < len(args):
				i++
				opts = append(opts, opt{name: name, value: args[i], hasValue: true, word: w, end: wordEnd(args[i])})
			case len(v) > 2 && strings.IndexByte(attached, v[1]) >= 0 && slices.Contains(withValue, name):
				prefix := name
				if v[2] == '=' {
					prefix += "="
				}
				opts = append(opts, opt{name: name, value: valueWord(w, prefix, v[len(prefix):]), hasValue: true, word: w, end: wordEnd(w)})
			default:
				opts = append(opts, opt{name: v, word: w, end: wordEnd(w)})
			}
		default:
			pos = append(pos, w)
		}
	}
	return opts, pos
}

// wordEnd returns the offset just after word w.
func wordEnd(w shell.Word) int { return w.Start + len(w.Raw) }

// valueWord returns the part of w after prefix as a word of its own. When the raw word does not
// start with prefix (the option itself was quoted), the unquoted value stands in for it.
func valueWord(w shell.Word, prefix, value string) shell.Word {
	if raw, ok := strings.CutPrefix(w.Raw, prefix); ok {
		return shell.Word{Raw: raw, Value: value, Start: w.Start + len(prefix)}
	}
	return shell.Word{Raw: value, Value: value, Start: w.Start}
}

// positionals returns the values of args that are not options or option values.
func positionals(args []shell.Word, withValue []string) []string {
	_, pos := parseOpts(args, withValue, "")
	out := make([]string, len(pos))
	for i, w := range pos {
		out[i] = w.Value
	}
	return out
}

// ---- image builds ----

var (
	// dockerGlobalOpts are docker's options before the subcommand that take a value.
	dockerGlobalOpts = []string{"--config", "-c", "--context", "-H", "--host", "-l", "--log-level", "--tlscacert", "--tlscert", "--tlskey"}
	dockerBuildOpts  = []string{
		"-t", "--tag", "-f", "--file", "--platform", "--build-arg", "--build-context", "--target", "--cache-from",
		"--cache-to", "-o", "--output", "--label", "--secret", "--ssh", "--network", "--progress", "--iidfile",
		"--metadata-file", "--builder", "--add-host", "--shm-size", "--ulimit", "--attest", "--allow",
		"--annotation", "--cgroup-parent", "--call",
	}
	railpackOpts = []string{"--name", "--platform", "--progress", "--cache-key", "--cache-from", "--cache-to", "--output", "--env"}
)

// imageBuild recognizes `docker build`, `docker image build`, `docker buildx build` and
// `railpack build` (R2). A build that only exports files is a BuildOther gate location.
func imageBuild(cmd shell.Command, args []shell.Word, ctx plan.Context, vars map[string]string) *plan.Build {
	if len(args) < 2 {
		return nil
	}
	args = expandProgram(args, vars)
	switch path.Base(args[0].Value) {
	case "docker":
		rest, tool, ok := dockerBuildArgs(args[1:])
		if ok {
			b := dockerBuild(cmd, rest, tool, ctx)
			if b.Image != nil && vars == nil {
				// The build subcommand is the word before rest. In a called file (vars set) the
				// program may come from a variable, so offsets are left unset there.
				b.Image.SubEnd = wordEnd(args[len(args)-len(rest)-1])
			}
			return b
		}
	case "railpack":
		if args[1].Value == "build" {
			return railpackBuild(cmd, args[2:], ctx)
		}
	}
	return nil
}

// dockerBuildArgs returns the arguments after a docker build subcommand, and the tool: "docker"
// for build and image build, "buildx" for buildx build.
func dockerBuildArgs(args []shell.Word) ([]shell.Word, string, bool) {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i].Value, "-") {
		name, _, eq := strings.Cut(args[i].Value, "=")
		if !eq && slices.Contains(dockerGlobalOpts, name) {
			i++
		}
		i++
	}
	at := func(k int) string {
		if i+k < len(args) {
			return args[i+k].Value
		}
		return ""
	}
	switch {
	case at(0) == "build":
		return args[i+1:], "docker", true
	case at(0) == "image" && at(1) == "build":
		return args[i+2:], "docker", true
	case at(0) == "buildx" && at(1) == "build":
		return args[i+2:], "buildx", true
	case at(0) == "buildx" && at(1) == "--builder" && at(3) == "build":
		return args[i+4:], "buildx", true
	}
	return nil, "", false
}

func dockerBuild(cmd shell.Command, args []shell.Word, tool string, ctx plan.Context) *plan.Build {
	opts, pos := parseOpts(args, dockerBuildOpts, "tfo")
	img := &plan.Image{Tool: tool}
	var file *string
	var outputs []string
	var pushArgs [][2]int
	check := false
	for _, o := range opts {
		switch o.name {
		case "--check":
			check = true
		case "--call":
			// --call check, outline or targets evaluates the build without building an image.
			f, _, _ := strings.Cut(o.value.Value, ",")
			check = check || strings.TrimSpace(f) != "build"
		case "-t", "--tag":
			if len(img.Tags) == 0 {
				img.TagEnd = o.end
			}
			img.Tags = append(img.Tags, o.value.Raw)
		case "-f", "--file":
			f := o.value.Value
			file = &f
		case "--platform":
			img.Platforms += platforms(o.value.Value)
		case "--push":
			img.Push = !o.hasValue || truthy(o.value.Value, true)
			pushArgs = append(pushArgs, [2]int{o.word.Start, o.end})
		case "--load":
			img.Load = !o.hasValue || truthy(o.value.Value, false)
		case "-o", "--output":
			outputs = append(outputs, o.value.Value)
		}
	}
	if img.Push {
		img.PushArgs = pushArgs // every --push word becomes --load when the push moves after the gate
	}
	exportOnly := len(outputs) > 0 && !img.Push
	for _, o := range outputs {
		switch outputKind(o) {
		case "push":
			img.OutputPush, exportOnly = true, false
		case "load":
			img.Load, exportOnly = true, false
		}
	}
	if len(pos) > 0 {
		img.Context = contextDir(ctx.Dir, pos[len(pos)-1].Value)
	}
	dir := img.Context
	switch {
	case file != nil:
		if *file != "-" {
			img.Dockerfile, _ = relPath(ctx.Dir, *file)
		}
		dir = ""
		if img.Dockerfile != "" {
			dir = path.Dir(img.Dockerfile)
		}
	case img.Context != "":
		img.Dockerfile = path.Join(img.Context, "Dockerfile")
	}
	if exportOnly || check {
		return &plan.Build{Kind: plan.BuildOther, Dir: dir, Cmd: cmd}
	}
	return &plan.Build{Kind: plan.BuildImage, Dir: dir, Cmd: cmd, Image: img}
}

// platforms counts a --platform list. A variable counts as two: the list is unknown, and a
// multi-platform build cannot be gated.
func platforms(v string) int {
	if variable(v) {
		return 2
	}
	n := 0
	for _, p := range strings.Split(v, ",") {
		if strings.TrimSpace(p) != "" {
			n++
		}
	}
	return n
}

// outputKind classifies a buildx output: "push" (type=registry, or type=image with push=true),
// "load" (type=docker into the daemon), or "export" (files, a tarball, an unpushed image, the
// cache). An output that cannot be read, such as a variable, counts as a push, so the gate goes
// before it.
func outputKind(v string) string {
	if variable(v) {
		return "push"
	}
	if !strings.Contains(v, "=") {
		return "export" // -o <dir> or -o - (a tarball on stdout)
	}
	kv := map[string]string{}
	for _, part := range strings.Split(v, ",") {
		k, val, _ := strings.Cut(part, "=")
		kv[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(val), `"'`)
	}
	switch kv["type"] {
	case "registry":
		return "push"
	case "image":
		if p, ok := kv["push"]; ok && truthy(p, true) {
			return "push"
		}
		return "export"
	case "docker":
		if kv["dest"] != "" {
			return "export"
		}
		return "load"
	case "", "local", "tar", "oci", "cacheonly":
		return "export"
	}
	return "push"
}

// truthy parses a boolean option value; a value that does not parse gives onError.
func truthy(v string, onError bool) bool {
	b, err := strconv.ParseBool(v)
	if err != nil {
		return onError
	}
	return b
}

// contextDir returns a local build context relative to the repository, or "" when it is not a
// certain local directory (a URL, stdin, a variable, an absolute path).
func contextDir(cur, v string) string {
	if v == "-" || strings.Contains(v, "://") || strings.HasPrefix(v, "git@") {
		return ""
	}
	d, _ := relPath(cur, v)
	return d
}

func railpackBuild(cmd shell.Command, args []shell.Word, ctx plan.Context) *plan.Build {
	opts, pos := parseOpts(args, railpackOpts, "")
	dir := "."
	if len(pos) > 0 {
		dir = pos[len(pos)-1].Value
	}
	b := &plan.Build{Dir: contextDir(ctx.Dir, dir), Cmd: cmd}
	var name *shell.Word
	n, output := 0, false
	for _, o := range opts {
		switch o.name {
		case "--name":
			name = &o.value
		case "--platform":
			n += platforms(o.value.Value)
		case "--output":
			output = true
		}
	}
	switch {
	case output:
		b.Kind = plan.BuildOther // writes a filesystem, not an image
	case name == nil:
		b.Kind = plan.BuildOther
		b.Notes = []plan.Note{{
			Code:    "RAILPACK_NO_NAME",
			Message: "this railpack build has no --name, so its image cannot be scanned; add `--name <image>:<tag>` and run setup again. Until then the gate checks the source lockfiles.",
		}}
	default:
		b.Kind = plan.BuildImage
		b.Image = &plan.Image{Tool: "railpack", Tags: []string{name.Raw}, Context: b.Dir, Platforms: n}
	}
	return b
}

// ---- pushes ----

// pushRule is how one tool pushes or deploys: the subcommand paths that do, and the tool's
// options before them that take a value.
type pushRule struct {
	opts []string
	subs [][]string
}

// composeOpts are compose's options before the subcommand that take a value. They are also
// docker's options for every subcommand (P3): that only makes positionals more accurate.
var composeOpts = []string{"-f", "--file", "-p", "--project-name", "--profile", "--env-file", "--project-directory", "--ansi", "--progress", "--parallel"}

var pushRules = map[string]pushRule{
	"docker": {append(slices.Clone(dockerGlobalOpts), composeOpts...),
		[][]string{{"push"}, {"image", "push"}, {"manifest", "push"}, {"buildx", "imagetools", "create"}, {"compose", "push"}, {"stack", "deploy"}}},
	"docker-compose": {composeOpts, [][]string{{"push"}}},
	"podman":         {nil, [][]string{{"push"}, {"manifest", "push"}}},
	"buildah":        {nil, [][]string{{"push"}, {"manifest", "push"}}},
	"crane":          {nil, [][]string{{"push"}, {"copy"}, {"cp"}}},
	"skopeo":         {nil, [][]string{{"copy"}}},
	"npm":            {[]string{"--prefix", "-w", "--workspace", "--loglevel"}, [][]string{{"publish"}}},
	"pnpm":           {[]string{"-C", "--dir", "--filter", "-F", "--loglevel"}, [][]string{{"publish"}}},
	"bun":            {[]string{"--cwd"}, [][]string{{"publish"}}},
	"yarn":           {[]string{"--cwd"}, [][]string{{"publish"}, {"npm", "publish"}}},
	"cargo":          {[]string{"--color", "--config", "-Z", "-C"}, [][]string{{"publish"}}},
	"gh":             {[]string{"-R", "--repo"}, [][]string{{"release", "create"}, {"release", "upload"}}},
	"fly":            {[]string{"-a", "--app", "-c", "--config", "-t", "--access-token"}, [][]string{{"deploy"}}},
	"vercel":         {[]string{"-t", "--token", "-S", "--scope", "--cwd", "-A", "--local-config", "-Q", "--global-config"}, [][]string{{"deploy"}}},
	"railway":        {[]string{"-s", "--service", "-e", "--environment"}, [][]string{{"up"}}},
	"kubectl": {[]string{"-n", "--namespace", "--context", "--kubeconfig", "-s", "--server", "--cluster", "--user", "--token", "--as", "--as-group", "--request-timeout"},
		[][]string{{"apply"}, {"set"}, {"rollout"}, {"create"}}},
	"helm": {[]string{"-n", "--namespace", "--kube-context", "--kubeconfig", "--registry-config", "--repository-cache", "--repository-config", "--kube-apiserver", "--kube-token", "--kube-as-user"},
		[][]string{{"upgrade"}, {"install"}}},
}

// isPush reports whether the command pushes or deploys (the fixed list of R2, including compose
// pushes and stack deploys). vars resolves a program written as a Makefile variable.
func isPush(args []shell.Word, vars map[string]string) bool {
	if len(args) == 0 {
		return false
	}
	args = expandProgram(args, vars)
	prog := path.Base(args[0].Value)
	if prog == "flyctl" {
		prog = "fly"
	}
	rule, ok := pushRules[prog]
	if !ok {
		return false
	}
	if prog == "vercel" && slices.ContainsFunc(args[1:], func(w shell.Word) bool { return w.Value == "--prod" || strings.HasPrefix(w.Value, "--prod=") }) {
		return true
	}
	pos := positionals(args[1:], rule.opts)
	if prog == "cargo" && len(pos) > 0 && strings.HasPrefix(pos[0], "+") {
		pos = pos[1:]
	}
	for _, sub := range rule.subs {
		if len(pos) >= len(sub) && slices.Equal(pos[:len(sub)], sub) {
			return true
		}
	}
	return false
}

// varProgram matches a program written as one variable: $(NAME), ${NAME} or $NAME.
var varProgram = regexp.MustCompile(`^\$(?:\(([A-Za-z_][A-Za-z0-9_]*)\)|\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))$`)

// knownTool reports a program name the image and push rules know.
func knownTool(p string) bool {
	_, ok := pushRules[p]
	return ok || p == "railpack" || p == "flyctl"
}

// expandProgram returns args with a program written as a variable ($(NAME), ${NAME}, $NAME)
// replaced: by the words of its Makefile value when that runs a known tool, with wrappers and
// options handled as in Argv (`KUBECTL := kubectl -n prod`, `DOCKER := sudo docker`), otherwise
// by the lower-cased name when that is a known tool ($(DOCKER) is docker). Only the Values of
// substituted words are meaningful; their offsets point into the variable's value.
func expandProgram(args []shell.Word, vars map[string]string) []shell.Word {
	if len(args) == 0 {
		return args
	}
	m := varProgram.FindStringSubmatch(args[0].Value)
	if m == nil {
		return args
	}
	name := m[1] + m[2] + m[3]
	if v, ok := vars[name]; ok {
		if cmds, _ := shell.Parse(v, shell.State{}); len(cmds) > 0 {
			if a := cmds[0].ArgWords(); len(a) > 0 && knownTool(path.Base(a[0].Value)) {
				return append(slices.Clone(a), args[1:]...)
			}
		}
	}
	if l := strings.ToLower(name); knownTool(l) {
		w := args[0]
		w.Value = l
		return append([]shell.Word{w}, args[1:]...)
	}
	return args
}

// shells are the programs whose -c script and script files setup reads.
var shells = []string{"bash", "sh", "zsh", "dash", "ksh"}

// inlineScript returns the script of `bash|sh|zsh -c '<script>'`.
func inlineScript(args []shell.Word) (string, bool) {
	if len(args) < 2 || !slices.Contains(shells, path.Base(args[0].Value)) {
		return "", false
	}
	c := false
	for i := 1; i < len(args); i++ {
		v := args[i].Value
		switch {
		case v == "--":
			if c && i+1 < len(args) {
				return args[i+1].Value, true
			}
			return "", false
		case v == "--rcfile" || v == "--init-file":
			i++
		case strings.HasPrefix(v, "--"):
		case len(v) > 1 && (v[0] == '-' || v[0] == '+'):
			c = c || strings.Contains(v[1:], "c")
			if strings.HasSuffix(v, "o") || strings.HasSuffix(v, "O") {
				i++
			}
		default:
			return v, c
		}
	}
	return "", false
}

// inlinePush reports whether a `bash|sh|zsh -c '<script>'` pushes. Its script is parsed in the
// command's state, and each command is checked like one in a called file at the same level
// (pushes, pushing image builds, file calls, nested -c), with the file's Makefile variables.
// Builds inside are not recognized.
func (c *classifier) inlinePush(cmd shell.Command, args []shell.Word, level int, stack []string, vars map[string]string) bool {
	script, ok := inlineScript(args)
	if !ok {
		return false
	}
	start := shell.State{Dir: plan.CleanDir(cmd.Before.Dir), Env: cmd.Before.Env, Self: cmd.Before.Self}
	cmds, _ := shell.Parse(script, start)
	for _, ic := range cmds {
		ic.Depth += cmd.Depth
		if _, push := c.nested(ic, contextOf(ic), "", level, stack, vars); push {
			return true
		}
	}
	return false
}

// contextOf is the context of a command found in a called file or an inline script.
func contextOf(cmd shell.Command) plan.Context {
	ctx := plan.Context{Dir: cmd.Before.Dir, Env: map[string]bool{}}
	for _, n := range cmd.Before.Env {
		ctx.Env[n] = true
	}
	for _, n := range cmd.Prefix() {
		ctx.Env[n] = true
	}
	return ctx
}

// ---- existing gates ----

const installURL = "tools.klar.ws/shearbolt/install.sh"

// cloneDirs are the CI variables that hold the repository's checkout directory.
var cloneDirs = []string{"$CI_PROJECT_DIR", "${CI_PROJECT_DIR}", "$BITBUCKET_CLONE_DIR", "${BITBUCKET_CLONE_DIR}", "$GITHUB_WORKSPACE", "${GITHUB_WORKSPACE}", "${{ github.workspace }}"}

// gateCmd recognizes an existing gate: the install command (any command containing the install
// URL) or `<...>shearbolt scan`, whose project is the directory of --config (default: the
// command's directory, where ./shearbolt.yaml is read). An uncertain project is "".
func gateCmd(cmd shell.Command, args []shell.Word, ctx plan.Context) *plan.GateCmd {
	for _, w := range cmd.Words {
		if strings.Contains(w.Raw, installURL) || strings.Contains(w.Value, installURL) {
			return &plan.GateCmd{Install: true}
		}
	}
	if len(args) < 2 || !strings.HasSuffix(args[0].Value, "shearbolt") || args[1].Value != "scan" {
		return nil
	}
	cfg, found := "", false
	rest := args[2:]
	for i := 0; i < len(rest); i++ {
		name, val, eq := strings.Cut(rest[i].Value, "=")
		if name != "--config" && name != "-config" {
			continue
		}
		found = true
		if !eq && i+1 < len(rest) {
			i++
			val = rest[i].Value
		}
		cfg = val
	}
	if !found {
		return &plan.GateCmd{Project: ctx.Dir}
	}
	base := ctx.Dir
	for _, v := range cloneDirs {
		if r, ok := strings.CutPrefix(cfg, v); ok && strings.HasPrefix(r, "/") {
			cfg, base = strings.TrimPrefix(r, "/"), "."
			break
		}
	}
	p, ok := relPath(base, cfg)
	if !ok {
		return &plan.GateCmd{}
	}
	return &plan.GateCmd{Project: path.Dir(p)}
}
