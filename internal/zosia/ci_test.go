package zosia

import (
	"errors"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func noFile(string) ([]byte, error) { return nil, errors.New("none") }

func TestDetectGitHubPullRequest(t *testing.T) {
	c, ok := DetectCI(env(map[string]string{
		"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "o/r",
		"GITHUB_REF": "refs/pull/12/merge", "GITHUB_REF_NAME": "12/merge", "GITHUB_HEAD_REF": "feature",
		"GITHUB_SHA": "abc", "GITHUB_EVENT_NAME": "pull_request", "GITHUB_RUN_ID": "99", "GITHUB_RUN_ATTEMPT": "2",
		"GITHUB_WORKFLOW": "Image", "GITHUB_JOB": "image", "GITHUB_EVENT_PATH": "/ev.json", "GITHUB_WORKSPACE": "/w",
	}), func(p string) ([]byte, error) { return []byte(`{"repository":{"default_branch":"main"}}`), nil })
	if !ok || *c.Branch != "feature" || *c.PullRequest != 12 || *c.DefaultBranch != "main" ||
		c.RunURL != "https://github.com/o/r/actions/runs/99/attempts/2" || c.Event != "pull_request" {
		t.Fatalf("%+v", c)
	}
}

func TestDetectGitHubTagHasNoBranch(t *testing.T) {
	c, ok := DetectCI(env(map[string]string{"GITHUB_ACTIONS": "true", "GITHUB_SERVER_URL": "https://github.com",
		"GITHUB_REPOSITORY": "o/r", "GITHUB_REF": "refs/tags/v1", "GITHUB_REF_NAME": "v1"}), noFile)
	if !ok || c.Branch != nil || c.PullRequest != nil || c.DefaultBranch != nil {
		t.Fatalf("%+v", c)
	}
}

func TestDetectGitLabMergeRequest(t *testing.T) {
	c, ok := DetectCI(env(map[string]string{"GITLAB_CI": "true", "CI_SERVER_URL": "https://gitlab.com",
		"CI_PROJECT_PATH": "g/p", "CI_COMMIT_REF_NAME": "feat", "CI_MERGE_REQUEST_SOURCE_BRANCH_NAME": "feat",
		"CI_MERGE_REQUEST_IID": "7", "CI_PIPELINE_SOURCE": "merge_request_event", "CI_DEFAULT_BRANCH": "main",
		"CI_JOB_URL": "https://gitlab.com/g/p/-/jobs/1"}), noFile)
	if !ok || *c.Branch != "feat" || *c.PullRequest != 7 || *c.DefaultBranch != "main" || c.RunURL != "https://gitlab.com/g/p/-/jobs/1" {
		t.Fatalf("%+v", c)
	}
}

func TestDetectBitbucket(t *testing.T) {
	c, ok := DetectCI(env(map[string]string{"BITBUCKET_BUILD_NUMBER": "5", "BITBUCKET_REPO_FULL_NAME": "w/r",
		"BITBUCKET_BRANCH": "main", "BITBUCKET_COMMIT": "def"}), noFile)
	if !ok || c.Provider != "bitbucket" || *c.Branch != "main" || c.Ref != "main" || c.Event != "push" ||
		c.RunURL != "https://bitbucket.org/w/r/pipelines/results/5" || c.DefaultBranch != nil {
		t.Fatalf("%+v", c)
	}
}

func TestDetectUnknownCI(t *testing.T) {
	if _, ok := DetectCI(env(map[string]string{"CI": "true"}), noFile); ok {
		t.Fatal("detected a CI from CI=true alone")
	}
}

func TestRepoPath(t *testing.T) {
	c := CI{CloneDir: "/w"}
	if got := c.RepoPath("/w/apps/web/shearbolt.yaml"); got != "apps/web/shearbolt.yaml" {
		t.Fatal(got)
	}
	if got := c.RepoPath("/elsewhere/shearbolt.yaml"); got != "shearbolt.yaml" {
		t.Fatal(got)
	}
}
