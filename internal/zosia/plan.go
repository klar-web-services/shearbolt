package zosia

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// PlanSchema is the document Zosia answers about an account's plan.
const PlanSchema = "zosia.plan/1"

// PlanRequest asks what the account of Secret has room for. Server and Repository name the
// repository as its reports do (the CI's server and its path there); both empty names none.
type PlanRequest struct {
	URL        string // base URL, such as https://zosiasec.com
	Secret     string
	Client     string
	Version    string
	Server     string
	Repository string
}

// PlanAnswer is the plan's room for projects. MaxProjects is nil when the plan has no limit,
// and Projects is how many the account has. Known says whether Zosia has the named repository;
// Here lists the shearbolt.yaml paths of its projects, which need no further place.
type PlanAnswer struct {
	Plan        string
	MaxProjects *int
	Projects    int
	Known       bool
	Here        []string
}

// FetchPlan asks once. It is asked by a person at a terminal, who can run the command again.
func FetchPlan(ctx context.Context, hc *http.Client, r PlanRequest) (PlanAnswer, error) {
	body := map[string]string{}
	if r.Server != "" && r.Repository != "" {
		body["server"], body["repository"] = r.Server, r.Repository
	}
	status, header, data, err := postJSON(ctx, hc, r.URL, "/api/v1/plan", r.Client+"/"+r.Version, r.Secret, body)
	if err != nil {
		return PlanAnswer{}, err
	}
	if status != http.StatusOK {
		return PlanAnswer{}, answerError(status, header, data)
	}
	answer, perr := ParsePlanAnswer(data)
	if perr != nil {
		return PlanAnswer{}, &Error{Code: "BAD_RESPONSE", Message: "Zosia answered with something that is not a plan: " + perr.Error()}
	}
	return answer, nil
}

// ParsePlanAnswer reads the document strictly: its keys by their exact names, each of its type.
// Keys it does not know are ignored, so the document can grow.
func ParsePlanAnswer(data []byte) (PlanAnswer, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return PlanAnswer{}, fmt.Errorf("it is not a JSON object")
	}
	var a PlanAnswer
	var schema string
	if json.Unmarshal(doc["schema"], &schema) != nil || schema != PlanSchema {
		return PlanAnswer{}, fmt.Errorf("its schema is not %s", PlanSchema)
	}
	count := func(key string) (int, error) {
		var n json.Number
		if raw, ok := doc[key]; !ok || len(raw) == 0 || raw[0] == '"' || json.Unmarshal(raw, &n) != nil {
			return 0, fmt.Errorf("%s is not a number", key)
		}
		i, err := n.Int64()
		if err != nil || i < 0 || i > 1<<31 {
			return 0, fmt.Errorf("%s is not a count", key)
		}
		return int(i), nil
	}
	raw, ok := doc["maxProjects"]
	if !ok {
		return PlanAnswer{}, fmt.Errorf("maxProjects is missing")
	}
	if string(raw) != "null" {
		n, err := count("maxProjects")
		if err != nil {
			return PlanAnswer{}, err
		}
		a.MaxProjects = &n
	}
	var err error
	if a.Projects, err = count("projects"); err != nil {
		return PlanAnswer{}, err
	}
	if raw, ok := doc["known"]; !ok || (string(raw) != "true" && string(raw) != "false") {
		return PlanAnswer{}, fmt.Errorf("known is not true or false")
	}
	a.Known = string(doc["known"]) == "true"
	if raw, ok := doc["here"]; !ok || len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &a.Here) != nil {
		return PlanAnswer{}, fmt.Errorf("here is not a list of paths")
	}
	if raw, ok := doc["plan"]; !ok || len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &a.Plan) != nil {
		return PlanAnswer{}, fmt.Errorf("plan is not a name")
	}
	return a, nil
}
