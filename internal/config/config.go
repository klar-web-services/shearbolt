// Package config loads and validates shearbolt.yaml.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Config is a validated shearbolt.yaml. Target paths are absolute after loading.
type Config struct {
	Version    WholeNumber `yaml:"version"`
	Project    string      `yaml:"project"`
	Targets    []Target    `yaml:"targets"`
	Policy     Policy      `yaml:"policy"`
	Exceptions []Exception `yaml:"exceptions"`
	// Dir is the directory relative paths were resolved against.
	Dir string `yaml:"-"`
}

// Target is one thing to scan. Exactly one of Image, Dir, File, Source or Compose is set.
type Target struct {
	Name             string   `yaml:"name" json:"name"`
	Image            string   `yaml:"image,omitempty" json:"image,omitempty"`
	Dir              string   `yaml:"dir,omitempty" json:"dir,omitempty"`
	File             string   `yaml:"file,omitempty" json:"file,omitempty"`
	Source           string   `yaml:"source,omitempty" json:"source,omitempty"`
	Compose          string   `yaml:"compose,omitempty" json:"compose,omitempty"`
	Exclude          []string `yaml:"exclude,omitempty" json:"exclude,omitempty"`
	LockfileFallback string   `yaml:"lockfileFallback,omitempty" json:"lockfileFallback,omitempty"`
}

// Policy decides which findings block (spec section 6).
type Policy struct {
	BlockKnownExploited bool    `yaml:"blockKnownExploited" json:"blockKnownExploited"`
	BlockEpssAtLeast    float64 `yaml:"blockEpssAtLeast" json:"blockEpssAtLeast"`
	BlockFixable        string  `yaml:"blockFixable" json:"blockFixable"`
	MaxDatabaseAgeHours int     `yaml:"maxDatabaseAgeHours" json:"maxDatabaseAgeHours"`
	MaxExceptionDays    int     `yaml:"maxExceptionDays" json:"maxExceptionDays"`
}

// Exception accepts one vulnerability on one exact package version until a date.
type Exception struct {
	ID         string `yaml:"id" json:"id"`
	Package    string `yaml:"package" json:"package"`
	Target     string `yaml:"target,omitempty" json:"target,omitempty"`
	Reason     string `yaml:"reason" json:"reason"`
	ApprovedBy string `yaml:"approvedBy" json:"approvedBy"`
	Expires    string `yaml:"expires" json:"expires"`
}

// FixableLevels are the accepted values of policy.blockFixable.
var FixableLevels = []string{"any", "low", "medium", "high", "critical", "never"}

// DefaultPolicy returns the built-in policy.
func DefaultPolicy() Policy {
	return Policy{BlockKnownExploited: true, BlockEpssAtLeast: 0.10, BlockFixable: "any", MaxDatabaseAgeHours: 48, MaxExceptionDays: 90}
}

// docsURL is where users learn the shearbolt.yaml format.
const docsURL = "https://github.com/klar-web-services/shearbolt#readme"

// FileError is a config file that is not valid YAML, does not fit the shearbolt.yaml format, or
// fails validation. Problems say what is wrong in the user's terms (with the line where YAML
// reports one), never in Go types.
type FileError struct {
	Path     string
	Problems []string
}

// Unwrap exposes the problems, so errors.As(err, &Problems{}) keeps working for callers.
func (e *FileError) Unwrap() error { return Problems(e.Problems) }

func (e *FileError) Error() string {
	return fmt.Sprintf("invalid configuration in %s:\n  - %s\nSee %s for the format of shearbolt.yaml.", e.Path, strings.Join(e.Problems, "\n  - "), docsURL)
}

var (
	yamlUnknownField = regexp.MustCompile(`^line (\d+): field (\S+) not found in type \S+$`)
	yamlWrongType    = regexp.MustCompile("^line (\\d+): cannot unmarshal !!(\\w+)(?: `([^`]*)`)? into (\\S+)$")
	yamlSyntax       = regexp.MustCompile(`^yaml: line (\d+): (.+)$`)
	yamlGoType       = regexp.MustCompile(` (?:into|in type) \S+`)
)

// yamlProblems translates a YAML decoding error into FileError problems.
func yamlProblems(err error) []string {
	var te *yaml.TypeError
	if errors.As(err, &te) {
		problems := make([]string, 0, len(te.Errors))
		for _, e := range te.Errors {
			problems = append(problems, yamlProblem(e))
		}
		return problems
	}
	msg := err.Error()
	if m := yamlSyntax.FindStringSubmatch(msg); m != nil {
		return []string{fmt.Sprintf("line %s: not valid YAML (%s); check the indentation and quotes", m[1], m[2])}
	}
	return []string{fmt.Sprintf("not valid YAML (%s); check the indentation and quotes", strings.TrimPrefix(msg, "yaml: "))}
}

func yamlProblem(e string) string {
	if m := yamlUnknownField.FindStringSubmatch(e); m != nil {
		return fmt.Sprintf("line %s: unknown field %q (check its spelling and indentation)", m[1], m[2])
	}
	if m := yamlWrongType.FindStringSubmatch(e); m != nil {
		return fmt.Sprintf("line %s: expected %s, found %s", m[1], expectedKind(m[4]), foundKind(m[2], m[3]))
	}
	return yamlGoType.ReplaceAllString(e, "") // other messages, such as duplicate keys, without Go types
}

// expectedKind names what a Go type accepts in YAML terms.
func expectedKind(goType string) string {
	switch {
	case strings.HasPrefix(goType, "[]"):
		return "a list"
	case strings.HasPrefix(goType, "map["), strings.Contains(goType, "."):
		return "a mapping" // a struct such as the config itself, a target or the policy
	case goType == "bool":
		return "true or false"
	case goType == "string":
		return "a text value"
	case strings.HasPrefix(goType, "int"), strings.HasPrefix(goType, "uint"):
		return "a whole number"
	case strings.HasPrefix(goType, "float"):
		return "a number"
	}
	return "a different kind of value"
}

// foundKind names what the YAML held.
func foundKind(tag, value string) string {
	switch {
	case tag == "seq":
		return "a list"
	case tag == "map":
		return "a mapping"
	case tag == "null" || value == "":
		return "an empty value"
	}
	return strconv.Quote(value)
}

// WholeNumber is an int that the YAML must give as a whole number: yaml.v3 alone would truncate
// version: 1.5 to 1 and accept it.
type WholeNumber int

// UnmarshalYAML accepts only YAML integers.
func (n *WholeNumber) UnmarshalYAML(node *yaml.Node) error {
	var i int
	if node.Kind == yaml.ScalarNode && node.ShortTag() == "!!int" && node.Decode(&i) == nil {
		*n = WholeNumber(i)
		return nil
	}
	found := "a list"
	switch {
	case node.Kind == yaml.MappingNode:
		found = "a mapping"
	case node.Kind == yaml.ScalarNode:
		found = strconv.Quote(node.Value)
	}
	return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: version must be a whole number, found %s", node.Line, found)}}
}

// wholeHours and wholeDays check that policy.maxDatabaseAgeHours and policy.maxExceptionDays are
// whole numbers: yaml.v3 alone would truncate 47.9 to 47 into Policy's int fields and accept it.
type (
	wholeHours struct{}
	wholeDays  struct{}
)

func (wholeHours) UnmarshalYAML(node *yaml.Node) error {
	return wholeNumberField(node, "policy.maxDatabaseAgeHours")
}

func (wholeDays) UnmarshalYAML(node *yaml.Node) error {
	return wholeNumberField(node, "policy.maxExceptionDays")
}

// wholeNumberField fails a YAML number that is not an integer (47.9, 48.0, 1e2, .inf). Other
// kinds of values are left to the main decode, which reports them.
func wholeNumberField(node *yaml.Node, name string) error {
	if node.Kind == yaml.ScalarNode && node.ShortTag() == "!!float" {
		return &yaml.TypeError{Errors: []string{fmt.Sprintf("line %d: %s must be a whole number, found %s", node.Line, name, strconv.Quote(node.Value))}}
	}
	return nil
}

// wholePolicyProblems decodes data a second time, leniently, to check the whole-number policy
// fields with the types above, so Policy keeps its int fields. Only their problems are returned;
// everything else is the main decode's.
func wholePolicyProblems(data []byte) []string {
	var doc struct {
		Policy struct {
			MaxDatabaseAgeHours wholeHours `yaml:"maxDatabaseAgeHours"`
			MaxExceptionDays    wholeDays  `yaml:"maxExceptionDays"`
		} `yaml:"policy"`
	}
	var te *yaml.TypeError
	if err := yaml.Unmarshal(data, &doc); !errors.As(err, &te) {
		return nil
	}
	var out []string
	for _, e := range te.Errors {
		if strings.Contains(e, " must be a whole number, found ") {
			out = append(out, e)
		}
	}
	return out
}

// mergeProblems appends the whole-number problems to the main decode's, except where it already
// reports a wrong number on that line (such as .inf, which does not fit an int either).
func mergeProblems(main, whole []string) []string {
	out := slices.Clone(main)
	for _, w := range whole {
		line, _, _ := strings.Cut(w, ":")
		if !slices.ContainsFunc(main, func(m string) bool { return strings.HasPrefix(m, line+": expected a whole number") }) {
			out = append(out, w)
		}
	}
	return out
}

// Env looks up an environment variable, like os.LookupEnv.
type Env func(string) (string, bool)

// Problems lists every validation problem so users can fix them in one pass.
type Problems []string

func (p Problems) Error() string {
	return "invalid configuration:\n  - " + strings.Join(p, "\n  - ")
}

var (
	namePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	varPattern  = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-[^}]*)?\}`)
	// imagePattern is the character set of an image reference (registry host and port,
	// repository path, tag, digest). It leaves out whitespace, control characters and shell
	// metacharacters, so a reference copied into a command line cannot add a command, and the
	// first character cannot start an option.
	imagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)
)

// Kind returns "image", "dir", "file", "source" or "compose" (empty if none is set).
func (t Target) Kind() string {
	switch {
	case t.Image != "":
		return "image"
	case t.Dir != "":
		return "dir"
	case t.File != "":
		return "file"
	case t.Source != "":
		return "source"
	case t.Compose != "":
		return "compose"
	}
	return ""
}

// Ref returns the image reference or path the target scans.
func (t Target) Ref() string { return t.Image + t.Dir + t.File + t.Source + t.Compose }

// PackageName returns the part of Package before its last '@' (scoped npm names keep their leading '@').
func (e Exception) PackageName() string {
	i := strings.LastIndex(e.Package, "@")
	if i <= 0 {
		return ""
	}
	return e.Package[:i]
}

// PackageVersion returns the part of Package after its last '@'.
func (e Exception) PackageVersion() string {
	i := strings.LastIndex(e.Package, "@")
	if i <= 0 {
		return ""
	}
	return e.Package[i+1:]
}

// ExpiresAt returns the first instant the exception is no longer valid: the end of its expiry day, UTC.
func (e Exception) ExpiresAt() (time.Time, error) {
	day, err := time.Parse("2006-01-02", e.Expires)
	if err != nil {
		return time.Time{}, err
	}
	return day.Add(24 * time.Hour), nil
}

// Load reads, validates and resolves a config file.
func Load(path string, env Env, now time.Time) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{Policy: DefaultPolicy()}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	whole := wholePolicyProblems(data)
	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &FileError{Path: path, Problems: []string{"the file is empty"}}
		}
		return nil, &FileError{Path: path, Problems: mergeProblems(yamlProblems(err), whole)}
	}
	if len(whole) > 0 {
		return nil, &FileError{Path: path, Problems: whole}
	}
	if err := dec.Decode(&yaml.Node{}); !errors.Is(err, io.EOF) {
		return nil, &FileError{Path: path, Problems: []string{"it must contain a single YAML document (remove the extra --- and what follows)"}}
	}
	cfg.Dir = filepath.Dir(abs)
	if err := cfg.finish(env, now); err != nil {
		var p Problems
		if errors.As(err, &p) {
			return nil, &FileError{Path: path, Problems: p}
		}
		return nil, err
	}
	return cfg, nil
}

// AdHoc builds a config for targets given on the command line, using the default policy.
func AdHoc(project, dir string, images, dirs, files, sources []string, env Env, now time.Time) (*Config, error) {
	if project == "" || project == "/" || project == "." {
		project = "adhoc"
	}
	cfg := &Config{Version: 1, Project: project, Policy: DefaultPolicy(), Dir: dir}
	add := func(kind string, refs []string) {
		for i, ref := range refs {
			name := kind
			if i > 0 {
				name = fmt.Sprintf("%s-%d", kind, i+1)
			}
			t := Target{Name: name}
			switch kind {
			case "image":
				t.Image = ref
			case "dir":
				t.Dir = ref
			case "file":
				t.File = ref
			case "source":
				t.Source = ref
			}
			cfg.Targets = append(cfg.Targets, t)
		}
	}
	add("image", images)
	add("dir", dirs)
	add("file", files)
	add("source", sources)
	if err := cfg.finish(env, now); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Expand replaces ${VAR} and ${VAR:-default} with environment values.
// An unset or empty ${VAR} without a default is an error.
func Expand(s string, env Env) (string, error) {
	var missing []string
	out := varPattern.ReplaceAllStringFunc(s, func(m string) string {
		g := varPattern.FindStringSubmatch(m)
		if v, ok := env(g[1]); ok && v != "" {
			return v
		}
		if g[2] != "" {
			return strings.TrimPrefix(g[2], ":-")
		}
		if !slices.Contains(missing, g[1]) {
			missing = append(missing, g[1])
		}
		return ""
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable(s) %s is not set or is empty", strings.Join(missing, ", "))
	}
	if strings.Contains(varPattern.ReplaceAllString(s, ""), "${") {
		return "", fmt.Errorf("malformed variable reference in %q (use ${VAR} or ${VAR:-default})", s)
	}
	return out, nil
}

func (c *Config) finish(env Env, now time.Time) error {
	var p Problems
	if c.Version != 1 {
		p = append(p, fmt.Sprintf("version must be 1, got %d", c.Version))
	}
	if strings.TrimSpace(c.Project) == "" {
		p = append(p, "project is required")
	}
	if len(c.Targets) == 0 {
		p = append(p, "at least one target is required")
	}
	names := map[string]bool{}
	for i := range c.Targets {
		t := &c.Targets[i]
		label := fmt.Sprintf("targets[%d]", i)
		if t.Name != "" {
			label = fmt.Sprintf("target %q", t.Name)
		}
		if t.Name == "" {
			p = append(p, label+": name is required")
		} else if !namePattern.MatchString(t.Name) {
			p = append(p, label+fmt.Sprintf(": name %q may only contain letters, digits, '.', '_' and '-'", t.Name))
		} else if names[t.Name] {
			p = append(p, label+": duplicate name")
		}
		names[t.Name] = true
		set := 0
		for _, v := range []string{t.Image, t.Dir, t.File, t.Source, t.Compose} {
			if v != "" {
				set++
			}
		}
		if set != 1 {
			p = append(p, label+": set exactly one of image, dir, file, source or compose")
			continue
		}
		kind := t.Kind()
		if len(t.Exclude) > 0 && kind != "source" && kind != "compose" {
			p = append(p, label+": exclude is only allowed on source and compose targets")
		}
		if t.LockfileFallback != "" && kind != "file" {
			p = append(p, label+": lockfileFallback is only allowed on file targets")
		}
		fields := []*string{&t.Image, &t.Dir, &t.File, &t.Source, &t.Compose, &t.LockfileFallback}
		for j := range t.Exclude {
			fields = append(fields, &t.Exclude[j])
		}
		imageExpanded := true
		for _, f := range fields {
			v, err := Expand(*f, env)
			if err != nil {
				p = append(p, label+": "+err.Error())
				imageExpanded = imageExpanded && f != &t.Image
				continue
			}
			*f = v
		}
		if t.Ref() == "" {
			p = append(p, label+": "+kind+" is empty after expanding variables")
			continue
		}
		// An image that could not be expanded already has its problem; its raw text holds "${".
		if kind == "image" && imageExpanded && !imagePattern.MatchString(t.Image) {
			p = append(p, label+fmt.Sprintf(": image reference %q may only contain letters, digits, '.', '_', '/', ':', '@' and '-', and must start with a letter or digit", t.Image))
		}
		for _, e := range t.Exclude {
			if kind == "compose" {
				if !namePattern.MatchString(e) {
					p = append(p, label+": exclude entries of a compose target are service names (letters, digits, '.', '_' and '-')")
					break
				}
				continue
			}
			if strings.ContainsAny(e, "*?[{\\") {
				p = append(p, label+fmt.Sprintf(": exclude entries are paths relative to the source, not globs (got %q)", e))
			}
		}
		for _, f := range []*string{&t.Dir, &t.File, &t.Source, &t.Compose, &t.LockfileFallback} {
			if *f != "" && !filepath.IsAbs(*f) {
				*f = filepath.Join(c.Dir, *f)
			}
		}
	}
	pol := c.Policy
	if !(pol.BlockEpssAtLeast > 0 && pol.BlockEpssAtLeast <= 1) {
		p = append(p, "policy.blockEpssAtLeast must be greater than 0 and at most 1")
	}
	if !slices.Contains(FixableLevels, pol.BlockFixable) {
		p = append(p, "policy.blockFixable must be one of "+strings.Join(FixableLevels, ", "))
	}
	if pol.MaxDatabaseAgeHours <= 0 {
		p = append(p, "policy.maxDatabaseAgeHours must be a positive number of hours")
	}
	if pol.MaxExceptionDays <= 0 {
		p = append(p, "policy.maxExceptionDays must be a positive number of days")
	}
	today := now.UTC().Truncate(24 * time.Hour)
	for i, e := range c.Exceptions {
		label := fmt.Sprintf("exceptions[%d]", i)
		if e.ID != "" {
			label = "exception " + e.ID
		}
		required := []struct{ name, value string }{
			{"id", e.ID}, {"package", e.Package}, {"reason", e.Reason}, {"approvedBy", e.ApprovedBy}, {"expires", e.Expires},
		}
		for _, r := range required {
			if strings.TrimSpace(r.value) == "" {
				p = append(p, label+": "+r.name+" is required")
			}
		}
		v := e.PackageVersion()
		if e.Package != "" && (e.PackageName() == "" || v == "" || strings.ContainsAny(e.Package, "*?<>| ") || strings.HasPrefix(v, "^") || strings.HasPrefix(v, "~")) {
			p = append(p, label+": package must be an exact name@version, without wildcards or ranges")
		}
		if e.Target != "" && !names[e.Target] {
			p = append(p, label+": target "+e.Target+" is not defined")
		}
		if e.Expires != "" {
			day, err := time.Parse("2006-01-02", e.Expires)
			switch {
			case err != nil:
				p = append(p, label+": expires must be a date like 2026-12-01")
			case pol.MaxExceptionDays > 0 && day.Sub(today) > time.Duration(pol.MaxExceptionDays)*24*time.Hour:
				p = append(p, fmt.Sprintf("%s: expires %s is more than policy.maxExceptionDays (%d) days away", label, e.Expires, pol.MaxExceptionDays))
			}
		}
	}
	if len(p) > 0 {
		return p
	}
	return nil
}
