package zosia

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// What an answer may hold. The code and the secret are printed or handed on, so neither may carry
// anything but the plain characters Zosia makes them of; and the answer's times set how long this
// waits, so they are bounded (Zosia gives 600 and 5 seconds).
var (
	userCodeForm = regexp.MustCompile(`^[A-Za-z0-9-]{1,32}$`)
	secretForm   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,512}$`)
)

const (
	maxConnectExpiry   = 900
	maxConnectInterval = 60
)

// ConnectAsk is what Zosia's approval page shows of what asks for a secret: Shearbolt on this
// machine, for a repository.
type ConnectAsk struct {
	Repository string // host name and path, such as github.com/acme/shop; "" when it cannot be told
	Hostname   string
	Version    string
	SecretName string // the name proposed for the secret; the person approving may change it
}

// ConnectRequest is a started request: the person opens Link and approves the user code; the
// device code collects the secret.
type ConnectRequest struct {
	DeviceCode string `json:"deviceCode"`
	UserCode   string `json:"userCode"`
	ExpiresIn  int    `json:"expiresIn"`
	Interval   int    `json:"interval"`
}

// Link is where the request is approved. It is made from the address the request went to: an
// address in the answer is not used, so the answer cannot send the person elsewhere.
func (r ConnectRequest) Link(url string) string {
	return strings.TrimRight(url, "/") + "/app/connect?code=" + r.UserCode
}

// connectUnit is the unit of the server's intervals; tests shorten it.
var connectUnit = time.Second

// StartConnect asks Zosia for a request to hand this repository a secret.
func StartConnect(ctx context.Context, hc *http.Client, url, agent string, ask ConnectAsk) (ConnectRequest, error) {
	status, header, data, err := postJSON(ctx, hc, url, "/api/v1/connect", agent, "", map[string]string{
		"kind": "repository", "repository": ask.Repository, "hostname": ask.Hostname, "version": ask.Version, "secretName": ask.SecretName,
	})
	if err != nil {
		return ConnectRequest{}, err
	}
	var r ConnectRequest
	if status != http.StatusCreated {
		return r, answerError(status, header, data)
	}
	if err := json.Unmarshal(data, &r); err != nil || r.DeviceCode == "" || !userCodeForm.MatchString(r.UserCode) ||
		r.Interval < 1 || r.Interval > maxConnectInterval || r.ExpiresIn < 1 || r.ExpiresIn > maxConnectExpiry {
		return ConnectRequest{}, &Error{Code: "BAD_RESPONSE", Message: "Zosia answered with something that is not a connect request"}
	}
	return r, nil
}

// WaitConnect polls until the request is approved and returns the secret and its name in Zosia.
// A denial, an expiry and a cancelled context end it with an Error of that code.
func WaitConnect(ctx context.Context, hc *http.Client, url, agent string, r ConnectRequest) (secret, name string, err error) {
	ctx, cancel := context.WithTimeout(ctx, time.Duration(r.ExpiresIn)*connectUnit)
	defer cancel()
	wait := time.Duration(r.Interval) * connectUnit
	last := &Error{Code: "EXPIRED", Message: "nobody approved the request in time"}
	for {
		select {
		case <-ctx.Done():
			if ctx.Err() == context.Canceled {
				return "", "", &Error{Code: "CANCELLED", Message: "cancelled"}
			}
			return "", "", last
		case <-time.After(wait):
		}
		wait = time.Duration(r.Interval) * connectUnit
		status, header, data, err := postJSON(ctx, hc, url, "/api/v1/connect/token", agent, "", map[string]string{"deviceCode": r.DeviceCode})
		if err != nil {
			if ctx.Err() == nil {
				last = err
			}
			continue
		}
		switch {
		case status == http.StatusOK:
			var res struct{ Secret, Name string }
			if err := json.Unmarshal(data, &res); err != nil || !secretForm.MatchString(res.Secret) {
				return "", "", &Error{Code: "BAD_RESPONSE", Message: "Zosia answered with something that is not a secret"}
			}
			return res.Secret, res.Name, nil
		case status == http.StatusAccepted:
		case status == http.StatusTooManyRequests || status >= 500:
			if e := answerError(status, header, data); e.after > 0 {
				wait = e.after
			}
		default:
			return "", "", answerError(status, header, data)
		}
	}
}

// postJSON sends body as JSON and returns the answer as it is. With a secret it is sent as the
// request's bearer.
func postJSON(ctx context.Context, hc *http.Client, url, path, agent, secret string, body any) (int, http.Header, []byte, *Error) {
	data, err := json.Marshal(body)
	if err != nil {
		return 0, nil, nil, &Error{Code: "PAYLOAD", Message: err.Error()}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(url, "/")+path, bytes.NewReader(data))
	if err != nil {
		return 0, nil, nil, &Error{Code: "NETWORK", Message: "invalid Zosia URL: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", agent)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, nil, &Error{Code: "NETWORK", Message: "cannot reach Zosia: " + unwrapped(err)}
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, resp.Header, answer, nil
}

// answerError reads Zosia's {code, message} body as send.go's post does.
func answerError(status int, header http.Header, data []byte) *Error {
	var e struct{ Code, Message string }
	_ = json.Unmarshal(data, &e)
	if e.Code == "" {
		e.Code = "HTTP_" + strconv.Itoa(status)
	}
	if e.Message == "" {
		e.Message = fmt.Sprintf("Zosia answered %d %s", status, http.StatusText(status))
	}
	out := &Error{Code: e.Code, Message: e.Message}
	if s, err := strconv.Atoi(header.Get("Retry-After")); err == nil && s > 0 {
		out.after = time.Duration(s) * connectUnit
	}
	return out
}
