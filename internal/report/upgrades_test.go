package report

import (
	"slices"
	"testing"

	"github.com/klar-web-services/shearbolt/internal/model"
)

// blockF is one blocking finding of a package.
func blockF(typ, pkg, version, fixState string, fixes ...string) model.Finding {
	return model.Finding{
		ID: "GHSA-" + pkg + "-" + version, Type: typ, Package: pkg, Version: version,
		FixState: fixState, FixVersions: fixes,
		Decision: model.Decision{Action: model.Block, Reasons: []string{"fix available"}},
	}
}

func fixed(typ, pkg, version string, fixes ...string) model.Finding {
	return blockF(typ, pkg, version, "fixed", fixes...)
}

// lodashFindings are the real Grype 0.121.2 matches for testdata/projects/npm-vulnerable
// on the 2026-11-08 database (fix versions only; IDs are placeholders).
func lodashFindings() []model.Finding {
	return []model.Finding{
		fixed("npm", "lodash", "4.17.20", "4.17.21"),
		fixed("npm", "lodash", "4.17.20", "4.17.21"),
		fixed("npm", "lodash", "4.17.20", "4.18.0"),
		fixed("npm", "lodash", "4.17.20", "4.17.23"),
	}
}

func TestUpgrades(t *testing.T) {
	tests := []struct {
		name    string
		blocked []model.Finding
		want    []Upgrade
	}{
		{
			name:    "lodash takes the highest fix version over all its findings",
			blocked: lodashFindings(),
			want:    []Upgrade{{Type: "npm", Package: "lodash", Version: "4.18.0", Count: 4}},
		},
		{
			name: "a not-fixed finding suppresses its package only",
			blocked: append(lodashFindings(),
				blockF("npm", "lodash", "4.17.20", "not-fixed"),
				fixed("npm", "minimist", "1.2.5", "1.2.6"),
			),
			want: []Upgrade{{Type: "npm", Package: "minimist", Version: "1.2.6", Count: 1}},
		},
		{
			name:    "fixed without any fix version suppresses the package",
			blocked: []model.Finding{fixed("npm", "a", "1.0.0", "1.0.1"), fixed("npm", "a", "1.0.0")},
		},
		{
			name:    "wont-fix and unknown fix states suppress the package",
			blocked: []model.Finding{fixed("npm", "a", "1.0.0", "1.0.1"), blockF("npm", "a", "1.0.0", "wont-fix", "1.0.2"), blockF("npm", "b", "1.0.0", "unknown", "1.0.2")},
		},
		{
			name:    "deb packages are not eligible",
			blocked: []model.Finding{fixed("deb", "openssl", "3.0.14-1~deb12u1", "3.0.15-1~deb12u1")},
		},
		{
			name:    "other ecosystems are not eligible even with semver versions",
			blocked: []model.Finding{fixed("go-module", "golang.org/x/net", "v0.1.0", "v0.17.0"), fixed("python", "requests", "2.0.0", "2.31.0"), fixed("", "x", "1.0.0", "1.0.1")},
		},
		{
			name:    "numeric, not lexical",
			blocked: []model.Finding{fixed("npm", "a", "4.8.0", "4.9.0"), fixed("npm", "a", "4.8.0", "4.10.0")},
			want:    []Upgrade{{Type: "npm", Package: "a", Version: "4.10.0", Count: 2}},
		},
		{
			name:    "several fix versions on one finding",
			blocked: []model.Finding{fixed("npm", "a", "2.3.0", "2.3.4", "3.0.8")},
			want:    []Upgrade{{Type: "npm", Package: "a", Version: "3.0.8", Count: 1}},
		},
		{
			name:    "the same package at two installed versions sums its findings",
			blocked: []model.Finding{fixed("npm", "lodash", "4.17.20", "4.17.21"), fixed("npm", "lodash", "4.17.15", "4.17.19", "4.17.21"), fixed("npm", "lodash", "4.17.15", "4.17.21")},
			want:    []Upgrade{{Type: "npm", Package: "lodash", Version: "4.17.21", Count: 3}},
		},
		{
			name:    "a fix version that is not semver suppresses the package",
			blocked: []model.Finding{fixed("npm", "lodash", "4.17.20", "4.17.21"), fixed("npm", "lodash", "4.17.20", "4.17.x")},
		},
		{
			name:    "an installed version that is not semver suppresses the package",
			blocked: []model.Finding{fixed("npm", "a", "latest", "1.0.1")},
		},
		{
			name:    "maximum fix not above the installed version",
			blocked: []model.Finding{fixed("npm", "a", "1.2.0", "1.1.0")},
		},
		{
			name:    "maximum fix equal to the installed version",
			blocked: []model.Finding{fixed("npm", "a", "1.2.0", "1.2.0")},
		},
		{
			name:    "maximum fix above one installed version but not the other",
			blocked: []model.Finding{fixed("npm", "a", "1.0.0", "1.5.0"), fixed("npm", "a", "2.0.0", "1.5.0")},
		},
		{
			name:    "rust crate",
			blocked: []model.Finding{fixed("rust-crate", "smallvec", "1.6.0", "1.6.1"), fixed("rust-crate", "smallvec", "1.6.0", "0.6.14", "1.6.1")},
			want:    []Upgrade{{Type: "rust-crate", Package: "smallvec", Version: "1.6.1", Count: 2}},
		},
		{
			name:    "a release sorts above its pre-release",
			blocked: []model.Finding{fixed("npm", "a", "0.9.0", "1.0.0-rc.1", "1.0.0")},
			want:    []Upgrade{{Type: "npm", Package: "a", Version: "1.0.0", Count: 1}},
		},
		{
			name:    "numeric pre-release identifiers compare numerically",
			blocked: []model.Finding{fixed("npm", "a", "0.9.0", "1.0.0-alpha.10", "1.0.0-alpha.9")},
			want:    []Upgrade{{Type: "npm", Package: "a", Version: "1.0.0-alpha.10", Count: 1}},
		},
		{
			name:    "an installed pre-release is below its release",
			blocked: []model.Finding{fixed("npm", "a", "1.0.0-rc.1", "1.0.0")},
			want:    []Upgrade{{Type: "npm", Package: "a", Version: "1.0.0", Count: 1}},
		},
		{
			name:    "leading v is accepted and kept as reported",
			blocked: []model.Finding{fixed("npm", "a", "v1.0.0", "v1.0.1")},
			want:    []Upgrade{{Type: "npm", Package: "a", Version: "v1.0.1", Count: 1}},
		},
		{
			name:    "a maximum with two spellings is ambiguous",
			blocked: []model.Finding{fixed("npm", "a", "1.0.0", "1.0.1"), fixed("npm", "a", "1.0.0", "v1.0.1")},
		},
		{
			name:    "a maximum that differs only in build metadata is ambiguous",
			blocked: []model.Finding{fixed("npm", "a", "1.0.0", "1.0.1+a", "1.0.1+b")},
		},
		{
			name:    "build metadata is kept when it is the only spelling",
			blocked: []model.Finding{fixed("npm", "a", "1.0.0", "1.0.1+build.7", "1.0.0+x")},
			want:    []Upgrade{{Type: "npm", Package: "a", Version: "1.0.1+build.7", Count: 1}},
		},
		{
			name:    "the same name in two ecosystems is two packages",
			blocked: []model.Finding{fixed("npm", "same", "1.0.0", "1.0.1"), fixed("rust-crate", "same", "1.0.0", "1.0.2"), blockF("rust-crate", "other", "1.0.0", "not-fixed")},
			want:    []Upgrade{{Type: "npm", Package: "same", Version: "1.0.1", Count: 1}, {Type: "rust-crate", Package: "same", Version: "1.0.2", Count: 1}},
		},
		{
			name: "sorted by count, then package",
			blocked: []model.Finding{
				fixed("npm", "zeta", "1.0.0", "1.0.1"),
				fixed("npm", "beta", "1.0.0", "1.0.1"),
				fixed("npm", "alpha", "1.0.0", "1.0.1"),
				fixed("npm", "beta", "1.0.0", "1.0.2"),
			},
			want: []Upgrade{
				{Type: "npm", Package: "beta", Version: "1.0.2", Count: 2},
				{Type: "npm", Package: "alpha", Version: "1.0.1", Count: 1},
				{Type: "npm", Package: "zeta", Version: "1.0.1", Count: 1},
			},
		},
		{
			name: "no findings",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Upgrades(tt.blocked)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("Upgrades() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestUpgradesIgnoresWarnedAndExcepted(t *testing.T) {
	warned := blockF("npm", "lodash", "4.17.20", "not-fixed")
	warned.Decision.Action = model.Warn
	excepted := fixed("npm", "lodash", "4.17.20", "9.0.0")
	excepted.Decision.Action = model.Excepted
	blocked, _, _ := Split(append([]model.Finding{warned, excepted}, lodashFindings()...))
	got := Upgrades(blocked)
	want := []Upgrade{{Type: "npm", Package: "lodash", Version: "4.18.0", Count: 4}}
	if !slices.Equal(got, want) {
		t.Fatalf("Upgrades() = %+v, want %+v", got, want)
	}
}

func TestParseSemver(t *testing.T) {
	valid := []string{
		"0.0.0", "1.2.3", "v1.2.3", "10.20.30", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-0.3.7",
		"1.0.0-x.7.z.92", "1.0.0-x-y-z.--", "1.0.0+20130313144700", "1.0.0-beta+exp.sha.5114f85",
		"1.0.0+21AF26D3----117B344092BD", "1.0.0-rc.1+build.1", "99999999999999999999999.0.0",
	}
	for _, s := range valid {
		if _, ok := parseSemver(s); !ok {
			t.Errorf("parseSemver(%q) failed, want ok", s)
		}
	}
	invalid := []string{
		"", "v", "1", "1.2", "1.2.3.4", "01.2.3", "1.02.3", "1.2.03", "1.2.3-", "1.2.3+", "1.2.3-01",
		"1.2.3-a..b", "1.2.3+a..b", "1.2.3-a_b", "4.17.x", "x.y.z", " 1.2.3", "1.2.3 ", "vv1.2.3",
		"V1.2.3", "=1.2.3", "^1.2.3", "1.2.3-1~deb12u1", "3.0.15-1~deb12u1", "1.2.3-α", "-1.2.3",
		"1.-2.3", "+1.2.3", "1.2.3\n", "1..3", "1.2.",
	}
	for _, s := range invalid {
		if v, ok := parseSemver(s); ok {
			t.Errorf("parseSemver(%q) = %+v, want failure", s, v)
		}
	}
}

func TestCompareSemver(t *testing.T) {
	// Ascending by semver 2.0 precedence (section 11 of the specification, plus core cases).
	ordered := []string{
		"0.0.1", "0.1.0", "0.9.9", "1.0.0-0", "1.0.0-1", "1.0.0-2", "1.0.0-10", "1.0.0-A", "1.0.0-a",
		"1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.9", "1.0.0-alpha.10", "1.0.0-alpha.beta",
		"1.0.0-beta", "1.0.0-beta.2", "1.0.0-beta.11", "1.0.0-rc.1", "1.0.0", "1.0.1", "1.9.0",
		"1.10.0", "2.0.0", "10.0.0", "99999999999999999999.0.0", "100000000000000000000.0.0",
	}
	for i := range ordered {
		for j := range ordered {
			a, okA := parseSemver(ordered[i])
			b, okB := parseSemver(ordered[j])
			if !okA || !okB {
				t.Fatalf("parse %q, %q failed", ordered[i], ordered[j])
			}
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := sign(compareSemver(a, b)); got != want {
				t.Errorf("compareSemver(%q, %q) = %d, want %d", ordered[i], ordered[j], got, want)
			}
		}
	}
	equal := [][2]string{{"1.0.0", "v1.0.0"}, {"1.0.0+a", "1.0.0"}, {"1.0.0+a", "1.0.0+b"}, {"1.0.0-rc.1+x", "v1.0.0-rc.1"}}
	for _, p := range equal {
		a, _ := parseSemver(p[0])
		b, _ := parseSemver(p[1])
		if got := compareSemver(a, b); got != 0 {
			t.Errorf("compareSemver(%q, %q) = %d, want 0", p[0], p[1], got)
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}
