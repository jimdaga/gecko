# Use the installed compiler; never download a replacement toolchain.
export GOTOOLCHAIN = local

.PHONY: lint lint-fix lint-fmt
lint lint-fix lint-fmt:
	@rc=0; for module in orlop platform-api controllers; do \
		$(MAKE) -C $$module $@ || rc=1; \
	done; exit $$rc

.PHONY: test
test:
	$(MAKE) -C orlop test
	$(MAKE) -C platform-api test
	$(MAKE) -C controllers test

# Keep the Go 1.26.10 baseline for CVE-2026-111943 without replacing vendor builds.
.PHONY: check-toolchain
check-toolchain:
	@test "$$(go env GOVERSION | cut -d' ' -f1)" = go1.26.10 || { \
		echo "Install Go 1.26.10 before running this target; automatic toolchain downloads are disabled." >&2; \
		exit 1; \
	}

lint lint-fix lint-fmt test: check-toolchain
