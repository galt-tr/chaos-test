# bsv-regtest: a standalone private BSV regtest network (teranodes, SV nodes, arcade,
# merkle-service, wallet, alert system). Run `make` targets from this directory.
PROJECT        := bsv-regtest
N              ?= 3
SV             ?= 2
SVNODE_IMAGE   ?= docker.io/bitcoinsv/bitcoin-sv:1.2.2
# teranode is built from source at TERANODE_REF with the patches in patches/teranode applied;
# EXTRA_PATCHES may name a second directory of patches (chaos-test uses it for its PR branch)
TERANODE_REF   ?= main
TERANODE_TAG   ?= $(subst /,-,$(TERANODE_REF))
TERANODE_REPO  ?= https://github.com/bsv-blockchain/teranode
EXTRA_PATCHES  ?=
PATCH_FILES     = $(wildcard $(CURDIR)/patches/teranode/*.patch) $(if $(EXTRA_PATCHES),$(wildcard $(EXTRA_PATCHES)/*.patch))
ALERT_SYSTEM_REF  ?= v0.1.17
ALERT_SYSTEM_REPO ?= https://github.com/bsv-blockchain/go-alert-system
# source checkouts used for image builds; never `vendor/`, which would switch Go to -mod=vendor
UPSTREAM       := upstream
# compose profiles started by `make up`; e.g. make up PROFILES="--profile tools"
PROFILES       ?= --profile tools --profile arcade --profile merkle --profile wallet
# INTERNAL=1 renders egress-free compose networks (a `make gen` setting, not a compose variable)
INTERNAL       ?=
BUSYBOX_IMAGE  ?= docker.io/library/busybox:1.37   # used by `make clean`
# container runtime: podman if installed, else docker; override with RUNTIME=docker
RUNTIME        ?= $(shell command -v podman >/dev/null 2>&1 && echo podman || echo docker)
COMPOSE        ?= $(RUNTIME) compose
export RUNTIME

.PHONY: gen build build-tools build-teranode build-alert-system build-walletd up down clean wait status logs tools test test-walletd tidy

gen: ## regenerate compose.yaml + config/ for N teranodes and SV SV nodes (keys are kept)
	go run ./cmd/gen -n $(N) -sv $(SV) -out . -teranode-image localhost/$(PROJECT)/teranode:$(TERANODE_TAG) -svnode-image $(SVNODE_IMAGE) $(if $(filter 1 true yes,$(INTERNAL)),-internal)

build: build-tools build-alert-system build-teranode build-walletd ## build all images

build-tools: ## alertctl + stackctl image
	$(RUNTIME) build -t localhost/$(PROJECT)/tools:local -f docker/tools.Dockerfile .

build-teranode: ## check out TERANODE_REF into upstream/teranode, apply the patches, build the image
	@if [ ! -d $(UPSTREAM)/teranode/.git ]; then \
	  git clone --depth 1 --branch $(TERANODE_REF) $(TERANODE_REPO) $(UPSTREAM)/teranode; \
	else echo "refreshing $(UPSTREAM)/teranode to $(TERANODE_REF) (a disposable build checkout: local changes are dropped)"; \
	  git -C $(UPSTREAM)/teranode fetch -q --depth 1 origin $(TERANODE_REF) && git -C $(UPSTREAM)/teranode reset -q --hard FETCH_HEAD && git -C $(UPSTREAM)/teranode clean -fdq; fi
	@cd $(UPSTREAM)/teranode && for p in $(PATCH_FILES); do \
	  if git apply --3way --check $$p 2>/dev/null; then git apply --3way $$p && echo "applied $$(basename $$p)"; \
	  elif git apply --reverse --check $$p 2>/dev/null; then echo "already in $(TERANODE_REF): $$(basename $$p)"; \
	  else echo "PATCH $$(basename $$p) DOES NOT APPLY to $(TERANODE_REF); rebase it (see docs/patches.md)"; exit 1; fi; done
	cd $(UPSTREAM)/teranode && $(RUNTIME) build -t localhost/$(PROJECT)/teranode:$(TERANODE_TAG) \
	  --build-arg BASE_IMG=ghcr.io/bsv-blockchain/teranode-base:build-latest \
	  --build-arg RUN_IMG=ghcr.io/bsv-blockchain/teranode-base:run-latest \
	  --build-arg GIT_SHA=$$(git rev-parse HEAD) --build-arg GIT_COMMIT=$$(git rev-parse --short HEAD) \
	  --build-arg GIT_VERSION=$(TERANODE_TAG)-$(PROJECT) --build-arg BUILD_JOBS=10 .

build-alert-system: ## go-alert-system image (hub and SV sidecars; our Dockerfile, fully-qualified base images)
	@if [ ! -d $(UPSTREAM)/go-alert-system/.git ]; then \
	  git clone --depth 1 --branch $(ALERT_SYSTEM_REF) $(ALERT_SYSTEM_REPO) $(UPSTREAM)/go-alert-system; fi
	$(RUNTIME) build -t localhost/$(PROJECT)/alert-system:$(ALERT_SYSTEM_REF) -f docker/alert-system.Dockerfile $(UPSTREAM)/go-alert-system

build-walletd: ## walletd image (the go-wallet-toolbox wallet sidecar; its own Go module)
	$(RUNTIME) build -t localhost/$(PROJECT)/walletd:local -f walletd/Dockerfile walletd

up: ## start the network (teranodes, SV nodes + sidecars, kafka, alert hub) + profiles
	mkdir -p .data
	$(COMPOSE) $(PROFILES) up -d

down: ## stop and remove the network (keeps .data/)
	$(COMPOSE) $(PROFILES) down

clean: down ## also wipe chain/alert state (through a container: the files belong to other uids)
	@if [ -d .data ]; then $(RUNTIME) run --rm -v $(CURDIR)/.data:/d:Z $(BUSYBOX_IMAGE) sh -c 'rm -rf /d/* /d/.[!.]*' && rmdir .data; fi

wait: ## wait until every node answers (teranode health port, SV node RPC, then sidecar health best-effort)
	@# Bounded, and loud on failure: a container that exits at startup used to leave these
	@# loops spinning forever with no output, which looks identical to a slow boot.
	@for n in $$(seq 1 $(N)); do port=$$((20000 + (n-1)*2000)); i=0; \
	  until curl -sf --max-time 2 http://localhost:$$port/health >/dev/null 2>&1; do \
	    i=$$((i+1)); if [ $$i -ge 150 ]; then echo "teranode$$n did not become healthy in 5m:"; \
	      $(RUNTIME) ps -a --filter name=$(PROJECT)-teranode$$n --format '  {{.Names}}  {{.Status}}'; \
	      echo "  last logs:"; $(RUNTIME) logs --tail 5 $(PROJECT)-teranode$$n 2>&1 | sed 's/^/    /'; exit 1; fi; \
	    sleep 2; done; echo "teranode$$n healthy"; done
	@for j in $$(seq 1 $(SV)); do port=$$((40000 + (j-1)*1000 + 332)); i=0; \
	  until curl -sf --max-time 2 -u bitcoin:bitcoin -H 'content-type: application/json' -d '{"jsonrpc":"1.0","id":"w","method":"getblockcount","params":[]}' http://localhost:$$port >/dev/null 2>&1; do \
	    i=$$((i+1)); if [ $$i -ge 150 ]; then echo "svnode$$j did not answer RPC in 5m:"; \
	      $(RUNTIME) ps -a --filter name=$(PROJECT)-svnode$$j --format '  {{.Names}}  {{.Status}}'; \
	      echo "  last logs:"; $(RUNTIME) logs --tail 5 $(PROJECT)-svnode$$j 2>&1 | sed 's/^/    /'; exit 1; fi; \
	    sleep 2; done; echo "svnode$$j healthy"; done
	@for j in $$(seq 1 $(SV)); do port=$$((40000 + (j-1)*1000 + 300)); i=0; \
	  until curl -sf --max-time 2 http://localhost:$$port/health >/dev/null 2>&1 || [ $$i -ge 45 ]; do sleep 2; i=$$((i+1)); done; \
	  if [ $$i -ge 45 ]; then echo "alert-svnode$$j: API not up yet (it starts after 2 alert peers connect)"; else echo "alert-svnode$$j up"; fi; done

status: ## tips and alert sequences of every node
	scripts/tips.sh

logs: ## follow all logs
	$(COMPOSE) $(PROFILES) logs -f

tools: ## shell in the tools container
	$(RUNTIME) exec -it $(PROJECT)-tools bash

test: ## unit tests of the generator, CLIs and their packages
	CGO_ENABLED=0 go test ./...

test-walletd: ## walletd is a separate module outside go.work (it needs gorm's real sqlite driver)
	cd walletd && GOWORK=off CGO_ENABLED=0 go test ./...

tidy: ## GOWORK=off: in workspace mode `go mod tidy` would leave this module's go.sum incomplete for standalone builds
	GOWORK=off go mod tidy && cd walletd && GOWORK=off go mod tidy
