package zosia

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const Schema = "zosia.report/1"

type Request struct {
	URL     string // base URL, such as https://zosiasec.com
	Key     Key
	Secret  string
	Client  string // shearbolt or tripcoil
	Version string
	Kind    string
	Source  any
	Report  json.RawMessage
	Now     time.Time
}

// Policy: how long a send may take in all, how many attempts it makes, the pause before a
// retry, and the longest Retry-After it honors.
type Policy struct {
	Total         time.Duration
	Attempts      int
	Pause         time.Duration
	MaxRetryAfter time.Duration
}

type Result struct {
	ID        string `json:"id"`
	URL       string `json:"url"`
	Duplicate bool   `json:"duplicate"`
}

// Error carries Zosia's error code (or NETWORK, SEAL) and a message for the log. It never holds
// the secret.
type Error struct {
	Code    string
	Message string
	retry   bool
	after   time.Duration
}

func (e *Error) Error() string { return e.Message }

func Payload(r Request) ([]byte, error) {
	return json.Marshal(map[string]any{
		"schema": Schema,
		"kind":   r.Kind,
		"sentAt": r.Now.UTC().Format(time.RFC3339),
		"client": map[string]string{"name": r.Client, "version": r.Version},
		"source": r.Source,
		"report": r.Report,
	})
}

func Send(ctx context.Context, hc *http.Client, p Policy, r Request) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, p.Total)
	defer cancel()
	plain, err := Payload(r)
	if err != nil {
		return Result{}, &Error{Code: "PAYLOAD", Message: err.Error()}
	}
	body, err := Seal(r.Key, r.Secret, plain)
	if err != nil {
		return Result{}, &Error{Code: "SEAL", Message: "cannot seal the report: " + err.Error()}
	}
	var last *Error
	for attempt := 1; ; attempt++ {
		res, err := post(ctx, hc, r, body)
		if err == nil {
			return res, nil
		}
		last = err
		if !err.retry || attempt >= p.Attempts {
			return Result{}, last
		}
		wait := p.Pause
		if err.after > 0 {
			if err.after > p.MaxRetryAfter {
				return Result{}, last
			}
			wait = err.after
		}
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) < wait {
			return Result{}, last
		}
		select {
		case <-ctx.Done():
			return Result{}, last
		case <-time.After(wait):
		}
	}
}

func post(ctx context.Context, hc *http.Client, r Request, body []byte) (Result, *Error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.URL, "/")+"/api/v1/reports", bytes.NewReader(body))
	if err != nil {
		return Result{}, &Error{Code: "NETWORK", Message: "invalid Zosia URL: " + err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+r.Secret)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", r.Client+"/"+r.Version)
	resp, err := hc.Do(req)
	if err != nil {
		return Result{}, &Error{Code: "NETWORK", Message: "cannot reach Zosia: " + unwrapped(err), retry: ctx.Err() == nil}
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		var res Result
		if err := json.Unmarshal(data, &res); err != nil {
			return Result{}, &Error{Code: "BAD_RESPONSE", Message: "Zosia answered with something that is not a result"}
		}
		return res, nil
	}
	var e struct{ Code, Message string }
	_ = json.Unmarshal(data, &e)
	if e.Code == "" {
		e.Code = "HTTP_" + strconv.Itoa(resp.StatusCode)
	}
	if e.Message == "" {
		e.Message = fmt.Sprintf("Zosia answered %s", resp.Status)
	}
	out := &Error{Code: e.Code, Message: e.Message, retry: resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500}
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
		out.after = time.Duration(s) * time.Second
	}
	return Result{}, out
}

// unwrapped drops net/http's "Post <url>:" prefix from a transport error.
func unwrapped(err error) string {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) && ue.Unwrap() != nil {
		return ue.Unwrap().Error()
	}
	return err.Error()
}
