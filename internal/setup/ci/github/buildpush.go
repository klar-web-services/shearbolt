package github

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/klar-web-services/shearbolt/internal/setup/ci/shelledit"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
)

// This file handles docker/build-push-action (R2, R5, Task 12): it is recognized as an image
// build, and a gated one is converted: push becomes false with load: true, the local scan tag is
// added to tags, and its push moves to a step after the gate that pushes the original tags.

const buildPushAction = "docker/build-push-action"

// metaTags matches a tags input that is exactly a metadata-action step's tags output.
var metaTags = regexp.MustCompile(`^\$\{\{\s*steps\.([A-Za-z0-9_-]+)\.outputs\.tags\s*\}\}$`)

// buildPushBuild recognizes a build-push-action step (R2): with.context (default "."), file,
// tags (split on commas and newlines), push (true or an expression pushes), load, platforms
// and outputs. An output that only exports files makes it a BuildOther gate location. Tags that
// are exactly ${{ steps.<id>.outputs.tags }} of a docker/metadata-action step are Computed, with
// that step's first images entry as ComputedRepo.
func buildPushBuild(d *yamledit.Doc, st *step, steps []*step) plan.Build {
	with := d.Get(st.m, "with")
	get := func(k string) (string, bool) {
		n := scalar(d.Get(with, k))
		if n == nil {
			return "", false
		}
		return n.Value, true
	}
	img := &plan.Image{Tool: "build-push-action"}
	ctxv, ok := get("context")
	if !ok {
		ctxv = "."
	}
	img.Context = actionPath(ctxv)
	dir := img.Context
	if f, ok := get("file"); ok {
		img.Dockerfile = actionPath(f)
		dir = ""
		if img.Dockerfile != "" {
			dir = path.Dir(img.Dockerfile)
		}
	} else if img.Context != "" {
		img.Dockerfile = path.Join(img.Context, "Dockerfile")
	}
	if t, ok := get("tags"); ok {
		img.Tags = splitList(t, true)
	}
	if p, ok := get("platforms"); ok {
		if strings.ContainsAny(p, "$`") {
			img.Platforms = 2 // unknown list: not gateable
		} else {
			img.Platforms = len(splitList(p, true))
		}
	}
	push, _ := get("push")
	img.Push = pushInput(push)
	load, _ := get("load")
	img.Load = strings.EqualFold(strings.TrimSpace(load), "true")
	exportOnly := false
	if o, ok := get("outputs"); ok {
		outs := splitList(o, false)
		exportOnly = len(outs) > 0 && !img.Push
		for _, out := range outs {
			switch outputKind(out) {
			case "push":
				img.OutputPush, exportOnly = true, false
			case "load":
				img.Load, exportOnly = true, false
			}
		}
	}
	if len(img.Tags) == 1 {
		if m := metaTags.FindStringSubmatch(img.Tags[0]); m != nil {
			for _, ms := range steps {
				if ms.id == m[1] && actionIs(ms.uses, "docker/metadata-action") {
					img.Computed = m[1]
					img.ComputedRepo = firstImage(value(d.Get(ms.m, "with", "images")))
				}
			}
		}
	}
	if exportOnly {
		return plan.Build{Kind: plan.BuildOther, Dir: dir}
	}
	return plan.Build{Kind: plan.BuildImage, Dir: dir, Image: img}
}

// pushInput reports whether a push input pushes: true in any case, or an expression. Only a
// false value turns it off; anything else counts as a push.
func pushInput(v string) bool {
	v = strings.TrimSpace(v)
	return v != "" && !strings.EqualFold(v, "false")
}

// actionPath returns an action input path as a repo-relative path, or "" for an expression, a
// variable, a URL or a path outside the repository.
func actionPath(v string) string {
	v = strings.TrimSpace(v)
	if strings.Contains(v, "://") || strings.HasPrefix(v, "git@") {
		return ""
	}
	return workDir(v)
}

// splitList splits an action list input on newlines and, when commas is set, on commas, and
// drops empty entries.
func splitList(v string, commas bool) []string {
	if commas {
		v = strings.ReplaceAll(v, ",", "\n")
	}
	var out []string
	for _, e := range strings.Split(v, "\n") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// firstImage returns metadata-action's first images entry without name= and its options.
func firstImage(images string) string {
	for _, e := range strings.Split(images, "\n") {
		if e = strings.TrimSpace(e); e != "" {
			e, _, _ = strings.Cut(e, ",")
			return strings.TrimSpace(strings.TrimPrefix(e, "name="))
		}
	}
	return ""
}

// outputKind classifies an outputs entry like buildx -o (R2): "push" (type=registry, or
// type=image with push=true), "load" (type=docker into the daemon) or "export". An entry that
// cannot be read, such as one with an expression, counts as a push.
func outputKind(v string) string {
	if strings.ContainsAny(v, "$`") {
		return "push"
	}
	if !strings.Contains(v, "=") {
		return "export"
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
		if p, ok := kv["push"]; ok && !strings.EqualFold(p, "false") {
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

// actionEdits converts gated build-push-action build b (Task 12): load: true, and for a push,
// push: false plus a push step after the gate; the scan tag goes into tags. An already
// converted step (push false, load true, a :shearbolt- tag) is left as it is.
func (e *jobEditor) actionEdits(sp *stepPlan, g plan.Gate, b plan.Build) error {
	st := sp.st
	img := b.Image
	if img == nil || img.Tool != "build-push-action" {
		return nil
	}
	iv := imageVarOf(g, b.Pos)
	needTag := iv != nil && !actionHasScanTag(img)
	if !img.Push && img.Load && !needTag {
		return nil
	}
	fail := func(err error) error {
		return failAt(st.line, "this build-push-action step cannot be changed in place (%v)", err)
	}
	if !editable(st) {
		return fail(fmt.Errorf("it uses YAML anchors, aliases or flow style"))
	}
	pad := strings.Repeat(" ", e.unit)
	with := e.d.Get(st.m, "with")
	if with == nil {
		value := "\n" + pad + "load: true"
		if needTag {
			value += "\n" + pad + "tags: " + yamledit.Scalar(iv.ScanTag)
			e.tags = append(e.tags, iv.ScanTag)
		}
		return wrapErr(fail, e.d.SetKey(st.m, "with", value, ""))
	}
	if with.Kind != yaml.MappingNode || flow(with) {
		return fail(fmt.Errorf("its with: is not a block mapping (flow style cannot be edited)"))
	}
	if img.Push {
		if len(img.Tags) == 0 {
			return failAt(st.line, "this build-push-action pushes but has no tags, so setup cannot push its image after the gate")
		}
		if actionHasScanTag(img) {
			return failAt(st.line, "this build-push-action pushes and its tags already hold setup's local scan tag, which must never be pushed; set push back to false, or remove setup's scan tag and Push image step, then run setup again")
		}
		if err := e.stepEnvRefs(st, with); err != nil {
			return err
		}
		if err := e.actionAttestations(st, with); err != nil {
			return err
		}
		if err := e.digestConsumers(st); err != nil {
			return err
		}
		lines, err := e.pushStepLines(st, with)
		if err != nil {
			return err
		}
		if err := e.d.SetKey(with, "push", "false", ""); err != nil {
			return fail(err)
		}
		if !img.Load { // outputs: type=docker already loads, and buildx refuses both
			if err := e.d.SetKey(with, "load", "true", "push"); err != nil {
				return fail(err)
			}
		}
		sp.pushStep = lines
		e.attest = true
	} else if !img.Load {
		if err := e.actionAttestations(st, with); err != nil {
			return err
		}
		after := ""
		if e.d.Get(with, "push") != nil {
			after = "push"
		}
		if err := e.d.SetKey(with, "load", "true", after); err != nil {
			return fail(err)
		}
	}
	if needTag {
		if err := e.addActionTag(with, iv.ScanTag); err != nil {
			return fail(err)
		}
		e.tags = append(e.tags, iv.ScanTag)
	}
	return nil
}

func actionHasScanTag(img *plan.Image) bool {
	for _, t := range img.Tags {
		if strings.Contains(t, ":shearbolt-") {
			return true
		}
	}
	return false
}

// addActionTag adds tag to the tags input (R5): a new line in a block, ",<tag>" on a one-line
// literal list, and a two-line block for a one-line expression such as
// ${{ steps.meta.outputs.tags }}.
func (e *jobEditor) addActionTag(with *yaml.Node, tag string) error {
	tn := e.d.Get(with, "tags")
	if tn == nil {
		return e.d.SetKey(with, "tags", yamledit.Scalar(tag), "")
	}
	if tn.Kind != yaml.ScalarNode {
		return fmt.Errorf("tags is not a string")
	}
	v := tn.Value
	var nv string
	switch {
	case tn.Style&(yaml.LiteralStyle|yaml.FoldedStyle) != 0 || strings.Contains(strings.TrimRight(v, "\n"), "\n"):
		if strings.HasSuffix(v, "\n") {
			nv = v + tag + "\n"
		} else {
			nv = v + "\n" + tag
		}
	case wholeExpr(strings.TrimSpace(v)):
		nv = strings.TrimSpace(v) + "\n" + tag + "\n"
	default:
		nv = strings.TrimRight(strings.TrimSpace(v), ",") + "," + tag
	}
	return e.d.ReplaceScript(tn, nv)
}

// pushStepLines renders the step that pushes a converted build's original tags after the gate:
// its if: is the push expression, the build step's if:, or both joined with &&, made safe from
// running after a failed gate; TAGS holds the original tags value.
func (e *jobEditor) pushStepLines(st *step, with *yaml.Node) ([]string, error) {
	if !e.j.runPosix {
		return nil, failAt(st.line, "run steps in this job do not use bash or sh, so setup cannot add the step that pushes the image after the gate")
	}
	tn := e.d.Get(with, "tags")
	pushExpr := ""
	if pv := strings.TrimSpace(value(e.d.Get(with, "push"))); strings.Contains(pv, "${{") {
		pushExpr = pv
	}
	stepIf := ""
	if st.ifNode != nil {
		stepIf = strings.TrimSpace(st.ifNode.Value)
	}
	for _, c := range []string{pushExpr, stepIf} {
		if strings.Contains(c, "${{") && !wholeExpr(c) {
			return nil, failAt(st.line, "the condition %s mixes ${{ }} with other text, so setup cannot build the push step's if:", c)
		}
	}
	cond := pushExpr
	switch {
	case stepIf != "" && pushExpr != "":
		cond = "${{ (" + bareExpr(stepIf) + ") && (" + bareExpr(pushExpr) + ") }}"
	case stepIf != "":
		cond = stepIf
	}
	if cond != "" {
		var err error
		if cond, err = safeIf(cond); err != nil {
			return nil, failAt(st.line, "the push step's if: %v", err)
		}
	}
	pad := strings.Repeat(" ", e.unit)
	out := []string{"name: Push image"}
	if cond != "" {
		out = append(out, "if: "+yamledit.Scalar(cond))
	}
	tags := yamledit.RenderScript(tn.Value, tn.Style, e.unit, 2*e.unit)
	out = append(out, "env:", pad+"TAGS: "+tags[0])
	out = append(out, tags[1:]...)
	return append(out,
		"run: |",
		pad+`for tag in $(echo "$TAGS" | tr ',' ' '); do`,
		pad+pad+`docker push "$tag"`,
		pad+"done",
	), nil
}

// envRef matches env.NAME and env['NAME'] in an expression.
var envRef = regexp.MustCompile(`(?i)\benv(?:\.([A-Za-z_][A-Za-z0-9_-]*)|\[\s*'([^']+)'\s*\])`)

// stepEnvRefs fails a converted step whose push, tags or if: reads a variable of the step's own
// env: the push step after the gate cannot see it, so it would be skipped or push nothing while
// CI stays green.
func (e *jobEditor) stepEnvRefs(st *step, with *yaml.Node) error {
	if len(st.env) == 0 {
		return nil
	}
	texts := []string{value(e.d.Get(with, "push")), value(e.d.Get(with, "tags"))}
	if st.ifNode != nil {
		texts = append(texts, st.ifNode.Value)
	}
	for _, t := range texts {
		for _, m := range envRef.FindAllStringSubmatch(t, -1) {
			name := m[1] + m[2]
			for _, own := range st.env {
				if strings.EqualFold(own, name) {
					return failAt(st.line, "this step's push, tags or if: reads env.%s from the step's own env:, which the push step after the gate cannot see; move %s to the job's env and run setup again", own, own)
				}
			}
		}
	}
	return nil
}

// actionAttestations fails a converted build-push-action that asks for attestations: with
// load: true they break on the classic image store, and docker push would drop them.
func (e *jobEditor) actionAttestations(st *step, with *yaml.Node) error {
	for _, k := range []string{"sbom", "provenance", "attests"} {
		n := e.d.Get(with, k)
		if n == nil {
			continue
		}
		if v := strings.TrimSpace(value(n)); n.Kind != yaml.ScalarNode || v != "" && !strings.EqualFold(v, "false") {
			return failAt(st.line, "this build-push-action asks for attestations (%s), which setup cannot keep when the image is loaded for the gate and pushed afterwards; set %s: false, or push from a step after the gate yourself", k, k)
		}
	}
	return nil
}

// pushBuildRefusals fails a run-step --push build that asks for attestations (--sbom,
// --provenance, --attest, in either flag form, unless set to false), or whose digest output a
// later step reads.
func (e *jobEditor) pushBuildRefusals(st *step, args []shell.Word, line int) error {
	if err := shelledit.AttestationFlags(args, line); err != nil {
		return err
	}
	return e.digestConsumers(st)
}

// digestConsumers fails a converted step with an id whose digest, imageid or metadata output
// another part of the job reads: after load: true or --load that output describes the local
// image, so signing or attesting it would break or name the wrong digest.
func (e *jobEditor) digestConsumers(st *step) error {
	if st.id == "" {
		return nil
	}
	id := regexp.QuoteMeta(st.id)
	ref := regexp.MustCompile(`(?i)\bsteps(?:\.` + id + `|\[\s*'` + id + `'\s*\])\.outputs(?:\.(digest|imageid|metadata)\b|\[\s*'(digest|imageid|metadata)'\s*\])`)
	var found string
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		if n == nil || found != "" || n == st.item || n == st.m {
			return
		}
		if n.Kind == yaml.ScalarNode {
			if m := ref.FindStringSubmatch(n.Value); m != nil {
				found = m[1] + m[2]
			}
			return
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(e.j.node)
	if found != "" {
		return failAt(st.line, "a later part of this job reads steps.%s.outputs.%s, which would describe the locally loaded image instead of the pushed one; push from a step after the gate yourself, or drop that use", st.id, found)
	}
	return nil
}
