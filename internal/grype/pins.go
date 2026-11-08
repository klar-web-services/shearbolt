// Package grype installs, runs and parses the pinned Grype release.
package grype

// Version is the Grype release this Shearbolt release runs. Change it only together with Checksums.
const Version = "0.121.2"

// Checksums holds the SHA-256 of each official Grype release archive, keyed by "<os>_<arch>".
// Source: https://github.com/anchore/grype/releases/download/v0.121.2/grype_0.121.2_checksums.txt
var Checksums = map[string]string{
	"darwin_amd64": "1a748df28f72e841dea5c6a19c736b61a79c3e81dc376e0c18e1ab0d274cfe08",
	"darwin_arm64": "3633c113d81ec1460b0520e23633d72a25caae62875fd48a8b0e4f22dcc7c704",
	"linux_amd64":  "9c8965ced9df657e5e851c14021bae838802f711edd7a48f9bc3c923a8d28f12",
	"linux_arm64":  "d7d2e160255b0f087280feabf6f213cf4c45d475690f3f1fe5dc336eb1037bf0",
}

// BinaryChecksums holds the SHA-256 of the grype executable inside each release archive, keyed like Checksums.
// Computed from the verified official archives; update together with Version and Checksums.
// To recompute on a version bump, after verifying the archive against the release's checksums file:
//
//	tar -xzOf grype_<version>_<os>_<arch>.tar.gz grype | sha256sum
var BinaryChecksums = map[string]string{
	"darwin_amd64": "19efe3b5ce2270e51cced0349410495e78c8df04fe276dedca4387da98db3e87",
	"darwin_arm64": "612d4dd9d98bec6f670118836f9fb1e49689e1ef843eebfc22bdac7f3d352ea6",
	"linux_amd64":  "e668ae615893f6b1525f6ee43473fa0111a082fa128409c0982809fd75f63e86",
	"linux_arm64":  "18544168137b142f56c7b4f15b4c0c525f3df2475204316fe606b0b7a9a7c6d5",
}
