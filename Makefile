# Repo-level Makefile. Three groups of targets:
#
#   proto*  — regenerate protobuf Go bindings (two proto trees):
#       internal/protocol/*.proto  →  internal/protocol/gen/*.pb.go
#           (go_package = "./gen"; scheduler/worker/daemon wire schema)
#       proto/agent/agent.proto    →  proto/agent/agent{,_grpc}.pb.go
#           (go_package = ".../proto/agent;agent"; host↔in-VM agent stream)
#     Requires protoc + protoc-gen-go + protoc-gen-go-grpc on PATH:
#       go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#       go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
#     (protoc itself from your package manager; libprotoc >= 3.20.)
#
#   build*  — host-platform compile-only check, per workspace module.
#   dist*   — cross-compiled release binaries for linux/windows/darwin.

PROTOC ?= protoc

PROTO_INTERNAL_DIR := internal/protocol
PROTO_INTERNAL_OUT := $(PROTO_INTERNAL_DIR)/gen
PROTO_INTERNAL_SRC := $(wildcard $(PROTO_INTERNAL_DIR)/*.proto)

PROTO_AGENT_DIR := proto/agent
PROTO_AGENT_SRC := $(wildcard $(PROTO_AGENT_DIR)/*.proto)

.PHONY: proto proto-internal proto-agent check-protoc

# Default proto target regenerates everything. Sub-targets exist so a
# single-tree edit (the common case) doesn't pay for the other one.
proto: proto-internal proto-agent

proto-internal: check-protoc
	@mkdir -p $(PROTO_INTERNAL_OUT)
	cd $(PROTO_INTERNAL_DIR) && $(PROTOC) \
		--go_out=./gen --go_opt=paths=source_relative \
		--go-grpc_out=./gen --go-grpc_opt=paths=source_relative \
		$(notdir $(PROTO_INTERNAL_SRC))

proto-agent: check-protoc
	cd $(PROTO_AGENT_DIR) && $(PROTOC) \
		--go_out=. --go_opt=paths=source_relative \
		--go-grpc_out=. --go-grpc_opt=paths=source_relative \
		$(notdir $(PROTO_AGENT_SRC))

check-protoc:
	@command -v $(PROTOC) >/dev/null || { \
		echo "protoc not found on PATH (set PROTOC=... or install libprotobuf)"; \
		exit 1; \
	}
	@command -v protoc-gen-go >/dev/null || { \
		echo "protoc-gen-go not found; run: go install google.golang.org/protobuf/cmd/protoc-gen-go@latest"; \
		exit 1; \
	}
	@command -v protoc-gen-go-grpc >/dev/null || { \
		echo "protoc-gen-go-grpc not found; run: go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest"; \
		exit 1; \
	}

# --- build ------------------------------------------------------------
#
# Five workspace modules (see go.work). `go build ./...` from the
# repo root only touches the main module — the others are sibling
# modules the workspace glues in, so each needs its own invocation.
# Per-module targets so a failure attributes to a specific module
# rather than disappearing into a sea of compiler output.
#
# `build` is compile-only (no install); it just confirms the tree
# builds. Use `go install ./...` from a module dir if you actually
# want binaries in $GOBIN.

GO ?= go

.PHONY: build build-main build-agent build-agent-windows build-pause build-proto build-squashfs

build: build-main build-agent build-pause build-proto build-squashfs

build-main:
	$(GO) build ./...

# Default build-agent builds for the host platform — on dev macOS that's
# the "other" stub that compiles but doesn't run. CI on a Linux worker
# host builds the real Linux PID-1 binary; the Windows worker uses
# build-agent-windows.
build-agent:
	cd agent && $(GO) build ./...

# Cross-build hpcc-agent.exe for the Windows runtime. The HvSocket
# server only exists under //go:build windows so a host-arch build
# from a Linux/macOS dev box doesn't materialise the Windows side;
# this target forces GOOS=windows so the deliverable the hcsshim
# runtime bind-mounts into containers actually contains the
# HvSocket listener.
build-agent-windows:
	cd agent && GOOS=windows GOARCH=amd64 $(GO) build -o hpcc-agent.exe .

build-pause:
	cd pause && $(GO) build ./...

build-proto:
	cd proto && $(GO) build ./...

build-squashfs:
	cd squashfs && $(GO) build ./...

# --- release / cross-platform binaries --------------------------------
#
# `dist` materialises every shippable binary for linux, windows, and
# darwin (amd64 + arm64) under dist/<os>-<arch>/. Three binaries ship:
#
#   hpcc        — repo-root CLI (scheduler client, `hpcc explain`, etc.)
#   hpcc-agent  — in-VM/Windows-container agent. Linux is the real
#                 PID-1; windows owns the HvSocket server; darwin is
#                 the "other" stub that compiles for dev parity only.
#   hpcc-pause  — container PID-1 keep-alive. Linux reaps; the rest
#                 just block on SIGTERM (reap_other.go).
#
# proto/ and squashfs/ are library-only modules and don't appear here.
# CGO is off so cross-compilation works from any host without sysroots.

DIST_DIR    ?= dist
DIST_OSES   ?= linux windows darwin
DIST_ARCHES ?= amd64 arm64

# .exe suffix on windows, empty elsewhere. Used in the per-target
# recipes below; $$ escapes for make so the shell sees a single $.
dist_ext = $(if $(filter windows,$(1)),.exe,)

.PHONY: dist dist-clean $(addprefix dist-,$(DIST_OSES))

dist: $(addprefix dist-,$(DIST_OSES))

dist-clean:
	rm -rf $(DIST_DIR)

# One phony per OS that fans out to every arch. Keeping the matrix
# expanded (rather than a pattern rule) means `make dist-linux` works
# and parallel `-j` builds get correct dependency tracking.
define DIST_OS_template
dist-$(1): $$(foreach arch,$$(DIST_ARCHES),dist-$(1)-$$(arch))
.PHONY: dist-$(1)
endef
$(foreach os,$(DIST_OSES),$(eval $(call DIST_OS_template,$(os))))

# Per-(os,arch) recipe: build the three binaries into dist/<os>-<arch>/.
# Each module gets its own `go build` since the workspace doesn't link
# sibling modules into a root `./...` build.
define DIST_OSARCH_template
dist-$(1)-$(2):
	@mkdir -p $$(DIST_DIR)/$(1)-$(2)
	GOOS=$(1) GOARCH=$(2) CGO_ENABLED=0 $$(GO) build \
		-o $$(DIST_DIR)/$(1)-$(2)/hpcc$$(call dist_ext,$(1)) .
	cd agent && GOOS=$(1) GOARCH=$(2) CGO_ENABLED=0 $$(GO) build \
		-o ../$$(DIST_DIR)/$(1)-$(2)/hpcc-agent$$(call dist_ext,$(1)) .
	cd pause && GOOS=$(1) GOARCH=$(2) CGO_ENABLED=0 $$(GO) build \
		-o ../$$(DIST_DIR)/$(1)-$(2)/hpcc-pause$$(call dist_ext,$(1)) .
.PHONY: dist-$(1)-$(2)
endef
$(foreach os,$(DIST_OSES),\
	$(foreach arch,$(DIST_ARCHES),\
		$(eval $(call DIST_OSARCH_template,$(os),$(arch)))))