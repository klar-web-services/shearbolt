// Package testutil holds helpers shared by Shearbolt's tests.
package testutil

import (
	"os"
	"path/filepath"
	"testing"
)

const fakeGrypeScript = `#!/bin/sh
# Fake Grype for Shearbolt tests. Controlled by FAKE_GRYPE_* variables; see FakeGrype.
log="${FAKE_GRYPE_LOG:-/dev/null}"
printf '%s\n' "$@" > "$log"
if [ "$log" != /dev/null ]; then env > "$log.env"; pwd > "$log.pwd"; fi
if [ -n "$FAKE_GRYPE_CALLS" ]; then printf '%s\n' "$*" >> "$FAKE_GRYPE_CALLS"; fi
if [ "$1 $2" = "db status" ]; then
  if [ -n "$FAKE_GRYPE_STATUS" ]; then printf '%s\n' "$FAKE_GRYPE_STATUS"; fi
  exit "${FAKE_GRYPE_STATUS_EXIT:-0}"
fi
if [ "$1 $2" = "db import" ] && [ -n "$FAKE_GRYPE_IMPORT_APPEND" ]; then printf '%s' "$FAKE_GRYPE_IMPORT_APPEND" >> "$3"; fi
if [ "$1 $2" = "db import" ] && [ -n "$FAKE_GRYPE_IMPORT_EXIT" ]; then exit "$FAKE_GRYPE_IMPORT_EXIT"; fi
built=$(date -u +%Y-%m-%dT%H:%M:%SZ)
default_json='{"matches":[],"source":{"type":"directory","target":"/src"},"descriptor":{"name":"grype","version":"0.121.2","db":{"status":{"built":"'"$built"'","schemaVersion":"v6.1.10","from":"https://example.test/db.tar.zst"}}}}'
default_sbom='{"bomFormat":"CycloneDX","components":[{"type":"library","name":"left-pad","purl":"pkg:npm/left-pad@1.3.0","properties":[{"name":"syft:location:0:path","value":"/package-lock.json"}]}]}'
while [ $# -gt 0 ]; do
  case "$1" in
    -o)
      case "$2" in
        json=*) printf '%s' "${FAKE_GRYPE_JSON:-$default_json}" > "${2#json=}" ;;
        cyclonedx-json=*) printf '%s' "${FAKE_GRYPE_SBOM:-$default_sbom}" > "${2#cyclonedx-json=}" ;;
      esac
      shift ;;
    -c)
      if [ "$log" != /dev/null ]; then cp "$2" "$log.cfg"; fi
      shift ;;
  esac
  shift
done
if [ -n "$FAKE_GRYPE_STDERR" ]; then printf '%s\n' "$FAKE_GRYPE_STDERR" >&2; fi
if [ -n "$FAKE_GRYPE_SLEEP" ]; then exec sleep "$FAKE_GRYPE_SLEEP"; fi
exit "${FAKE_GRYPE_EXIT:-0}"
`

// FakeGrype writes an executable that imitates Grype's command line and returns its path.
// Variables: FAKE_GRYPE_LOG (file receiving the arguments; "<log>.env", ".pwd", ".cfg" get the
// environment, working directory and config), FAKE_GRYPE_JSON and FAKE_GRYPE_SBOM (content
// written for -o json= / -o cyclonedx-json=), FAKE_GRYPE_EXIT, FAKE_GRYPE_STDERR, FAKE_GRYPE_SLEEP.
// FAKE_GRYPE_CALLS (file receiving one line per invocation, appended), FAKE_GRYPE_STATUS and
// FAKE_GRYPE_STATUS_EXIT (stdout and exit code of "db status"; no output by default) and
// FAKE_GRYPE_IMPORT_EXIT (exit code of "db import") and FAKE_GRYPE_IMPORT_APPEND (text "db import"
// appends to the archive, as if it changed during the import).
func FakeGrype(t testing.TB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grype")
	if err := os.WriteFile(path, []byte(fakeGrypeScript), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}
