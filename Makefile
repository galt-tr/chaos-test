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
STACK_VARS      = TERANODE_REF=$(TERANODE_REF) TERANODE_TAG=$(TERANODE_TAG) INTERNAL=$(INTERNAL) EXTRA_PATCHES=$(EXTRA_PATCHES)
RUNTIME        ?= $(shell command -v podman >/dev/null 2>&1 && echo podman || echo docker)
COMPOSE        ?= $(RUNTIME) compose
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

sim-up: ## start the orchestrator against the running network (needs the container runtime socket)
	mkdir -p sim/.data && cd sim && $(COMPOSE) up -d --force-recreate

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
