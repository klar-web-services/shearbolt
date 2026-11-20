package zosia

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var fast = Policy{Total: 5 * time.Second, Attempts: 2, Pause: 10 * time.Millisecond, MaxRetryAfter: time.Second}

func request(t *testing.T, url string) (Request, func([]byte) map[string]any) {
	k, priv := testKey(t)
	r := Request{URL: url, Key: k, Secret: "zosia_s3cret", Client: "shearbolt", Version: "1.4.0", Kind: "shearbolt",
		Source: map[string]string{"configPath": "shearbolt.yaml"}, Report: json.RawMessage(`{"runId":"r1"}`), Now: time.Unix(0, 0)}
	return r, func(body []byte) map[string]any {
		plain, err := open(t, priv, r.Secret, body)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(plain, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
}

func TestSendPostsASealedPayload(t *testing.T) {
	var body []byte
	var auth, agent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		auth, agent = req.Header.Get("Authorization"), req.Header.Get("User-Agent")
		body, _ = io.ReadAll(req.Body)
		w.WriteHeader(201)
		io.WriteString(w, `{"id":"abc","url":"https://zosiasec.com/app/reports/abc"}`)
	}))
	defer srv.Close()
	r, opener := request(t, srv.URL)
	res, err := Send(context.Background(), srv.Client(), fast, r)
	if err != nil || res.URL != "https://zosiasec.com/app/reports/abc" {
		t.Fatalf("Send = %+v, %v", res, err)
	}
	got := opener(body)
	if auth != "Bearer zosia_s3cret" || agent != "shearbolt/1.4.0" || got["schema"] != Schema || got["kind"] != "shearbolt" ||
		got["sentAt"] != "1970-01-01T00:00:00Z" || got["report"].(map[string]any)["runId"] != "r1" {
		t.Fatalf("auth %q agent %q payload %v", auth, agent, got)
	}
}

func TestSendRetriesOnceOn5xx(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if n.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		w.WriteHeader(201)
		io.WriteString(w, `{"id":"x","url":"u"}`)
	}))
	defer srv.Close()
	r, _ := request(t, srv.URL)
	if _, err := Send(context.Background(), srv.Client(), fast, r); err != nil || n.Load() != 2 {
		t.Fatalf("err %v after %d attempts", err, n.Load())
	}
}

func TestSendDoesNotRetryClientErrors(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n.Add(1)
		w.WriteHeader(403)
		io.WriteString(w, `{"code":"PLAN_LIMIT","message":"Your plan allows 5 repositories."}`)
	}))
	defer srv.Close()
	r, _ := request(t, srv.URL)
	_, err := Send(context.Background(), srv.Client(), fast, r)
	e, ok := err.(*Error)
	if !ok || e.Code != "PLAN_LIMIT" || e.Message != "Your plan allows 5 repositories." || n.Load() != 1 {
		t.Fatalf("err %#v after %d attempts", err, n.Load())
	}
}

func TestSendGivesUpWhenRetryAfterIsTooLong(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
		io.WriteString(w, `{"code":"RATE_LIMITED","message":"slow down"}`)
	}))
	defer srv.Close()
	r, _ := request(t, srv.URL)
	if _, err := Send(context.Background(), srv.Client(), fast, r); err == nil || n.Load() != 1 {
		t.Fatalf("err %v after %d attempts", err, n.Load())
	}
}

func TestNetworkErrorsNeverNameTheSecret(t *testing.T) {
	r, _ := request(t, "http://127.0.0.1:1")
	_, err := Send(context.Background(), http.DefaultClient, fast, r)
	if err == nil || strings.Contains(err.Error(), r.Secret) || err.(*Error).Code != "NETWORK" {
		t.Fatalf("err %v", err)
	}
}

func TestSendDoesNotRetryATimeout(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n.Add(1)
		w.WriteHeader(408)
		io.WriteString(w, `{"code":"TIMEOUT","message":"The report did not arrive in time."}`)
	}))
	defer srv.Close()
	r, _ := request(t, srv.URL)
	_, err := Send(context.Background(), srv.Client(), fast, r)
	if e, ok := err.(*Error); !ok || e.Code != "TIMEOUT" || n.Load() != 1 {
		t.Fatalf("err %#v after %d attempts", err, n.Load())
	}
}

func TestSendRetriesWhenZosiaIsBusy(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			io.WriteString(w, `{"code":"BUSY","message":"Zosia is busy. Try again shortly."}`)
			return
		}
		w.WriteHeader(201)
		io.WriteString(w, `{"id":"x","url":"u"}`)
	}))
	defer srv.Close()
	r, _ := request(t, srv.URL)
	if _, err := Send(context.Background(), srv.Client(), fast, r); err != nil || n.Load() != 2 {
		t.Fatalf("err %v after %d attempts", err, n.Load())
	}
}
