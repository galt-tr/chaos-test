# chaos-test: BSV alert-system scenario harness
N              ?= 3
SV             ?= 2
SVNODE_IMAGE   ?= docker.io/bitcoinsv/bitcoin-sv:1.2.2
TERANODE_REF   ?= fix/1422-height-anchored-freeze
TERANODE_TAG   ?= pr1764
TERANODE_REPO  ?= https://github.com/bsv-blockchain/teranode
ALERT_SYSTEM_REF  ?= v0.1.17
ALERT_SYSTEM_REPO ?= https://github.com/bsv-blockchain/go-alert-system
STACK          := stack
COMPOSE        := podman compose -f $(STACK)/compose.yaml
# compose profiles started by `make up`; e.g. make up PROFILES="--profile tools"
PROFILES       ?= --profile tools --profile arcade --profile merkle --profile wallet

.PHONY: gen build build-tools build-teranode build-alert-system up down wait status logs tools test tidy

gen: ## regenerate stack/compose.yaml + config for N teranodes and SV SV nodes (keys are kept)
	go run ./cmd/gen -n $(N) -sv $(SV) -out $(STACK) -teranode-image localhost/teranode-chaos:$(TERANODE_TAG) -svnode-image $(SVNODE_IMAGE)

build: build-tools build-alert-system build-teranode ## build all images

build-tools: ## alertctl + stackctl image
	podman build -t localhost/chaos-test:local .

build-teranode: ## clone TERANODE_REF, apply the alert-p2p settings patch, build the image
	@if [ ! -d $(STACK)/vendor/teranode/.git ]; then \
	  git clone --depth 1 --branch $(TERANODE_REF) $(TERANODE_REPO) $(STACK)/vendor/teranode; fi
	cd $(STACK)/vendor/teranode && git diff --quiet || (echo "vendor/teranode has local changes (patch already applied?) - continuing"; true)
	cd $(STACK)/vendor/teranode && for p in ../../patches/teranode/*.patch; do \
	  if git apply --3way --check $$p 2>/dev/null; then git apply --3way $$p && echo "applied $$(basename $$p)"; \
	  elif git apply --reverse --check $$p 2>/dev/null; then echo "already applied $$(basename $$p)"; \
	  else echo "PATCH $$(basename $$p) DOES NOT APPLY to $(TERANODE_REF) - fix stack/patches first"; exit 1; fi; done
	cd $(STACK)/vendor/teranode && podman build -t localhost/teranode-chaos:$(TERANODE_TAG) \
	  --build-arg BASE_IMG=ghcr.io/bsv-blockchain/teranode-base:build-latest \
	  --build-arg RUN_IMG=ghcr.io/bsv-blockchain/teranode-base:run-latest \
	  --build-arg GIT_SHA=$$(git rev-parse HEAD) --build-arg GIT_COMMIT=$$(git rev-parse --short HEAD) \
	  --build-arg GIT_VERSION=$(TERANODE_TAG)-chaos --build-arg BUILD_JOBS=10 .

build-alert-system: ## go-alert-system hub image (our Dockerfile, fully-qualified base images)
	@if [ ! -d $(STACK)/vendor/go-alert-system/.git ]; then \
	  git clone --depth 1 --branch $(ALERT_SYSTEM_REF) $(ALERT_SYSTEM_REPO) $(STACK)/vendor/go-alert-system; fi
	podman build -t localhost/go-alert-system:$(ALERT_SYSTEM_REF) -f $(STACK)/docker/go-alert-system.Dockerfile $(STACK)/vendor/go-alert-system

up: ## start the stack (teranodes, SV nodes + sidecars, kafka, alert hub) + profiles
	mkdir -p $(STACK)/.data/tools $(STACK)/.data/alert-system $(STACK)/.data/arcade $(STACK)/.data/merkle-service $(STACK)/.data/wallet-db
	@# the hub image runs as USER 65534; compose asks for :U on .data/alert-system but
	@# docker-compose (podman's preferred provider when installed) drops it, leaving the
	@# dir owned by root inside the userns. Do the remap ourselves, provider-independent.
	@podman unshare chown -R 65534:65534 $(STACK)/.data/alert-system
	@for j in $$(seq 1 $(SV)); do mkdir -p $(STACK)/.data/svnode$$j $(STACK)/.data/alert-svnode$$j; done
	cd $(STACK) && podman compose $(PROFILES) up -d

down: ## stop and remove the stack (keeps .data/)
	cd $(STACK) && podman compose $(PROFILES) down

clean: down ## also wipe chain/alert state
	podman unshare rm -rf $(STACK)/.data

wait: ## wait until every node answers (teranode health port, SV node RPC, then sidecar health best-effort)
	@for n in $$(seq 1 $(N)); do port=$$((20000 + (n-1)*2000)); \
	  until curl -sf --max-time 2 http://localhost:$$port/health >/dev/null 2>&1; do sleep 2; done; echo "teranode$$n healthy"; done
	@for j in $$(seq 1 $(SV)); do port=$$((40000 + (j-1)*1000 + 332)); \
	  until curl -sf --max-time 2 -u bitcoin:bitcoin -H 'content-type: application/json' -d '{"jsonrpc":"1.0","id":"w","method":"getblockcount","params":[]}' http://localhost:$$port >/dev/null 2>&1; do sleep 2; done; echo "svnode$$j healthy"; done
	@for j in $$(seq 1 $(SV)); do port=$$((40000 + (j-1)*1000 + 300)); i=0; \
	  until curl -sf --max-time 2 http://localhost:$$port/health >/dev/null 2>&1 || [ $$i -ge 45 ]; do sleep 2; i=$$((i+1)); done; \
	  if [ $$i -ge 45 ]; then echo "alert-svnode$$j: API not up yet (it starts after 2 alert peers connect)"; else echo "alert-svnode$$j up"; fi; done

status: ## tips and alert sequence
	$(STACK)/scripts/tips.sh

logs: ## follow all logs
	cd $(STACK) && podman compose logs -f

tools: ## shell in the tools container
	podman exec -it chaos-tools bash

test:
	CGO_ENABLED=0 go test ./cmd/... ./internal/...

tidy:
	go mod tidy

# ---- simulator layer ----------------------------------------------------------------------
.PHONY: ui sim-build sim-up sim-down sim-logs
ui: ## build the React UI into internal/api/uidist (embedded by the orchestrator)
	cd sim/ui && npm install --no-audit --no-fund && npm run build

sim-build: ## orchestrator image (UI=0 for API-only)
	podman build -t localhost/chaos-orchestrator:local --build-arg UI=$${UI:-1} -f sim/Dockerfile .

sim-up: ## start the orchestrator against the running stack (needs the podman socket)
	mkdir -p sim/.data && cd sim && podman compose up -d --force-recreate

sim-down:
	cd sim && podman compose down

sim-logs:
	podman logs -f chaos-orchestrator

reset: ## wipe chain + alert state everywhere (stack data, hub, tools log, simulator alert log/runs) and restart
	cd $(STACK) && podman compose $(PROFILES) down
	-cd sim && podman compose down
	podman unshare rm -rf $(STACK)/.data
	rm -rf sim/.data/alerts.json sim/.data/alerts.json.tmp
	$(MAKE) up wait
	$(MAKE) sim-up
