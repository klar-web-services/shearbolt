// Package compose reads the images a Docker Compose file pulls, for compose targets and setup
// (design: docs/shearbolt/specs/2026-11-09-compose-targets.md, section 3). It never runs Docker.
package compose

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Image is one pulled image and the services that run it.
type Image struct {
	Ref      string   // after variable expansion
	Platform string   // after variable expansion; "" when the services set none
	Services []string // in file order
	// Line is the first service's image: line, or the service's own line when the image comes
	// through an alias or merge key.
	Line int
}

// Problem stops a file, or one of its services, from being scanned. Its Message names no file
// or line; callers prefix "<file>:<line>: " (P5).
type Problem struct {
	Code    string // COMPOSE_INVALID, COMPOSE_UNSUPPORTED or COMPOSE_VARIABLE
	Service string // the service a COMPOSE_VARIABLE problem fails
	Line    int    // 0 when unknown
	Message string
}

// File is what one compose file pulls.
type File struct {
	Images  []Image
	Builds  []string  // services that build their image (not scanned)
	NoImage []string  // services that set neither image nor build
	Missing []Problem // COMPOSE_VARIABLE, one per service whose image or platform lacks a variable
	// Fatal is a COMPOSE_INVALID or COMPOSE_UNSUPPORTED problem: the file cannot be gated and
	// every other field is empty.
	Fatal *Problem
}

// Pulls reports whether the file pulls at least one image, counting services whose image
// needs a variable that is not set (setup reads without variables).
func (f *File) Pulls() bool { return f.Fatal == nil && (len(f.Images) > 0 || len(f.Missing) > 0) }

var (
	fileName = regexp.MustCompile(`^(docker-)?compose([.-][^/]+)?\.ya?ml$`)
	// imagePattern is config's rule for image references, so a reference cannot carry
	// whitespace or shell metacharacters into a command line, or start an option.
	imagePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:@-]*$`)
	platformPattern = regexp.MustCompile(`^[a-z0-9_]+/[a-z0-9_]+(/[a-z0-9_]+)?$`)
	yamlSyntax      = regexp.MustCompile(`^yaml: line (\d+): (.+)$`)
)

// FileName reports whether name is a compose file name setup looks for: compose.yaml,
// docker-compose.yml and their .<variant> or -<variant> forms such as docker-compose.prod.yml
// and compose-dev.yaml.
func FileName(name string) bool { return fileName.MatchString(name) }

// Read reads the compose file at path. The error is set only when the file cannot be read.
func Read(path string, env Env, exclude []string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, env, exclude), nil
}

// Parse reads a compose file's content. Services named in exclude are dropped before anything
// else is checked; a name the file does not define is COMPOSE_INVALID.
func Parse(data []byte, env Env, exclude []string) *File {
	fatal := func(code string, line int, format string, a ...any) *File {
		return &File{Fatal: &Problem{Code: code, Line: line, Message: fmt.Sprintf(format, a...)}}
	}
	var doc yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		line, msg := 0, strings.TrimPrefix(err.Error(), "yaml: ")
		if m := yamlSyntax.FindStringSubmatch(err.Error()); m != nil {
			line, _ = strconv.Atoi(m[1])
			msg = m[2]
		}
		return fatal("COMPOSE_INVALID", line, "the file is not valid YAML: %s", msg)
	}
	if err := dec.Decode(&yaml.Node{}); !errors.Is(err, io.EOF) {
		return fatal("COMPOSE_UNSUPPORTED", 0, "the file holds more than one YAML document (separated by ---), which Compose merges and Shearbolt does not follow yet")
	}
	root := &yaml.Node{Kind: yaml.MappingNode}
	if len(doc.Content) > 0 {
		root = resolve(doc.Content[0])
	}
	if root.Kind != yaml.MappingNode {
		return fatal("COMPOSE_INVALID", root.Line, "the file is not a YAML mapping")
	}
	top, err := pairs(root, 0)
	if err != nil {
		return fatal("COMPOSE_INVALID", errLine(err), "%v", err)
	}
	if p, ok := get(top, "include"); ok {
		return fatal("COMPOSE_UNSUPPORTED", p.key.Line, "include pulls in other compose files, which Shearbolt does not follow yet, so this file cannot be gated as it is")
	}
	var services []pair
	if p, ok := get(top, "services"); ok {
		switch v := resolve(p.value); {
		case v.Kind == yaml.ScalarNode && v.ShortTag() == "!!null":
		case v.Kind != yaml.MappingNode:
			return fatal("COMPOSE_INVALID", p.key.Line, "services must be a mapping of service names to services")
		default:
			if services, err = pairs(v, 0); err != nil {
				return fatal("COMPOSE_INVALID", errLine(err), "%v", err)
			}
		}
	}
	for _, x := range exclude {
		if !slices.ContainsFunc(services, func(p pair) bool { return p.key.Value == x }) {
			return fatal("COMPOSE_INVALID", 0, "exclude names the service %q, which this file does not define", x)
		}
	}
	f := &File{}
	type key struct{ ref, platform string }
	index := map[key]int{}
	for _, sp := range services {
		name := sp.key.Value
		if slices.Contains(exclude, name) {
			continue
		}
		svc := resolve(sp.value)
		if svc.Kind != yaml.MappingNode {
			return fatal("COMPOSE_INVALID", sp.key.Line, "service %s must be a mapping", name)
		}
		fields, err := pairs(svc, 0)
		if err != nil {
			return fatal("COMPOSE_INVALID", cmp.Or(errLine(err), sp.key.Line), "service %s: %v", name, err)
		}
		if p, ok := get(fields, "extends"); ok {
			return fatal("COMPOSE_UNSUPPORTED", p.key.Line, "service %s uses extends, which Shearbolt does not follow yet; to gate the rest of this file, exclude the service (exclude: [%s]), which is then not scanned", name, name)
		}
		if _, ok := get(fields, "build"); ok {
			f.Builds = append(f.Builds, name)
			continue
		}
		ip, ok := get(fields, "image")
		if !ok {
			f.NoImage = append(f.NoImage, name)
			continue
		}
		line := sp.key.Line
		if sp.value.Kind != yaml.AliasNode && ip.direct && ip.value.Kind != yaml.AliasNode {
			line = ip.key.Line
		}
		raw, ok := str(ip.value)
		if !ok {
			return fatal("COMPOSE_INVALID", line, "the image of service %s must be a string", name)
		}
		ref, missing, err := interpolate(raw, env)
		if err != nil {
			return fatal("COMPOSE_INVALID", line, "the image of service %s: %v", name, err)
		}
		platform, platformLine := "", 0
		if pp, ok := get(fields, "platform"); ok {
			platformLine = pp.key.Line
			praw, ok := str(pp.value)
			if !ok {
				return fatal("COMPOSE_INVALID", platformLine, "the platform of service %s must be a string", name)
			}
			var pm []missingVar
			if platform, pm, err = interpolate(praw, env); err != nil {
				return fatal("COMPOSE_INVALID", platformLine, "the platform of service %s: %v", name, err)
			}
			missing = append(missing, pm...)
		}
		if len(missing) > 0 {
			f.Missing = append(f.Missing, variableProblem(name, line, missing))
			continue
		}
		if !imagePattern.MatchString(ref) {
			return fatal("COMPOSE_INVALID", line, "the image reference %q of service %s may only contain letters, digits, '.', '_', '/', ':', '@' and '-', and must start with a letter or digit", ref, name)
		}
		if platform != "" && !platformPattern.MatchString(platform) {
			return fatal("COMPOSE_INVALID", platformLine, "the platform %q of service %s must look like linux/amd64 or linux/arm64/v8", platform, name)
		}
		k := key{ref, platform}
		if i, ok := index[k]; ok {
			f.Images[i].Services = append(f.Images[i].Services, name)
			continue
		}
		index[k] = len(f.Images)
		f.Images = append(f.Images, Image{Ref: ref, Platform: platform, Services: []string{name}, Line: line})
	}
	return f
}

// variableProblem is the COMPOSE_VARIABLE problem of a service whose image or platform needs
// variables that are not set.
func variableProblem(service string, line int, missing []missingVar) Problem {
	var names, said []string
	for _, m := range missing {
		if !slices.Contains(names, m.name) {
			names = append(names, m.name)
		}
		if m.message != "" {
			said = append(said, m.name+": "+m.message)
		}
	}
	verb, pronoun := "is", "it"
	if len(names) > 1 {
		verb, pronoun = "are", "them"
	}
	msg := fmt.Sprintf("service %s needs %s, which %s not set or empty. Set %s where shearbolt scan runs (for example as a CI variable), give %s a default (${%s:-value}), or, if CI builds this image, exclude the service (exclude: [%s])",
		service, strings.Join(names, ", "), verb, pronoun, pronoun, names[0], service)
	if len(said) > 0 {
		msg += " (the compose file says " + strings.Join(said, "; ") + ")"
	}
	return Problem{Code: "COMPOSE_VARIABLE", Service: service, Line: line, Message: msg}
}

// lineError is a problem with the line it was found on; its text carries no line (P5).
type lineError struct {
	line int
	msg  string
}

func (e *lineError) Error() string { return e.msg }

// errLine is the line a pairs error was found on, or 0.
func errLine(err error) int {
	var le *lineError
	if errors.As(err, &le) {
		return le.line
	}
	return 0
}

// pair is one mapping entry; direct is false for an entry merged in with <<.
type pair struct {
	key, value *yaml.Node
	direct     bool
}

// pairs returns mapping m's entries with merge keys (<<) applied: the mapping's own keys win,
// then earlier merged mappings over later ones, as YAML's merge key type says.
func pairs(m *yaml.Node, depth int) ([]pair, error) {
	if depth > 32 {
		return nil, &lineError{line: m.Line, msg: "merge keys (<<) nest too deeply"}
	}
	var own, merged []pair
	var sources []*yaml.Node
	seen := map[string]bool{}
	for i := 0; i+1 < len(m.Content); i += 2 {
		k, v := m.Content[i], m.Content[i+1]
		if k.Kind == yaml.ScalarNode && k.ShortTag() == "!!merge" {
			if v = resolve(v); v.Kind == yaml.SequenceNode {
				for _, s := range v.Content {
					sources = append(sources, resolve(s))
				}
			} else {
				sources = append(sources, v)
			}
			continue
		}
		seen[k.Value] = true
		own = append(own, pair{k, v, true})
	}
	for _, src := range sources {
		if src.Kind != yaml.MappingNode {
			return nil, &lineError{line: src.Line, msg: "a merge key (<<) must refer to a mapping"}
		}
		ps, err := pairs(src, depth+1)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			if !seen[p.key.Value] {
				seen[p.key.Value] = true
				merged = append(merged, pair{p.key, p.value, false})
			}
		}
	}
	return append(own, merged...), nil
}

func get(ps []pair, key string) (pair, bool) {
	for _, p := range ps {
		if p.key.Value == key {
			return p, true
		}
	}
	return pair{}, false
}

// resolve follows aliases to the node they name.
func resolve(n *yaml.Node) *yaml.Node {
	for i := 0; n.Kind == yaml.AliasNode && n.Alias != nil && i < 32; i++ {
		n = n.Alias
	}
	return n
}

// str returns n's value when it is a string scalar.
func str(n *yaml.Node) (string, bool) {
	n = resolve(n)
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!str" {
		return "", false
	}
	return n.Value, true
}
