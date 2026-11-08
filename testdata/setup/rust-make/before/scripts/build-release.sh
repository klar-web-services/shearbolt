#!/bin/sh
set -eu
# macOS runners build here; Linux uses `make release`.
cargo build --release --locked
strip target/release/demo || true
