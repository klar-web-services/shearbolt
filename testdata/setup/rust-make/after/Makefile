CARGO ?= cargo

.PHONY: dev test release

# Debug build for local work
dev:
	$(CARGO) build

test:
	$(CARGO) test --locked

# Release binary used by CI
release: test
	$(CARGO) auditable build --release --locked
	@ls -lh target/release/demo
