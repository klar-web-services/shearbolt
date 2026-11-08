#!/bin/sh
set -eu
# macOS runners build here; Linux uses `make release`.
cargo auditable build --release --locked
strip target/release/demo || true
