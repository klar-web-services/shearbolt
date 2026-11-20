package zosia

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var updateFixture = flag.Bool("update-fixture", false, "rewrite testdata/zosia/envelope.json")

const fixtureSecret = "zosia_fixture_secret_do_not_use"

var fixtureDir = filepath.Join("..", "..", "testdata", "zosia")

func fixtureKey(t *testing.T) (Key, []byte) {
	t.Helper()
	priv, err := kem.DeriveKeyPair(bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := priv.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	k, err := ParseKey("fixture:" + base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return k, raw
}

func TestFixtureOpens(t *testing.T) {
	k, _ := fixtureKey(t)
	path := filepath.Join(fixtureDir, "envelope.json")
	if *updateFixture {
		run := json.RawMessage(`{"schemaVersion":"1","runId":"fixture-run","project":"fixture","verdict":"block","exitCode":1,"targets":[],"findings":[],"exceptions":[],"warnings":[],"errors":[]}`)
		plain, err := Payload(Request{Client: "shearbolt", Version: "1.4.0", Kind: "shearbolt", Now: time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC),
			Source: map[string]any{"ci": map[string]any{"provider": "github", "server": "https://github.com", "repository": "example/fixture",
				"ref": "refs/heads/main", "branch": "main", "defaultBranch": "main", "sha": "0000000000000000000000000000000000000000",
				"event": "push", "pullRequest": nil, "runUrl": "https://github.com/example/fixture/actions/runs/1/attempts/1",
				"workflow": "Image", "job": "image"}, "configPath": "shearbolt.yaml", "targets": []string{}},
			Report: run})
		if err != nil {
			t.Fatal(err)
		}
		env, err := Seal(k, fixtureSecret, plain)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(env, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, priv := testKey(t)
	plain, err := open(t, priv, fixtureSecret, bytes.TrimSpace(env))
	if err != nil {
		t.Fatalf("the fixture does not open: %v", err)
	}
	var p map[string]any
	if err := json.Unmarshal(plain, &p); err != nil || p["schema"] != Schema || p["kind"] != "shearbolt" {
		t.Fatalf("payload %s, %v", plain, err)
	}
}
