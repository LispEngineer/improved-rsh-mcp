# Build and install rsh-mcp with the capability it needs.
# A plain "go build" replaces the binary and so silently removes the
# capability; "make install" rebuilds and grants it again in one step.

BIN ?= $(HOME)/.local/bin/rsh-mcp

.PHONY: all build install test

all: build

build:
	go build -o rsh-mcp .

test:
	go test ./...

install:
	mkdir -p $(dir $(BIN))
	go build -o $(BIN) .
	@echo
	@echo 'rsh-mcp must connect from a source port below 1024 (most RSH servers,'
	@echo 'including OpenVMS TCPIP$$RSH, require it). Granting the binary that one'
	@echo 'capability (cap_net_bind_service) needs root, so sudo may now ask for'
	@echo 'YOUR login password. Nothing else on the system is changed:'
	@echo '  sudo setcap cap_net_bind_service=+ep $(BIN)'
	@echo
	sudo setcap cap_net_bind_service=+ep $(BIN)
	@getcap $(BIN)
