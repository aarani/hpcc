# Repo-level Makefile. Today this exists for one job: regenerate the
# protobuf Go bindings. Two proto trees with different layouts:
#
#   internal/protocol/*.proto  →  internal/protocol/gen/*.pb.go
#       (go_package = "./gen"; scheduler/worker/daemon wire schema)
#
#   proto/agent/agent.proto    →  proto/agent/agent{,_grpc}.pb.go
#       (go_package = ".../proto/agent;agent"; host↔in-VM agent stream)
#
# Both invocations require protoc + protoc-gen-go + protoc-gen-go-grpc
# on PATH. Install with:
#   go install google.golang.org/protobuf/cmd/protoc-gen-go@latest
#   go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest
# (protoc itself from your package manager; libprotoc >= 3.20.)

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