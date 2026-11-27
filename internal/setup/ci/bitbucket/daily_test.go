package bitbucket_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/setup"
	"github.com/klar-web-services/shearbolt/internal/setup/plan"
	"github.com/klar-web-services/shearbolt/internal/setup/yamledit"
)

// dailyResult runs setup with the daily pipeline for branch and returns the plan and the part of
// bitbucket-pipelines.yml from its custom section on ("" when there is none).
func dailyResult(t *testing.T, pipelines, branch string) (*plan.Plan, string) {
	t.Helper()
	o := setup.Options{Root: tree(t, npmRepo(pipelines)), Yes: true, Version: "1.2.3", DefaultBranch: "main", Branches: []string{"release/1.x", "main", "develop"}}
	if branch != "" {
		o.Answers = map[string]string{"daily-branch": branch}
	}
	p := setup.Compute(o)
	if p.Error != nil {
		t.Fatalf("plan error: %+v", p.Error)
	}
	for _, c := range p.Changes {
		if c.Path != file {
			continue
		}
		if _, err := yamledit.Parse([]byte(c.Content)); err != nil {
			t.Fatalf("the edited file is not valid YAML: %v\n%s", err, c.Content)
		}
		if i := strings.Index(c.Content, "  custom:\n"); i >= 0 {
			return p, c.Content[i:]
		}
	}
	return p, ""
}

func dailyNote(t *testing.T, p *plan.Plan, code, text string) {
	t.Helper()
	var got []plan.Note
	for _, n := range p.Notes {
		if strings.HasPrefix(n.Code, "DAILY_") {
			got = append(got, n)
		}
	}
	if len(got) != 1 || got[0].Code != code || got[0].File != file || !strings.Contains(got[0].Message, text) {
		t.Errorf("daily notes = %+v, want one %s that says %q", got, code, text)
	}
}

const dailyComment = "    # Shearbolt's daily run for %s: the build and the scan of %s, without the push or deploy.\n    # Schedule it under Pipelines > Schedules.\n    shearbolt-daily:\n"

func dailyHeader(branch, pipeline string) string {
	return "  custom:\n" + strings.Replace(strings.Replace(dailyComment, "%s", branch, 1), "%s", pipeline, 1)
}

// The step that builds and pushes is cut after its scan: its deployment, its manual trigger and
// the after-script that pushes go, the rest of the step stays. A step that only deploys is left
// out, and so is everything after the scan.
func TestBitbucketDailyCutsTheStepAfterItsScan(t *testing.T) {
	p, got := dailyResult(t, lines(
		"pipelines:",
		"  branches:",
		"    main:",
		"      - step:",
		"          name: Notify",
		"          script:",
		"            - pipe: atlassian/slack-notify:2.0.0",
		"      - step:",
		"          name: Test",
		"          script:",
		"            - npm test",
		"      - step:",
		"          name: Build and push",
		"          deployment: production",
		"          trigger: manual",
		"          services:",
		"            - docker",
		"          script:",
		"            # the image",
		"            - docker build -t web:1 .",
		"            - docker push web:1",
		"          after-script:",
		"            - docker push web:latest",
		"          artifacts:",
		"            - dist/**",
		"      - step:",
		"          name: Deploy",
		"          script:",
		"            - ./deploy.sh",
	), "")
	want(t, got, dailyHeader("main", "branches/main")+lines(
		"      - step:",
		"          name: Test",
		"          script:",
		"            - npm test",
		"      - step:",
		"          name: Build and push",
		"          services:",
		"            - docker",
		"          caches:",
		"            - shearbolt",
		"          script:",
		"            # the image",
		"            - docker build -t web:1 -t web:shearbolt-$BITBUCKET_COMMIT .",
		"            - "+install,
		"            - "+scanWeb,
		"          artifacts:",
		"            - dist/**",
	))
	dailyNote(t, p, "DAILY_SCHEDULE", "choose branch main and the pipeline custom: shearbolt-daily")
	i := slices.IndexFunc(p.Questions, func(q plan.Question) bool { return q.ID == "daily-branch" })
	if i != len(p.Questions)-1 || p.Questions[i-1].ID != "daily" || p.Questions[i].Default != "main" || p.Questions[i].OnlyIf != "daily=yes" {
		t.Fatalf("questions = %+v", p.Questions)
	}
	var options []string
	for _, o := range p.Questions[i].Options {
		options = append(options, o.Value)
	}
	if !slices.Equal(options, []string{"develop", "main", "release/1.x"}) {
		t.Errorf("branch options = %v", options)
	}
}

const twoPipelines = "pipelines:\n  default:\n    - step:\n        name: Default build\n        script:\n          - docker build -t web:1 .\n" +
	"  branches:\n    '{main,develop}':\n      - step:\n          name: Main build\n          script:\n            - docker build -t web:1 .\n" +
	"    release/*:\n      - step:\n          name: Release build\n          script:\n            - docker build -t web:1 .\n"

// Setup copies the pipeline Bitbucket runs for the branch: its branches entry, by name or
// pattern, else default.
func TestBitbucketDailyTakesThePipelineOfTheBranch(t *testing.T) {
	for branch, tc := range map[string]struct{ pipeline, name string }{
		"main":        {"branches/{main,develop}", "Main build"},
		"release/1.x": {"branches/release/*", "Release build"},
		"hotfix":      {"default", "Default build"},
	} {
		o := setup.Options{Root: tree(t, npmRepo(twoPipelines)), Yes: true, Version: "1.2.3", DefaultBranch: branch}
		p := setup.Compute(o)
		if p.Error != nil {
			t.Fatalf("%s: plan error: %+v", branch, p.Error)
		}
		got := p.Changes[slices.IndexFunc(p.Changes, func(c plan.FileChange) bool { return c.Path == file })].Content
		got = got[strings.Index(got, "  custom:\n"):]
		if !strings.HasPrefix(got, dailyHeader(branch, tc.pipeline)) || strings.Count(got, "name: ") != 1 || !strings.Contains(got, "name: "+tc.name) {
			t.Errorf("%s: daily pipeline:\n%s", branch, got)
		}
	}
}

// A step that ends with its scan is copied as written, so a step shared through an anchor stays
// shared; one that pushes after the scan is written out without the push.
func TestBitbucketDailyAnchoredSteps(t *testing.T) {
	const defs = "definitions:\n  steps:\n    - step: &build\n        name: Build\n        script:\n          - docker build -t web:1 .\n%s" +
		"pipelines:\n  default:\n    - step: *build\n    - step:\n        script:\n          - docker push web:1\n"
	_, got := dailyResult(t, strings.Replace(defs, "%s", "", 1), "")
	want(t, got, dailyHeader("main", "default")+"      - step: *build\n")

	_, got = dailyResult(t, strings.Replace(defs, "%s", "          - docker push web:1\n", 1), "")
	want(t, got, dailyHeader("main", "default")+lines(
		"      - step:",
		"          name: Build",
		"          caches:",
		"            - shearbolt",
		"          script:",
		"            - docker build -t web:1 -t web:shearbolt-$BITBUCKET_COMMIT .",
		"            - "+install,
		"            - "+scanWeb,
	))
}

// An anchor defined inside the pipeline cannot be copied; the copy refers to it instead.
func TestBitbucketDailyInlineAnchor(t *testing.T) {
	_, got := dailyResult(t, "pipelines:\n  branches:\n    main:\n      - step: &build\n          script:\n            - docker build -t web:1 .\n    develop:\n      - step: *build\n", "")
	want(t, got, dailyHeader("main", "branches/main")+"      - step: *build\n")
}

func TestBitbucketDailyJoinsExistingCustomPipelines(t *testing.T) {
	p, _ := dailyResult(t, "pipelines:\n  custom:\n    nightly:\n      - step:\n          script:\n            - echo hi\n  default:\n    - step:\n        script:\n          - docker build -t web:1 .\n", "")
	got := p.Changes[slices.IndexFunc(p.Changes, func(c plan.FileChange) bool { return c.Path == file })].Content
	if !strings.Contains(got, "            - echo hi\n"+strings.TrimPrefix(dailyHeader("main", "default"), "  custom:\n")+"      - step:\n          caches:\n") ||
		strings.Count(got, "custom:") != 1 || !strings.Contains(got, "\n  default:\n") {
		t.Errorf("%s:\n%s", file, got)
	}
}

func TestBitbucketDailySkipped(t *testing.T) {
	for name, tc := range map[string]struct{ pipelines, branch, why string }{
		"a parallel group pushes": {lines(
			"pipelines:",
			"  default:",
			"    - parallel:",
			"        - step:",
			"            script:",
			"              - docker build -t web:1 .",
			"        - step:",
			"            script:",
			"              - ./deploy.sh",
		), "", "the parallel group or stage at line 7 holds a step that pushes or deploys"},
		"no pipeline for the branch": {"pipelines:\n  branches:\n    main:\n      - step:\n          script:\n            - docker build -t web:1 .\n",
			"develop", "no pipeline that setup can read runs for branch develop"},
		"the branch's pipeline has no scan": {"pipelines:\n  default:\n    - step:\n        script:\n          - echo hi\n  tags:\n    v*:\n      - step:\n          script:\n            - docker build -t web:1 .\n",
			"", "the pipeline that runs for branch main (default) has no Shearbolt scan"},
	} {
		t.Run(name, func(t *testing.T) {
			p, got := dailyResult(t, tc.pipelines, tc.branch)
			if got != "" {
				t.Errorf("a daily pipeline was added:\n%s", got)
			}
			dailyNote(t, p, "DAILY_SKIPPED", "no daily pipeline was added, because "+tc.why)
		})
	}
}

// A daily pipeline that is there is kept as it is: no question, no change, and no gate inside it.
func TestBitbucketDailyKept(t *testing.T) {
	root := tree(t, npmRepo("pipelines:\n  default:\n    - step:\n        script:\n          - docker build -t web:1 .\n          - docker push web:1\n"))
	o := setup.Options{Root: root, Yes: true, Version: "1.2.3"}
	if err := setup.Write(root, setup.Compute(o)); err != nil {
		t.Fatal(err)
	}
	p := setup.Compute(o)
	if p.Error != nil || len(p.Changes) != 0 || slices.ContainsFunc(p.Questions, func(q plan.Question) bool { return strings.HasPrefix(q.ID, "daily") }) {
		t.Errorf("error %+v, changes %+v, questions %+v", p.Error, p.Changes, p.Questions)
	}
}

func TestBitbucketDailyNo(t *testing.T) {
	root := tree(t, npmRepo("pipelines:\n  default:\n    - step:\n        script:\n          - docker build -t web:1 .\n"))
	// daily-branch needs no answer once daily is no.
	p := setup.Compute(setup.Options{Root: root, Answers: map[string]string{"daily": "no", "zosia": "no"}, Version: "1.2.3"})
	if p.Error != nil || !p.Complete {
		t.Fatalf("error %+v, complete %v", p.Error, p.Complete)
	}
	for _, c := range p.Changes {
		if strings.Contains(c.Content, "shearbolt-daily") {
			t.Errorf("%s holds a daily pipeline:\n%s", c.Path, c.Content)
		}
	}
}
