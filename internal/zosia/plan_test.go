package zosia

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestFetchPlan(t *testing.T) {
	var got map[string]string
	var path, auth, agent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path, auth, agent = req.Method+" "+req.URL.Path, req.Header.Get("Authorization"), req.Header.Get("User-Agent")
		got = nil
		json.NewDecoder(req.Body).Decode(&got)
		io.WriteString(w, `{"schema":"zosia.plan/1","plan":"Free","maxProjects":3,"projects":2,"known":true,"here":["shearbolt.yaml"],"later":"ignored"}`)
	}))
	defer srv.Close()
	r := PlanRequest{URL: srv.URL + "/", Secret: "zosia_x", Client: "shearbolt", Version: "1.12.0", Server: "https://github.com", Repository: "acme/shop"}
	a, err := FetchPlan(context.Background(), srv.Client(), r)
	if err != nil || a.Plan != "Free" || a.MaxProjects == nil || *a.MaxProjects != 3 || a.Projects != 2 || !a.Known || !slices.Equal(a.Here, []string{"shearbolt.yaml"}) {
		t.Fatalf("%+v %v", a, err)
	}
	if path != "POST /api/v1/plan" || auth != "Bearer zosia_x" || agent != "shearbolt/1.12.0" || got["server"] != "https://github.com" || got["repository"] != "acme/shop" {
		t.Fatalf("%s as %s: %v", path, agent, got)
	}
	// Without a repository to name, the body names none.
	r.Server, r.Repository = "", ""
	if _, err := FetchPlan(context.Background(), srv.Client(), r); err != nil || len(got) != 0 {
		t.Fatalf("body %v, error %v", got, err)
	}
}

func TestFetchPlanErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"code":"BAD_SECRET","message":"This secret is unknown or was revoked."}`)
	}))
	defer srv.Close()
	_, err := FetchPlan(context.Background(), srv.Client(), PlanRequest{URL: srv.URL, Secret: "zosia_secret"})
	if e, ok := err.(*Error); !ok || e.Code != "BAD_SECRET" || e.Message != "This secret is unknown or was revoked." {
		t.Fatalf("%#v", err)
	}
	_, err = FetchPlan(context.Background(), http.DefaultClient, PlanRequest{URL: "http://127.0.0.1:1", Secret: "zosia_secret"})
	if e, ok := err.(*Error); !ok || e.Code != "NETWORK" {
		t.Fatalf("%#v", err)
	}
	// An answer that sends the request elsewhere is not followed with the secret.
	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		t.Error("the request followed a redirect")
	}))
	defer elsewhere.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Redirect(w, req, elsewhere.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	hc := redirect.Client()
	hc.CheckRedirect = NoRedirect
	if _, err := FetchPlan(context.Background(), hc, PlanRequest{URL: redirect.URL, Secret: "zosia_secret"}); err == nil {
		t.Error("a redirect was taken for an answer")
	}
}

func TestParsePlanAnswer(t *testing.T) {
	a, err := ParsePlanAnswer([]byte(`{"schema":"zosia.plan/1","plan":"Unlimited","maxProjects":null,"projects":0,"known":false,"here":[]}`))
	if err != nil || a.MaxProjects != nil || a.Plan != "Unlimited" || a.Known || len(a.Here) != 0 {
		t.Fatalf("%+v %v", a, err)
	}
	for name, body := range map[string]string{
		"another schema":       `{"schema":"zosia.plan/2","plan":"Free","maxProjects":3,"projects":2,"known":true,"here":[]}`,
		"no schema":            `{"plan":"Free","maxProjects":3,"projects":2,"known":true,"here":[]}`,
		"a limit in quotes":    `{"schema":"zosia.plan/1","plan":"Free","maxProjects":"3","projects":2,"known":true,"here":[]}`,
		"no limit at all":      `{"schema":"zosia.plan/1","plan":"Free","projects":2,"known":true,"here":[]}`,
		"a negative limit":     `{"schema":"zosia.plan/1","plan":"Free","maxProjects":-1,"projects":2,"known":true,"here":[]}`,
		"a count that is half": `{"schema":"zosia.plan/1","plan":"Free","maxProjects":3,"projects":2.5,"known":true,"here":[]}`,
		"no count":             `{"schema":"zosia.plan/1","plan":"Free","maxProjects":3,"known":true,"here":[]}`,
		"known as a word":      `{"schema":"zosia.plan/1","plan":"Free","maxProjects":3,"projects":2,"known":"yes","here":[]}`,
		"here is no list":      `{"schema":"zosia.plan/1","plan":"Free","maxProjects":3,"projects":2,"known":true,"here":"shearbolt.yaml"}`,
		"here holds a number":  `{"schema":"zosia.plan/1","plan":"Free","maxProjects":3,"projects":2,"known":true,"here":[1]}`,
		"keys in other case":   `{"Schema":"zosia.plan/1","Plan":"Free","MaxProjects":3,"Projects":2,"Known":true,"Here":[]}`,
		"a list":               `[]`,
		"not JSON":             `<html>`,
	} {
		if a, err := ParsePlanAnswer([]byte(body)); err == nil {
			t.Errorf("%s: no error, got %+v", name, a)
		}
	}
}
