package zosia

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// CI is the pipeline a report comes from, as sent in source.ci.
type CI struct {
	Provider      string  `json:"provider"`
	Server        string  `json:"server"`
	Repository    string  `json:"repository"`
	Ref           string  `json:"ref"`
	Branch        *string `json:"branch"`
	DefaultBranch *string `json:"defaultBranch"`
	SHA           string  `json:"sha"`
	Event         string  `json:"event"`
	PullRequest   *int    `json:"pullRequest"`
	RunURL        string  `json:"runUrl"`
	Workflow      string  `json:"workflow"`
	Job           string  `json:"job"`
	CloneDir      string  `json:"-"`
}

var githubPR = regexp.MustCompile(`^refs/pull/(\d+)/merge$`)

func ptr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func num(s string) *int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return nil
	}
	return &n
}

// DetectCI reads the pipeline from the environment. ok is false outside GitHub Actions, GitLab CI
// and Bitbucket Pipelines, or when the repository cannot be named.
func DetectCI(getenv func(string) string, readFile func(string) ([]byte, error)) (CI, bool) {
	var c CI
	switch {
	case getenv("GITHUB_ACTIONS") == "true":
		c = CI{Provider: "github", Server: getenv("GITHUB_SERVER_URL"), Repository: getenv("GITHUB_REPOSITORY"),
			Ref: getenv("GITHUB_REF"), SHA: getenv("GITHUB_SHA"), Event: getenv("GITHUB_EVENT_NAME"),
			Workflow: getenv("GITHUB_WORKFLOW"), Job: getenv("GITHUB_JOB"), CloneDir: getenv("GITHUB_WORKSPACE")}
		switch {
		case getenv("GITHUB_HEAD_REF") != "":
			c.Branch = ptr(getenv("GITHUB_HEAD_REF"))
		case strings.HasPrefix(c.Ref, "refs/heads/"):
			c.Branch = ptr(getenv("GITHUB_REF_NAME"))
		}
		if m := githubPR.FindStringSubmatch(c.Ref); m != nil {
			c.PullRequest = num(m[1])
		}
		if id := getenv("GITHUB_RUN_ID"); id != "" && c.Server != "" {
			c.RunURL = c.Server + "/" + c.Repository + "/actions/runs/" + id
			if a := getenv("GITHUB_RUN_ATTEMPT"); a != "" {
				c.RunURL += "/attempts/" + a
			}
		}
		if p := getenv("GITHUB_EVENT_PATH"); p != "" {
			if data, err := readFile(p); err == nil {
				var ev struct {
					Repository struct {
						DefaultBranch string `json:"default_branch"`
					} `json:"repository"`
				}
				if json.Unmarshal(data, &ev) == nil {
					c.DefaultBranch = ptr(ev.Repository.DefaultBranch)
				}
			}
		}
	case getenv("GITLAB_CI") == "true":
		c = CI{Provider: "gitlab", Server: getenv("CI_SERVER_URL"), Repository: getenv("CI_PROJECT_PATH"),
			Ref: getenv("CI_COMMIT_REF_NAME"), SHA: getenv("CI_COMMIT_SHA"), Event: getenv("CI_PIPELINE_SOURCE"),
			PullRequest: num(getenv("CI_MERGE_REQUEST_IID")), RunURL: getenv("CI_JOB_URL"),
			Workflow: getenv("CI_PIPELINE_NAME"), Job: getenv("CI_JOB_NAME"),
			DefaultBranch: ptr(getenv("CI_DEFAULT_BRANCH")), CloneDir: getenv("CI_PROJECT_DIR")}
		c.Branch = ptr(getenv("CI_MERGE_REQUEST_SOURCE_BRANCH_NAME"))
		if c.Branch == nil {
			c.Branch = ptr(getenv("CI_COMMIT_BRANCH"))
		}
	case getenv("BITBUCKET_BUILD_NUMBER") != "":
		repo := getenv("BITBUCKET_REPO_FULL_NAME")
		c = CI{Provider: "bitbucket", Server: "https://bitbucket.org", Repository: repo,
			Branch: ptr(getenv("BITBUCKET_BRANCH")), SHA: getenv("BITBUCKET_COMMIT"),
			PullRequest: num(getenv("BITBUCKET_PR_ID")), Event: "push", Job: getenv("BITBUCKET_STEP_UUID"),
			RunURL:   "https://bitbucket.org/" + repo + "/pipelines/results/" + getenv("BITBUCKET_BUILD_NUMBER"),
			CloneDir: getenv("BITBUCKET_CLONE_DIR")}
		c.Ref = getenv("BITBUCKET_BRANCH")
		if c.Ref == "" {
			c.Ref = getenv("BITBUCKET_TAG")
		}
		if c.PullRequest != nil {
			c.Event = "pull_request"
		}
	default:
		return CI{}, false
	}
	if c.Server == "" || c.Repository == "" {
		return CI{}, false
	}
	return c, true
}

// RepoPath is path relative to the clone directory, slash-separated, or its base name when it
// lies outside (or the clone directory is unknown).
func (c CI) RepoPath(path string) string {
	abs, err := filepath.Abs(path)
	if err == nil && c.CloneDir != "" {
		if rel, err := filepath.Rel(c.CloneDir, abs); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.Base(path)
}
