package grype

import (
	"os"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile("../../testdata/grype/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseSample(t *testing.T) {
	res, err := Parse(load(t, "sample.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Findings) != 3 {
		t.Fatalf("findings = %d, want 3 (duplicate removed)", len(res.Findings))
	}
	if want := time.Date(2026, 11, 7, 6, 18, 48, 0, time.UTC); !res.DBBuilt.Equal(want) {
		t.Errorf("DBBuilt = %v", res.DBBuilt)
	}
	if res.DBSchema != "v6.1.10" {
		t.Errorf("schema = %q", res.DBSchema)
	}
	if res.Identity.ManifestDigest != "sha256:2f41790747d893c2a74b2c976544d35528c5d2c0f8aff4289bac324e56d2bfb6" || len(res.Identity.RepoDigests) != 1 {
		t.Errorf("identity = %+v", res.Identity)
	}

	brace := res.Findings[0]
	if brace.Package != "brace-expansion" || brace.Severity != "high" || brace.FixState != "fixed" || brace.FixVersions[0] != "2.1.6" {
		t.Errorf("brace = %+v", brace)
	}
	if len(brace.Aliases) != 1 || brace.Aliases[0] != "CVE-2026-102278" || brace.EPSS != 0.00491 || brace.KnownExploited {
		t.Errorf("brace signals = %+v", brace)
	}
	if len(brace.CVSS) != 1 || brace.CVSS[0].BaseScore != 7.5 {
		t.Errorf("brace cvss = %+v", brace.CVSS)
	}

	node := res.Findings[1]
	if !node.KnownExploited || node.EPSS != 0.82641 || node.Type != "binary" {
		t.Errorf("KEV/EPSS from related vulnerability not picked up: %+v", node)
	}

	if crypto := res.Findings[2]; crypto.Severity != "unknown" {
		t.Errorf("unrecognized severity must become unknown, got %q", crypto.Severity)
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, in := range []string{"not json", `{"matches": []}`, `{"descriptor": {"name": "grype", "db": {"status": {"built": ""}}}}`} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("Parse(%q) succeeded, want error", in)
		}
	}
}

func TestParseSBOMCountsPackagesNotFiles(t *testing.T) {
	inv, err := ParseSBOM(load(t, "sample.cdx.json"))
	if err != nil {
		t.Fatal(err)
	}
	if inv.Packages != 3 || inv.Cargo != 2 {
		t.Fatalf("inventory = %+v, want 3 packages, 2 cargo", inv)
	}
	want := map[string]int{"/package-lock.json": 1, "/web/Cargo.lock": 2, "/Cargo.lock": 1}
	if len(inv.Locations) != len(want) {
		t.Fatalf("locations = %v, want %v", inv.Locations, want)
	}
	for k, v := range want {
		if inv.Locations[k] != v {
			t.Fatalf("locations = %v, want %v", inv.Locations, want)
		}
	}
	if _, err := ParseSBOM([]byte(`{"components": []}`)); err == nil {
		t.Fatal("a document without bomFormat CycloneDX must be rejected")
	}
}

func TestParseRejectsInvalidImageSource(t *testing.T) {
	in := `{"source": {"type": "image", "target": "not-an-object"}, "descriptor": {"name": "grype", "db": {"status": {"built": "2026-11-07T06:18:48Z"}}}}`
	if _, err := Parse([]byte(in)); err == nil || !strings.Contains(err.Error(), "invalid image source") {
		t.Fatalf("err = %v", err)
	}
}
