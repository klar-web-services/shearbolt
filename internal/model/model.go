package model

import (
	"time"

	"github.com/klar-web-services/shearbolt/internal/config"
)

type Action string

const (
	Block    Action = "block"
	Warn     Action = "warn"
	Excepted Action = "excepted"
)

type Decision struct {
	Action    Action   `json:"action"`
	Reasons   []string `json:"reasons"`
	Exception string   `json:"exception,omitempty"`
}

type CVSS struct {
	Source    string  `json:"source,omitempty"`
	Version   string  `json:"version"`
	Vector    string  `json:"vector"`
	BaseScore float64 `json:"baseScore"`
}

type Finding struct {
	ID             string   `json:"id"`
	Aliases        []string `json:"aliases"`
	Target         string   `json:"target"`
	Image          string   `json:"image,omitempty"`
	Package        string   `json:"package"`
	Version        string   `json:"version"`
	Type           string   `json:"type"`
	PURL           string   `json:"purl,omitempty"`
	Locations      []string `json:"locations"`
	Severity       string   `json:"severity"`
	CVSS           []CVSS   `json:"cvss,omitempty"`
	EPSS           float64  `json:"epss"`
	KnownExploited bool     `json:"knownExploited"`
	FixState       string   `json:"fixState"`
	FixVersions    []string `json:"fixVersions"`
	URLs           []string `json:"urls,omitempty"`
	Description    string   `json:"description,omitempty"`
	Decision       Decision `json:"decision"`
}

type Identity struct {
	SHA256         string   `json:"sha256,omitempty"`
	ImageID        string   `json:"imageId,omitempty"`
	ManifestDigest string   `json:"manifestDigest,omitempty"`
	RepoDigests    []string `json:"repoDigests,omitempty"`
}

type ImageResult struct {
	Ref      string   `json:"ref"`
	Platform string   `json:"platform,omitempty"`
	Services []string `json:"services"`
	Line     int      `json:"line"`
	Status   string   `json:"status"` // scanned | skipped | error
	Note     string   `json:"note,omitempty"`
	Identity Identity `json:"identity"`
	Packages int      `json:"packages"`
	Raw      []string `json:"raw,omitempty"`
}

type TargetResult struct {
	Name     string        `json:"name"`
	Kind     string        `json:"kind"` // image | source | dir | file | compose
	Ref      string        `json:"ref"`
	Exclude  []string      `json:"exclude,omitempty"`
	Status   string        `json:"status"` // scanned | skipped | error
	Note     string        `json:"note,omitempty"`
	Identity Identity      `json:"identity"`
	Packages int           `json:"packages"`
	Raw      []string      `json:"raw,omitempty"`
	Images   []ImageResult `json:"images,omitempty"`
}

type Database struct {
	Built         time.Time `json:"built"`
	SchemaVersion string    `json:"schemaVersion"`
	Source        string    `json:"source"`
}

type ExceptionStatus struct {
	ID      string `json:"id"`
	Package string `json:"package"`
	Target  string `json:"target,omitempty"`
	Expires string `json:"expires"`
	Status  string `json:"status"` // applied | unused | expired
}

type Diagnostic struct {
	Code    string `json:"code"`
	Target  string `json:"target,omitempty"`
	Message string `json:"message"`
}

type Run struct {
	SchemaVersion string            `json:"schemaVersion"`
	RunID         string            `json:"runId"`
	Project       string            `json:"project"`
	StartedAt     time.Time         `json:"startedAt"`
	FinishedAt    time.Time         `json:"finishedAt"`
	CIMode        bool              `json:"ciMode"`
	Shearbolt     string            `json:"shearboltVersion"`
	Grype         string            `json:"grypeVersion"`
	Database      *Database         `json:"database"`
	DBArchive     string            `json:"dbArchive,omitempty"`
	Policy        config.Policy     `json:"policy"`
	Targets       []TargetResult    `json:"targets"`
	Findings      []Finding         `json:"findings"`
	Exceptions    []ExceptionStatus `json:"exceptions"`
	Warnings      []Diagnostic      `json:"warnings"`
	Errors        []Diagnostic      `json:"errors"`
	Verdict       string            `json:"verdict"` // pass | block | error
	ExitCode      int               `json:"exitCode"`
	EvidenceDir   string            `json:"-"`
}
