# chaos-test: the harness that drives bsv-regtest (the network lives in ./bsv-regtest and has
# its own Makefile; the stack targets below delegate to it with chaos-test's defaults).
STACK          := bsv-regtest
# teranode is built from source at TERANODE_REF (a branch, tag, or PR ref). EXTRA_PATCHES may
# point at a directory of *.patch files to apply on top of that checkout - e.g. a local copy of
# the legacy-bridge fix that lets the SV nodes follow the teranode chain (see the README). Empty
# by default, so the default build is vanilla upstream teranode.
TERANODE_REF   ?= main
TERANODE_TAG   ?= $(subst /,-,$(TERANODE_REF))
EXTRA_PATCHES  ?=
# INTERNAL=1 renders egress-free compose networks; left open by default. Egress-free networks
# together with the legacy bridge also need a teranode build carrying the legacy-bridge fix.
INTERNAL       ?=
STACK_VARS      = TERANODE_REF=$(TERANODE_REF) TERANODE_TAG=$(TERANODE_TAG) INTERNAL=$(INTERNAL) EXTRA_PATCHES=$(EXTRA_PATCHES) RUNTIME=$(RUNTIME)
RUNTIME        ?= $(shell command -v podman >/dev/null 2>&1 && echo podman || echo docker)
COMPOSE        ?= $(RUNTIME) compose
# Host path of the engine's API socket, bind-mounted into the orchestrator at /var/run/docker.sock
# (sim/compose.yaml) for chaos actions and container logs. Recursive on purpose, and not exported:
# `podman info` then runs only when a recipe expands $(CONTAINER_SOCKET) (sim-up, and reset through
# it), never for the other targets.
#   podman: podman info's socket (rootless /run/user/<uid>/podman/podman.sock, root /run/podman/podman.sock)
#   docker: DOCKER_HOST's unix path when set (rootless docker), else /var/run/docker.sock (Docker Desktop too)
PODMAN_SOCKET     = $(or $(shell podman info --format '{{.Host.RemoteSocket.Path}}' 2>/dev/null),$(or $(XDG_RUNTIME_DIR),/run/user/$(shell id -u))/podman/podman.sock)
DOCKER_SOCKET     = $(or $(patsubst unix://%,%,$(filter unix://%,$(DOCKER_HOST))),/var/run/docker.sock)
CONTAINER_SOCKET ?= $(if $(filter podman,$(RUNTIME)),$(PODMAN_SOCKET),$(DOCKER_SOCKET))
STACK_TARGETS  := gen build build-tools build-teranode build-alert-system build-walletd up down clean wait status logs tools test-walletd

.PHONY: $(STACK_TARGETS) test walletd-build
$(STACK_TARGETS):
	$(MAKE) -C $(STACK) $(STACK_VARS) $@

# bsv-regtest's committed compose.yaml is rendered with its own defaults (teranode main, open
# networks); the harness re-renders with its settings before every `up` so they never drift.
up: gen

walletd-build: build-walletd ## kept for muscle memory

test: ## both modules (go.work); walletd is a separate module, see test-walletd
	CGO_ENABLED=0 go test ./...

tidy: ## each module with GOWORK=off, so their go.sum files stay complete for standalone builds
	GOWORK=off go mod tidy && $(MAKE) -C $(STACK) tidy

# ---- simulator layer ----------------------------------------------------------------------
.PHONY: ui sim-build sim-up sim-down sim-logs reset
ui: ## build the React UI into internal/api/uidist (embedded by the orchestrator)
	cd sim/ui && npm install --no-audit --no-fund && npm run build

sim-build: ## orchestrator image (UI=0 for API-only)
	$(RUNTIME) build -t localhost/chaos-orchestrator:local --build-arg UI=$${UI:-1} -f sim/Dockerfile .

sim-up: ## start the orchestrator against the running network (bind-mounts the engine socket, see sim/compose.yaml)
	@sock="$(CONTAINER_SOCKET)"; \
	if [ "$(RUNTIME)" = podman ] && [ ! -S "$$sock" ]; then \
	  echo "sim-up: $$sock is not a socket; run: systemctl --user enable --now podman.socket" >&2; exit 1; \
	elif [ ! -S "$$sock" ]; then \
	  echo "sim-up: note: $$sock not found on this host (fine on Docker Desktop; otherwise set DOCKER_HOST or CONTAINER_SOCKET)" >&2; \
	fi; \
	mkdir -p sim/.data && cd sim && CONTAINER_SOCKET="$$sock" $(COMPOSE) up -d --force-recreate

sim-down:
	cd sim && $(COMPOSE) down

sim-logs:
	$(RUNTIME) logs -f chaos-orchestrator

reset: ## wipe chain + alert state everywhere (network data, hub, tools log, simulator alert log/runs) and restart
	$(MAKE) -C $(STACK) $(STACK_VARS) down
	-cd sim && $(COMPOSE) down
	$(MAKE) -C $(STACK) $(STACK_VARS) clean
	rm -rf sim/.data/alerts.json sim/.data/alerts.json.tmp
	$(MAKE) up wait
	$(MAKE) sim-up
