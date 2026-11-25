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

const testDeviceCode = "device-code-only-this-machine-knows"

func fastConnect(t *testing.T) {
	old := connectUnit
	connectUnit = time.Millisecond
	t.Cleanup(func() { connectUnit = old })
}

func TestStartConnect(t *testing.T) {
	var got map[string]string
	var agent, path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		agent, path, auth = req.Header.Get("User-Agent"), req.Method+" "+req.URL.Path, req.Header.Get("Authorization")
		json.NewDecoder(req.Body).Decode(&got)
		w.WriteHeader(201)
		io.WriteString(w, `{"deviceCode":"`+testDeviceCode+`","userCode":"BCDF-GHJK","verificationUri":"https://elsewhere.example/app/connect",
			"verificationUriComplete":"https://elsewhere.example/app/connect?code=BCDF-GHJK","expiresIn":600,"interval":5}`)
	}))
	defer srv.Close()
	r, err := StartConnect(context.Background(), srv.Client(), srv.URL+"/", "shearbolt/1.12.0",
		ConnectAsk{Repository: "github.com/acme/shop", Hostname: "laptop", Version: "1.12.0", SecretName: "acme/shop"})
	if err != nil || r != (ConnectRequest{DeviceCode: testDeviceCode, UserCode: "BCDF-GHJK", ExpiresIn: 600, Interval: 5}) {
		t.Fatalf("%+v %v", r, err)
	}
	if path != "POST /api/v1/connect" || agent != "shearbolt/1.12.0" || auth != "" || got["kind"] != "repository" || got["repository"] != "github.com/acme/shop" ||
		got["hostname"] != "laptop" || got["version"] != "1.12.0" || got["secretName"] != "acme/shop" {
		t.Fatalf("%s as %s: %v", path, agent, got)
	}
	// The link is made from the address that was asked, never from one in the answer.
	if link := r.Link(srv.URL + "/"); link != srv.URL+"/app/connect?code=BCDF-GHJK" {
		t.Errorf("link %q", link)
	}
}

func TestStartConnectErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, `{"code":"RATE_LIMITED","message":"Too many requests from this address. Try again later."}`)
	}))
	defer srv.Close()
	_, err := StartConnect(context.Background(), srv.Client(), srv.URL, "shearbolt/test", ConnectAsk{})
	if e, ok := err.(*Error); !ok || e.Code != "RATE_LIMITED" || e.Message != "Too many requests from this address. Try again later." {
		t.Fatalf("%#v", err)
	}
	_, err = StartConnect(context.Background(), http.DefaultClient, "http://127.0.0.1:1", "shearbolt/test", ConnectAsk{})
	if e, ok := err.(*Error); !ok || e.Code != "NETWORK" {
		t.Fatalf("%#v", err)
	}
	for _, body := range []string{`{}`, `{"deviceCode":"d","userCode":"BCDF-GHJK","expiresIn":600,"interval":0}`, `not json`,
		// A wait that Zosia's answer would stretch without end, and a code that would write to the terminal.
		`{"deviceCode":"d","userCode":"BCDF-GHJK","expiresIn":86400,"interval":5}`, `{"deviceCode":"d","userCode":"BCDF-GHJK","expiresIn":600,"interval":3600}`,
		`{"deviceCode":"d","userCode":"BC\u001b[2JDF","expiresIn":600,"interval":5}`, `{"deviceCode":"d","userCode":"BCDF GHJK\r\nx","expiresIn":600,"interval":5}`} {
		bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(201)
			io.WriteString(w, body)
		}))
		_, err := StartConnect(context.Background(), bad.Client(), bad.URL, "shearbolt/test", ConnectAsk{})
		bad.Close()
		if e, ok := err.(*Error); !ok || e.Code != "BAD_RESPONSE" {
			t.Errorf("%s: %#v", body, err)
		}
	}
}

func TestWaitConnectPollsUntilApproved(t *testing.T) {
	fastConnect(t)
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var body map[string]string
		json.NewDecoder(req.Body).Decode(&body)
		if req.URL.Path != "/api/v1/connect/token" || body["deviceCode"] != testDeviceCode {
			t.Errorf("%s %v", req.URL.Path, body)
		}
		switch polls.Add(1) {
		case 1:
			w.WriteHeader(202)
			io.WriteString(w, `{"code":"PENDING","message":"Nobody has answered the request yet."}`)
		case 2:
			w.Header().Set("Retry-After", "2")
			w.WriteHeader(429)
			io.WriteString(w, `{"code":"SLOW_DOWN","message":"Asked again too soon."}`)
		case 3:
			w.WriteHeader(503)
		default:
			io.WriteString(w, `{"secret":"zosia_abc","name":"shop CI"}`)
		}
	}))
	defer srv.Close()
	secret, name, err := WaitConnect(context.Background(), srv.Client(), srv.URL, "shearbolt/test", ConnectRequest{DeviceCode: testDeviceCode, UserCode: "BCDF-GHJK", ExpiresIn: 600, Interval: 1})
	if err != nil || secret != "zosia_abc" || name != "shop CI" || polls.Load() != 4 {
		t.Fatalf("%q %q %v after %d polls", secret, name, err, polls.Load())
	}
}

func TestWaitConnectEnds(t *testing.T) {
	fastConnect(t)
	for name, c := range map[string]struct {
		status int
		body   string
		code   string
	}{
		"denied":          {403, `{"code":"DENIED","message":"The request was denied in Zosia."}`, "DENIED"},
		"the plan's room": {403, `{"code":"PLAN_LIMIT","message":"The account's plan allows 10 secrets."}`, "PLAN_LIMIT"},
		"expired":         {410, `{"code":"EXPIRED","message":"The request has expired."}`, "EXPIRED"},
		"no secret":       {200, `{"name":"x"}`, "BAD_RESPONSE"},
		"an odd secret":   {200, `{"secret":"zosia_\u001b]0;x\u0007","name":"x"}`, "BAD_RESPONSE"},
		"nobody answers":  {202, `{"code":"PENDING"}`, "EXPIRED"},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.WriteHeader(c.status)
			io.WriteString(w, c.body)
		}))
		_, _, err := WaitConnect(context.Background(), srv.Client(), srv.URL, "shearbolt/test", ConnectRequest{DeviceCode: testDeviceCode, ExpiresIn: 20, Interval: 1})
		srv.Close()
		if e, ok := err.(*Error); !ok || e.Code != c.code || strings.Contains(e.Message, testDeviceCode) {
			t.Errorf("%s: %#v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := WaitConnect(ctx, http.DefaultClient, "http://127.0.0.1:1", "shearbolt/test", ConnectRequest{DeviceCode: testDeviceCode, ExpiresIn: 600, Interval: 1})
	if e, ok := err.(*Error); !ok || e.Code != "CANCELLED" {
		t.Errorf("cancelled: %#v", err)
	}
}
