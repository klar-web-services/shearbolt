package testutil

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Match describes one finding in a generated Grype document.
type Match struct {
	ID          string
	Package     string
	Version     string
	Type        string // default "npm"
	Severity    string // default "High"
	FixState    string // default "not-fixed"
	FixVersions []string
	KEV         bool
	EPSS        float64
	Related     []string
	Location    string // default "/package-lock.json"
}

// GrypeJSON returns a minimal Grype JSON report with the given database build time.
func GrypeJSON(built time.Time, matches ...Match) string {
	type ref struct {
		CVE string `json:"cve"`
	}
	type epss struct {
		CVE  string  `json:"cve"`
		EPSS float64 `json:"epss"`
	}
	type fix struct {
		Versions []string `json:"versions"`
		State    string   `json:"state"`
	}
	type vuln struct {
		ID             string   `json:"id"`
		Severity       string   `json:"severity"`
		Fix            fix      `json:"fix"`
		KnownExploited []ref    `json:"knownExploited,omitempty"`
		EPSS           []epss   `json:"epss,omitempty"`
		URLs           []string `json:"urls"`
	}
	type loc struct {
		Path string `json:"path"`
	}
	type artifact struct {
		Name      string `json:"name"`
		Version   string `json:"version"`
		Type      string `json:"type"`
		PURL      string `json:"purl"`
		Locations []loc  `json:"locations"`
	}
	type match struct {
		Vulnerability vuln     `json:"vulnerability"`
		Related       []vuln   `json:"relatedVulnerabilities"`
		Artifact      artifact `json:"artifact"`
	}
	out := []match{}
	for _, m := range matches {
		typ := or(m.Type, "npm")
		versions := m.FixVersions
		if versions == nil {
			versions = []string{}
		}
		v := vuln{ID: m.ID, Severity: or(m.Severity, "High"), Fix: fix{Versions: versions, State: or(m.FixState, "not-fixed")}, URLs: []string{"https://example.test/" + m.ID}}
		if m.KEV {
			v.KnownExploited = []ref{{CVE: m.ID}}
		}
		if m.EPSS > 0 {
			v.EPSS = []epss{{CVE: m.ID, EPSS: m.EPSS}}
		}
		related := []vuln{}
		for _, r := range m.Related {
			related = append(related, vuln{ID: r, Severity: v.Severity, Fix: v.Fix, URLs: []string{}})
		}
		out = append(out, match{
			Vulnerability: v,
			Related:       related,
			Artifact: artifact{
				Name: m.Package, Version: m.Version, Type: typ,
				PURL:      fmt.Sprintf("pkg:%s/%s@%s", typ, m.Package, m.Version),
				Locations: []loc{{Path: or(m.Location, "/package-lock.json")}},
			},
		})
	}
	doc := map[string]any{
		"matches": out,
		"source":  map[string]any{"type": "directory", "target": "/src"},
		"descriptor": map[string]any{
			"name": "grype", "version": "0.121.2",
			"db": map[string]any{"status": map[string]any{
				"built": built.UTC().Format(time.RFC3339), "schemaVersion": "v6.1.10", "from": "https://example.test/db.tar.zst",
			}},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// SBOMJSON returns a CycloneDX document with one library component per purl (each located in its lockfile, plus one file component).
func SBOMJSON(purls ...string) string {
	type property struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	type component struct {
		Type       string     `json:"type"`
		Name       string     `json:"name"`
		PURL       string     `json:"purl,omitempty"`
		Properties []property `json:"properties,omitempty"`
	}
	components := []component{{Type: "file", Name: "/package-lock.json"}}
	for _, p := range purls {
		path := "/package-lock.json"
		if strings.HasPrefix(p, "pkg:cargo/") {
			path = "/Cargo.lock"
		}
		components = append(components, component{Type: "library", Name: p, PURL: p,
			Properties: []property{{Name: "syft:location:0:path", Value: path}}})
	}
	b, err := json.Marshal(map[string]any{"bomFormat": "CycloneDX", "specVersion": "1.6", "components": components})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// SBOMAt is like SBOMJSON, but every package is read from the given scan-root-relative file
// (for example "/yarn.lock") instead of a default lockfile.
func SBOMAt(path string, purls ...string) string {
	type property struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	type component struct {
		Type       string     `json:"type"`
		Name       string     `json:"name"`
		PURL       string     `json:"purl,omitempty"`
		Properties []property `json:"properties,omitempty"`
	}
	components := []component{{Type: "file", Name: path}}
	for _, p := range purls {
		components = append(components, component{Type: "library", Name: p, PURL: p,
			Properties: []property{{Name: "syft:location:0:path", Value: path}}})
	}
	b, err := json.Marshal(map[string]any{"bomFormat": "CycloneDX", "specVersion": "1.6", "components": components})
	if err != nil {
		panic(err)
	}
	return string(b)
}
