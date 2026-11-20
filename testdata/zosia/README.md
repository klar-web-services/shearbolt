# Zosia test fixture

`envelope.json` is a Shearbolt report sealed the way `internal/zosia` seals reports, for Zosia's own tests (Zosia copies it to `tests/fixtures/shearbolt-envelope.json`). TEST KEY ONLY, derived from a fixed seed; never use it anywhere else.

- Key ID: `fixture`
- Private key (base64url, RFC 9180 SerializePrivateKey): `iGYVFh1cTatr6Zu2mxgPIshusKwbiRLpWO1d0IgTV3s`
- Secret: `zosia_fixture_secret_do_not_use`

Regenerate with `go test ./internal/zosia/ -run TestFixtureOpens -update-fixture` (the ciphertext changes every time; Zosia must then copy it again).
