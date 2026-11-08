package github_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/setup/ci/github"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/shell"
)

const pushLoop = "        run: |\n" +
	"          for tag in $(echo \"$TAGS\" | tr ',' ' '); do\n" +
	"            docker push \"$tag\"\n" +
	"          done\n"

func TestBuildPushRecognition(t *testing.T) {
	src := head + lines(
		"      - id: meta",
		"        uses: docker/metadata-action@v5",
		"        with:",
		"          images: |",
		"            name=ghcr.io/Acme/Web,enable=true",
		"            docker.io/acme/web",
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          context: ./apps/web",
		"          file: apps/web/Dockerfile.prod",
		"          platforms: linux/amd64",
		"          push: ${{ github.event_name != 'pull_request' }}",
		"          tags: ${{ steps.meta.outputs.tags }}",
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          outputs: type=registry",
		"          tags: a:1",
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          outputs: type=docker",
		"          tags: b:1, c:1",
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          outputs: type=local,dest=out",
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: 'False'",
		"          platforms: ${{ matrix.platforms }}",
	)
	jobs, _, err := github.Plugin{}.Jobs(plan.Repo{Root: t.TempDir()}, wf, []byte(src), func(shell.Command, plan.Context) plan.Classified { return plan.Classified{} })
	if err != nil || len(jobs) != 1 {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	st := jobs[0].Steps
	b := st[2].Builds[0]
	want := plan.Image{Tool: "build-push-action", Tags: []string{"${{ steps.meta.outputs.tags }}"}, Dockerfile: "apps/web/Dockerfile.prod", Context: "apps/web",
		Platforms: 1, Push: true, Computed: "meta", ComputedRepo: "ghcr.io/Acme/Web"}
	if b.Kind != plan.BuildImage || b.Dir != "apps/web" || !reflect.DeepEqual(*b.Image, want) || b.Pos.Command != -1 || b.Pos.Line != 14 || b.Pos.Step != 2 {
		t.Errorf("build = %+v, image %+v", b, b.Image)
	}
	for i, s := range st {
		if len(s.Push) != 0 {
			t.Errorf("step %d: an image build's own push is not a Step.Push: %v", i, s.Push)
		}
	}
	if im := st[3].Builds[0].Image; !im.OutputPush || im.Gateable() || im.Context != "." || im.Dockerfile != "Dockerfile" {
		t.Errorf("type=registry: %+v", im)
	}
	if im := st[4].Builds[0].Image; !im.Load || im.Push || !reflect.DeepEqual(im.Tags, []string{"b:1", "c:1"}) {
		t.Errorf("type=docker: %+v", im)
	}
	if b := st[5].Builds[0]; b.Kind != plan.BuildOther || b.Image != nil {
		t.Errorf("type=local is an export, not an image: %+v", b)
	}
	if im := st[6].Builds[0].Image; im.Push || im.Gateable() {
		t.Errorf("push: 'False' and an expression platform list: %+v", im)
	}
}

func TestBuildPushLiteralTrue(t *testing.T) {
	for _, push := range []string{"true", "TRUE"} {
		p, got := result(t, npmRepo("web", head+lines(
			"      - name: Build",
			"        uses: docker/build-push-action@v6",
			"        with:",
			"          push: "+push,
			"          tags: ghcr.io/acme/web:latest",
		), nil))
		wantWorkflow(t, got, head+lines(
			"      - name: Build",
			"        uses: docker/build-push-action@v6",
			"        with:",
			"          push: false",
			"          load: true",
			"          tags: ghcr.io/acme/web:latest,ghcr.io/acme/web:shearbolt-${{ github.sha }}",
			"      - name: shearbolt",
			"        uses: klar-web-services/shearbolt@v1",
			"        env:",
			"          SHEARBOLT_IMAGE: ghcr.io/acme/web:shearbolt-${{ github.sha }}",
			"      - name: Push image",
			"        env:",
			"          TAGS: ghcr.io/acme/web:latest",
		)+pushLoop)
		if !hasNote(p, "ATTESTATIONS_DROPPED") || !hasNote(p, "TAG_ADDED") {
			t.Errorf("notes = %v", noteCodes(p))
		}
	}
}

func TestBuildPushWithoutPushOnlyLoads(t *testing.T) {
	p, got := result(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          context: .",
		"          tags: web:1",
		"      - run: echo done",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          context: .",
		"          tags: web:1,web:shearbolt-${{ github.sha }}",
		"          load: true",
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: web:shearbolt-${{ github.sha }}",
		"      - run: echo done",
	))
	if hasNote(p, "ATTESTATIONS_DROPPED") {
		t.Errorf("nothing was pushed: %v", noteCodes(p))
	}
}

func TestBuildPushConditions(t *testing.T) {
	_, got := result(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        if: github.ref == 'refs/heads/main'",
		"        with:",
		"          push: ${{ github.event_name == 'push' }}",
		"          tags: web:1",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - uses: docker/build-push-action@v6",
		"        if: github.ref == 'refs/heads/main'",
		"        with:",
		"          push: false",
		"          load: true",
		"          tags: web:1,web:shearbolt-${{ github.sha }}",
		"      - name: shearbolt",
		"        if: github.ref == 'refs/heads/main'",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: web:shearbolt-${{ github.sha }}",
		"      - name: Push image",
		"        if: ${{ (github.ref == 'refs/heads/main') && (github.event_name == 'push') }}",
		"        env:",
		"          TAGS: web:1",
	)+pushLoop)

	_, got = result(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        if: ${{ !cancelled() }}",
		"        with:",
		"          push: ${{ github.event_name == 'push' }}",
		"          tags: web:1",
	), nil))
	for _, want := range []string{
		"      - name: shearbolt\n        if: ${{ !cancelled() }}\n",
		"      - name: Push image\n        if: ${{ success() && ((!cancelled()) && (github.event_name == 'push')) }}\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("workflow lacks %q:\n%s", want, got)
		}
	}

	// A literal push: true still follows the build step's own condition.
	_, got = result(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        if: github.ref == 'refs/heads/main'",
		"        with:",
		"          push: true",
		"          tags: web:1",
	), nil))
	if !strings.Contains(got, "      - name: Push image\n        if: github.ref == 'refs/heads/main'\n") {
		t.Errorf("workflow:\n%s", got)
	}
}

func TestBuildPushTags(t *testing.T) {
	// Tags that already hold a commit tag still get the shearbolt- tag, and it is the one scanned.
	_, got := result(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: true",
		"          tags: ghcr.io/acme/web:${{ github.sha }},ghcr.io/acme/web:latest",
	), nil))
	for _, want := range []string{
		"          tags: ghcr.io/acme/web:${{ github.sha }},ghcr.io/acme/web:latest,ghcr.io/acme/web:shearbolt-${{ github.sha }}\n",
		"          SHEARBOLT_IMAGE: ghcr.io/acme/web:shearbolt-${{ github.sha }}\n",
		"          TAGS: ghcr.io/acme/web:${{ github.sha }},ghcr.io/acme/web:latest\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("workflow lacks %q:\n%s", want, got)
		}
	}

	// A block list keeps its lines plus the new one; the push step gets the original lines.
	_, got = result(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: true",
		"          tags: |",
		"            ghcr.io/acme/web:latest",
		"            ghcr.io/acme/web:1.0",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: false",
		"          load: true",
		"          tags: |",
		"            ghcr.io/acme/web:latest",
		"            ghcr.io/acme/web:1.0",
		"            ghcr.io/acme/web:shearbolt-${{ github.sha }}",
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: ghcr.io/acme/web:shearbolt-${{ github.sha }}",
		"      - name: Push image",
		"        env:",
		"          TAGS: |",
		"            ghcr.io/acme/web:latest",
		"            ghcr.io/acme/web:1.0",
	)+pushLoop)

	// metadata-action images with an expression: the slot's target name is the repository.
	_, got = result(t, npmRepo("storefront", head+lines(
		"      - id: meta",
		"        uses: docker/metadata-action@v5",
		"        with:",
		"          images: ghcr.io/${{ github.repository }}",
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: true",
		"          tags: ${{ steps.meta.outputs.tags }}",
	), nil))
	for _, want := range []string{
		"          tags: |\n            ${{ steps.meta.outputs.tags }}\n            storefront:shearbolt-${{ github.sha }}\n",
		"          SHEARBOLT_IMAGE: storefront:shearbolt-${{ github.sha }}\n",
		"          TAGS: ${{ steps.meta.outputs.tags }}\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("workflow lacks %q:\n%s", want, got)
		}
	}
}

// Several converted builds in one job: every push step follows the gate, in order.
func TestBuildPushSeveralSteps(t *testing.T) {
	_, got := result(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: true",
		"          tags: web:1",
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          file: Dockerfile",
		"          push: true",
		"          tags: worker:1",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: false",
		"          load: true",
		"          tags: web:1,web:shearbolt-${{ github.sha }}",
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          file: Dockerfile",
		"          push: false",
		"          load: true",
		"          tags: worker:1,worker:shearbolt-${{ github.sha }}",
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"        env:",
		"          SHEARBOLT_IMAGE: web:shearbolt-${{ github.sha }}",
		"          SHEARBOLT_IMAGE_WORKER: worker:shearbolt-${{ github.sha }}",
		"      - name: Push image",
		"        env:",
		"          TAGS: web:1",
	)+pushLoop+lines(
		"      - name: Push image",
		"        env:",
		"          TAGS: worker:1",
	)+pushLoop)
}

func TestBuildPushUnsupportedAndOutputs(t *testing.T) {
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        with: {push: true, tags: web:1}",
	), nil), "flow-style with", "flow")
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: true",
	), nil), "push without tags", "no tags")
	// Builds under different conditions cannot share one gate.
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - run: docker build -t web:1 .",
		"      - uses: docker/build-push-action@v6",
		"        if: github.ref == 'refs/heads/main'",
		"        with:",
		"          file: Dockerfile",
		"          push: true",
		"          tags: worker:1",
	), nil), "different conditions", differentIfs)

	// An export is not an image: the config gets no image target.
	p, got := result(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          outputs: type=local,dest=out",
		"      - run: ./upload.sh out",
	), map[string]string{"upload.sh*": "#!/bin/sh\necho upload\n"}))
	if c := change(p, "shearbolt.yaml"); c == nil || strings.Contains(c.Content, "image:") {
		t.Errorf("config: %+v", c)
	}
	if !strings.Contains(got, "          outputs: type=local,dest=out\n      - name: shearbolt\n") {
		t.Errorf("the gate follows the export:\n%s", got)
	}
}

// Batch 4 review fixes.

// The push step cannot see the build step's own env, so a push, tags or if: that reads it would
// be skipped or push nothing while CI stays green.
func TestBuildPushStepEnvRefused(t *testing.T) {
	for _, with := range [][]string{
		{"          push: ${{ env.PUSH }}", "          tags: ghcr.io/acme/web:latest"},
		{"          push: true", "          tags: ${{ env.TAGS }}"},
	} {
		wantUnsupported(t, npmRepo("web", head+lines(
			"      - uses: docker/build-push-action@v6",
			"        env:",
			"          PUSH: ${{ github.ref == 'refs/heads/main' }}",
			"          TAGS: ghcr.io/acme/web:latest",
			"        with:",
		)+lines(with...), nil), with[0], "env.")
	}
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        if: ${{ env['PUSH'] == 'yes' }}",
		"        env:",
		"          PUSH: yes",
		"        with:",
		"          push: true",
		"          tags: web:1",
	), nil), "step if", "env.")
	// The job's env is visible to the push step too.
	_, got := result(t, npmRepo("web", strings.Replace(head, "    steps:\n", "    env:\n      TAGS: ghcr.io/acme/web:latest\n    steps:\n", 1)+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: true",
		"          tags: ${{ env.TAGS }}",
	), nil))
	if !strings.Contains(got, "          TAGS: ${{ env.TAGS }}\n") {
		t.Errorf("workflow:\n%s", got)
	}
}

// After load: true or --load, a step's digest output is the local image's, so its consumers would
// sign or attest the wrong digest.
func TestBuildPushDigestConsumersRefused(t *testing.T) {
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - id: push",
		"        uses: docker/build-push-action@v6",
		"        with:",
		"          push: true",
		"          tags: web:1",
		"      - uses: actions/attest-build-provenance@v2",
		"        with:",
		"          subject-name: web",
		"          subject-digest: ${{ steps.push.outputs.digest }}",
	), nil), "attest-build-provenance", "digest")
	wantUnsupported(t, npmRepo("web", strings.Replace(head, "    steps:\n", "    outputs:\n      image-id: ${{ steps.push.outputs.imageid }}\n    steps:\n", 1)+lines(
		"      - id: push",
		"        uses: docker/build-push-action@v6",
		"        with:",
		"          push: true",
		"          tags: web:1",
	), nil), "job output", "imageid")
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - id: img",
		"        run: docker buildx build --push -t web:1 .",
		"      - run: cosign sign web@${{ steps.img.outputs.digest }}",
	), nil), "run-step --push", "digest")
	// Other outputs are fine.
	_, got := result(t, npmRepo("web", head+lines(
		"      - id: push",
		"        uses: docker/build-push-action@v6",
		"        with:",
		"          push: true",
		"          tags: web:1",
		"      - run: echo ${{ steps.push.outcome }}",
	), nil))
	if !strings.Contains(got, "      - name: Push image\n") {
		t.Errorf("workflow:\n%s", got)
	}
}

const reconvert = "set push back to false, or remove setup's scan tag and Push image step, then run setup again"

// A pushing step that already holds setup's scan tag would push it.
func TestBuildPushReconversionRefused(t *testing.T) {
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: true",
		"          tags: web:1,web:shearbolt-${{ github.sha }}",
	), nil), "push with a scan tag", reconvert)
}

// Attestations a build asks for would break with load or be dropped by docker push.
func TestBuildPushAttestationsRefused(t *testing.T) {
	for _, input := range []string{"provenance: mode=max", "sbom: true", "attests: type=sbom"} {
		wantUnsupported(t, npmRepo("web", head+lines(
			"      - uses: docker/build-push-action@v6",
			"        with:",
			"          push: true",
			"          tags: web:1",
			"          "+input,
		), nil), input, "attestation")
	}
	for _, flag := range []string{"--sbom=true", "--sbom", "--provenance=mode=max", "--attest type=sbom", "--attest=type=provenance"} {
		wantUnsupported(t, npmRepo("web", head+lines(
			"      - run: docker buildx build --push "+flag+" -t web:1 .",
		), nil), flag, "attestation")
	}
	// Turned off explicitly, they are fine.
	_, got := result(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: true",
		"          provenance: false",
		"          sbom: FALSE",
		"          tags: web:1",
	), nil))
	if !strings.Contains(got, "      - name: Push image\n") {
		t.Errorf("workflow:\n%s", got)
	}
	_, got = result(t, npmRepo("web", head+lines(
		"      - name: Image",
		"        run: docker buildx build --push --provenance=false -t web:1 .",
	), nil))
	if !strings.Contains(got, "        run: docker push web:1\n") {
		t.Errorf("workflow:\n%s", got)
	}
}

// outputs: type=docker already loads; adding load: true as well would make buildx refuse.
func TestBuildPushOutputsDockerGetsNoLoad(t *testing.T) {
	_, got := result(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: true",
		"          outputs: type=docker",
		"          tags: web:1",
	), nil))
	if strings.Contains(got, "load:") || !strings.Contains(got, "          push: false\n") || !strings.Contains(got, "      - name: Push image\n") {
		t.Errorf("workflow:\n%s", got)
	}
}

// outputs: type=image,push=true pushes from inside the build: not gateable, so the gate runs
// before the step and the step is left alone.
func TestBuildPushImageOutputPushes(t *testing.T) {
	p, got := result(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          outputs: type=image,push=true",
		"          tags: web:1",
	), nil))
	wantWorkflow(t, got, head+lines(
		"      - name: shearbolt",
		"        uses: klar-web-services/shearbolt@v1",
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          outputs: type=image,push=true",
		"          tags: web:1",
	))
	if !hasNote(p, "PUSH_IN_BUILD") {
		t.Errorf("notes = %v", noteCodes(p))
	}
}

// An aliased step would change the anchored action it points at.
func TestBuildPushAliasStepRefused(t *testing.T) {
	src := lines(
		"x-build: &build",
		"  uses: docker/build-push-action@v6",
		"  with:",
		"    push: true",
		"    tags: web:1",
		"on: push",
		"jobs:",
		"  build:",
		"    runs-on: ubuntu-latest",
		"    steps:",
		"      - uses: actions/checkout@v5",
		"      - *build",
	)
	wantUnsupported(t, npmRepo("web", src, nil), "alias step", "anchors, aliases or flow style")
}

// The push step is a bash script; a job whose run steps default to another shell gets none.
func TestBuildPushNeedsBash(t *testing.T) {
	wantUnsupported(t, npmRepo("web", strings.Replace(head, "    steps:\n", "    defaults:\n      run:\n        shell: pwsh\n    steps:\n", 1)+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: true",
		"          tags: web:1",
	), nil), "pwsh default", "bash or sh")
}

// docker/bake-action that pushes is a push step: a bake push before the build runs ungated.
func TestBakeActionPushes(t *testing.T) {
	for _, push := range []string{"true", "True", "${{ github.event_name == 'push' }}"} {
		p, _ := result(t, npmRepo("web", head+lines(
			"      - uses: docker/bake-action@v5",
			"        with:",
			"          push: "+push,
			"      - run: docker build -t web:1 .",
		), nil))
		if !hasNote(p, "PUSH_BEFORE_GATE") {
			t.Errorf("push: %s: notes = %v", push, noteCodes(p))
		}
	}
	src := head + lines(
		"      - uses: docker/bake-action@v5",
		"        with:",
		"          push: false",
	)
	jobs, _, err := github.Plugin{}.Jobs(plan.Repo{Root: t.TempDir()}, wf, []byte(src), func(shell.Command, plan.Context) plan.Classified { return plan.Classified{} })
	if err != nil || len(jobs[0].Steps[1].Push) != 0 {
		t.Errorf("push: false is no push: %+v, %v", jobs, err)
	}
}

// Batch 4 follow-up: setup adds load next to explicit attestations in builds that do not push
// as well, so those are refused the same way.
func TestLoadWithAttestationsRefused(t *testing.T) {
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          sbom: true",
		"          tags: web:1",
	), nil), "build-push-action without push", "attestation")
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - run: docker buildx build --provenance=mode=max -t web:1 .",
	), nil), "buildx without --push", "attestation")
}

// Mutant killers for the Batch 4 review guards.
func TestBuildPushGuardDetails(t *testing.T) {
	// Env names are compared case-insensitively, as GitHub does.
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        env:",
		"          push_flag: 'true'",
		"        with:",
		"          push: ${{ env.PUSH_FLAG }}",
		"          tags: web:1",
	), nil), "env name case", "env.")
	// An attestation input that is not a plain value.
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - uses: docker/build-push-action@v6",
		"        with:",
		"          push: true",
		"          tags: web:1",
		"          attests:",
		"            - type=sbom",
	), nil), "attests list", "attestation")
	// --attest takes a value, and =false does not turn it off.
	wantUnsupported(t, npmRepo("web", head+lines(
		"      - run: docker buildx build --push --attest=false -t web:1 .",
	), nil), "--attest=false", "attestation")
	// The bracket forms of a digest reference.
	for _, ref := range []string{"steps.push.outputs['digest']", "steps['push'].outputs.digest"} {
		wantUnsupported(t, npmRepo("web", head+lines(
			"      - id: push",
			"        uses: docker/build-push-action@v6",
			"        with:",
			"          push: true",
			"          tags: web:1",
			"      - run: echo ${{ "+ref+" }}",
		), nil), ref, "digest")
	}
	// The step's own text is not a consumer of its output.
	_, got := result(t, npmRepo("web", head+lines(
		"      - id: push",
		"        uses: docker/build-push-action@v6",
		"        with:",
		"          push: true",
		"          tags: web:1",
		"          labels: org.example.note=steps.push.outputs.digest",
	), nil))
	if !strings.Contains(got, "      - name: Push image\n") {
		t.Errorf("workflow:\n%s", got)
	}
}
