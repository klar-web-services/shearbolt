package grype

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/klar-web-services/shearbolt/internal/model"
)

// Result is what Shearbolt takes from one Grype JSON report.
type Result struct {
	Findings []model.Finding
	DBBuilt  time.Time
	DBSchema string
	DBSource string
	Identity model.Identity
}

// Inventory counts the packages in a CycloneDX SBOM written by Grype.
type Inventory struct {
	Packages int // components that are neither files nor the operating system
	Cargo    int // components with a pkg:cargo purl
	// Locations counts packages per file they were read from (syft:location:<n>:path),
	// as scan-root-relative paths with a leading slash, e.g. "/web/pnpm-lock.yaml".
	Locations map[string]int
}

type vulnerability struct {
	ID          string   `json:"id"`
	Severity    string   `json:"severity"`
	Description string   `json:"description"`
	URLs        []string `json:"urls"`
	Fix         struct {
		Versions []string `json:"versions"`
		State    string   `json:"state"`
	} `json:"fix"`
	KnownExploited []json.RawMessage `json:"knownExploited"`
	EPSS           []struct {
		EPSS float64 `json:"epss"`
	} `json:"epss"`
	CVSS []struct {
		Source  string `json:"source"`
		Version string `json:"version"`
		Vector  string `json:"vector"`
		Metrics struct {
			BaseScore float64 `json:"baseScore"`
		} `json:"metrics"`
	} `json:"cvss"`
}

type document struct {
	Matches []struct {
		Vulnerability          vulnerability   `json:"vulnerability"`
		RelatedVulnerabilities []vulnerability `json:"relatedVulnerabilities"`
		Artifact               struct {
			Name      string `json:"name"`
			Version   string `json:"version"`
			Type      string `json:"type"`
			PURL      string `json:"purl"`
			Locations []struct {
				Path string `json:"path"`
			} `json:"locations"`
		} `json:"artifact"`
	} `json:"matches"`
	Source struct {
		Type   string          `json:"type"`
		Target json.RawMessage `json:"target"`
	} `json:"source"`
	Descriptor struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		DB      struct {
			Status struct {
				Built         string `json:"built"`
				SchemaVersion string `json:"schemaVersion"`
				From          string `json:"from"`
			} `json:"status"`
		} `json:"db"`
	} `json:"descriptor"`
}

var locationProperty = regexp.MustCompile(`^syft:location:\d+:path$`)

var knownSeverities = []string{"critical", "high", "medium", "low", "negligible"}

// Parse converts a Grype JSON report into findings. Unknown or malformed output is an error.
func Parse(data []byte) (*Result, error) {
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("unparseable Grype output: %w", err)
	}
	if doc.Descriptor.Name != "grype" {
		return nil, errors.New("unparseable Grype output: the grype descriptor is missing")
	}
	built, err := time.Parse(time.RFC3339, doc.Descriptor.DB.Status.Built)
	if err != nil {
		return nil, errors.New("Grype output has no valid vulnerability database build time")
	}
	res := &Result{
		DBBuilt:  built.UTC(),
		DBSchema: doc.Descriptor.DB.Status.SchemaVersion,
		DBSource: doc.Descriptor.DB.Status.From,
	}
	if doc.Source.Type == "image" && len(doc.Source.Target) > 0 {
		var img struct {
			ImageID        string   `json:"imageID"`
			ManifestDigest string   `json:"manifestDigest"`
			RepoDigests    []string `json:"repoDigests"`
		}
		if err := json.Unmarshal(doc.Source.Target, &img); err != nil {
			return nil, errors.New("unparseable Grype output: invalid image source")
		}
		res.Identity = model.Identity{ImageID: img.ImageID, ManifestDigest: img.ManifestDigest, RepoDigests: img.RepoDigests}
	}
	seen := map[string]bool{}
	for _, m := range doc.Matches {
		v := m.Vulnerability
		if v.ID == "" || m.Artifact.Name == "" {
			return nil, errors.New("unparseable Grype output: a match has no vulnerability ID or package name")
		}
		locations := make([]string, 0, len(m.Artifact.Locations))
		for _, l := range m.Artifact.Locations {
			locations = append(locations, l.Path)
		}
		key := strings.Join([]string{v.ID, m.Artifact.Name, m.Artifact.Version, m.Artifact.Type, strings.Join(locations, ",")}, "\x00")
		if seen[key] {
			continue
		}
		seen[key] = true
		f := model.Finding{
			ID:          v.ID,
			Aliases:     []string{},
			Package:     m.Artifact.Name,
			Version:     m.Artifact.Version,
			Type:        m.Artifact.Type,
			PURL:        m.Artifact.PURL,
			Locations:   locations,
			Severity:    normalizeSeverity(v.Severity),
			FixState:    v.Fix.State,
			FixVersions: v.Fix.Versions,
			URLs:        v.URLs,
			Description: v.Description,
		}
		if f.FixState == "" {
			f.FixState = "unknown"
		}
		if f.FixVersions == nil {
			f.FixVersions = []string{}
		}
		for _, x := range append([]vulnerability{v}, m.RelatedVulnerabilities...) {
			if len(x.KnownExploited) > 0 {
				f.KnownExploited = true
			}
			for _, e := range x.EPSS {
				f.EPSS = max(f.EPSS, e.EPSS)
			}
			if x.ID != "" && x.ID != v.ID && !slices.Contains(f.Aliases, x.ID) {
				f.Aliases = append(f.Aliases, x.ID)
			}
		}
		for _, c := range v.CVSS {
			f.CVSS = append(f.CVSS, model.CVSS{Source: c.Source, Version: c.Version, Vector: c.Vector, BaseScore: c.Metrics.BaseScore})
		}
		res.Findings = append(res.Findings, f)
	}
	return res, nil
}

// ParseSBOM counts packages in a CycloneDX SBOM. File and operating-system components are not packages.
func ParseSBOM(data []byte) (Inventory, error) {
	var doc struct {
		BOMFormat  string `json:"bomFormat"`
		Components []struct {
			Type       string `json:"type"`
			PURL       string `json:"purl"`
			Properties []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"properties"`
		} `json:"components"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return Inventory{}, fmt.Errorf("unparseable SBOM from Grype: %w", err)
	}
	if doc.BOMFormat != "CycloneDX" {
		return Inventory{}, errors.New("unparseable SBOM from Grype: not a CycloneDX document")
	}
	inv := Inventory{Locations: map[string]int{}}
	for _, c := range doc.Components {
		if c.Type == "file" || c.Type == "operating-system" {
			continue
		}
		inv.Packages++
		if strings.HasPrefix(c.PURL, "pkg:cargo/") {
			inv.Cargo++
		}
		for _, p := range c.Properties {
			if locationProperty.MatchString(p.Name) {
				inv.Locations[p.Value]++
			}
		}
	}
	return inv, nil
}

func normalizeSeverity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if slices.Contains(knownSeverities, s) {
		return s
	}
	return "unknown"
}
