# chaos-test: the harness that drives bsv-regtest (the network lives in ./bsv-regtest and has
# its own Makefile; the stack targets below delegate to it with chaos-test's defaults).
STACK          := bsv-regtest
# chaos-test builds the teranode PR under test; bsv-regtest on its own defaults to main.
TERANODE_REF   ?= fix/1422-height-anchored-freeze
TERANODE_TAG   ?= pr1764
STACK_VARS      = TERANODE_REF=$(TERANODE_REF) TERANODE_TAG=$(TERANODE_TAG)
STACK_TARGETS  := gen build build-tools build-teranode build-alert-system build-walletd up down clean wait status logs tools tidy test-walletd

.PHONY: $(STACK_TARGETS) test walletd-build
$(STACK_TARGETS):
	$(MAKE) -C $(STACK) $(STACK_VARS) $@

walletd-build: build-walletd ## kept for muscle memory

test: ## both modules (go.work); walletd is a separate module, see test-walletd
	CGO_ENABLED=0 go test ./...

# ---- simulator layer ----------------------------------------------------------------------
.PHONY: ui sim-build sim-up sim-down sim-logs reset
ui: ## build the React UI into internal/api/uidist (embedded by the orchestrator)
	cd sim/ui && npm install --no-audit --no-fund && npm run build

sim-build: ## orchestrator image (UI=0 for API-only)
	podman build -t localhost/chaos-orchestrator:local --build-arg UI=$${UI:-1} -f sim/Dockerfile .

sim-up: ## start the orchestrator against the running network (needs the podman socket)
	mkdir -p sim/.data && cd sim && podman compose up -d --force-recreate

sim-down:
	cd sim && podman compose down

sim-logs:
	podman logs -f chaos-orchestrator

reset: ## wipe chain + alert state everywhere (network data, hub, tools log, simulator alert log/runs) and restart
	$(MAKE) -C $(STACK) $(STACK_VARS) down
	-cd sim && podman compose down
	$(MAKE) -C $(STACK) $(STACK_VARS) clean
	rm -rf sim/.data/alerts.json sim/.data/alerts.json.tmp
	$(MAKE) up wait
	$(MAKE) sim-up
