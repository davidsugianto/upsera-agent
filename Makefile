BINARY_SERVER := bin/agent-server
BINARY_CLI    := bin/agent-cli
PKG           := ./...

HELM_CHART    := deploy/helm/k8s-triage-agent
TOFU_DIR      := infra/gke-lab

# Dev environment. Only the kind cluster upsera-lab is ever targeted: every
# cluster command passes --context $(KIND_CONTEXT) and --kubeconfig explicitly
# and depends on kind-guard. Nothing uses the current kubectl context or runs
# `kubectl config use-context`.
KIND_CLUSTER    := upsera-lab
KIND_CONTEXT    ?= kind-upsera-lab
# Dedicated kubeconfig, because `kind create cluster` would otherwise switch the
# current context in ~/.kube/config. bin/ is gitignored.
override DEV_KUBECONFIG := $(CURDIR)/bin/dev-kubeconfig
KUBECTL_DEV      = kubectl --kubeconfig $(DEV_KUBECONFIG) --context $(KIND_CONTEXT)
# dev-kubeconfig holds the kind admin credentials and is only used to set up the
# lab. The agent itself runs with agent-kubeconfig, a short-lived token for the
# read-only ServiceAccount, so the dev loop exercises the real permissions.
# Both paths use `override` so a command-line value can never redirect the rm in
# dev-down to another file.
override AGENT_KUBECONFIG := $(CURDIR)/bin/agent-kubeconfig
# Pinned OpenTelemetry Collector contrib image for dev-collector.
OTELCOL_IMAGE   ?= ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-contrib:0.162.0
OTEL_ENDPOINT   ?= http://localhost:4317

.PHONY: help
help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build both binaries
	go build -o $(BINARY_SERVER) ./cmd/agent-server
	go build -o $(BINARY_CLI) ./cmd/agent-cli

.PHONY: test
test: ## Run unit tests
	go test -race -coverprofile=coverage.out $(PKG)

.PHONY: lint
lint: ## Run golangci-lint
	golangci-lint run

.PHONY: fmt
fmt: ## Format and tidy
	go fmt $(PKG)
	go mod tidy

.PHONY: eval
eval: kind-guard ## Run the diagnosis scenarios against the kind lab cluster and write test/results/report.json
	go run ./test/harness --scenarios ./test/scenarios --report ./test/results \
		--admin-kubeconfig $(DEV_KUBECONFIG) --agent-kubeconfig $(AGENT_KUBECONFIG) --context $(KIND_CONTEXT)

.PHONY: eval-validate
eval-validate: ## Validate the scenario files (no cluster)
	go run ./test/harness --validate --scenarios ./test/scenarios

.PHONY: vet
vet: ## Run go vet
	go vet $(PKG)

.PHONY: helm-lint
helm-lint: ## Lint the Helm chart
	helm lint $(HELM_CHART)

.PHONY: tofu-validate
tofu-validate: ## Check formatting and validate the OpenTofu module (no plan/apply)
	cd $(TOFU_DIR) && tofu fmt -check -recursive && tofu init -backend=false -input=false && tofu validate

.PHONY: ci
ci: build vet test lint helm-lint tofu-validate ## Run every check CI runs

.PHONY: kind-guard
kind-guard: ## Fail if KIND_CONTEXT was overridden (every cluster command uses it explicitly)
	@if [ "$(KIND_CONTEXT)" != "kind-upsera-lab" ]; then \
		echo "refusing to run: kubectl context is '$(KIND_CONTEXT)', expected 'kind-upsera-lab'" >&2; \
		exit 1; \
	fi

.PHONY: dev-up
dev-up: kind-guard ## Create the kind cluster upsera-lab (if absent), apply reader RBAC, and mint the read-only agent token
	@mkdir -p $(dir $(DEV_KUBECONFIG))
	@if kind get clusters | grep -qx '$(KIND_CLUSTER)'; then \
		echo "kind cluster $(KIND_CLUSTER) already exists"; \
		kind export kubeconfig --name $(KIND_CLUSTER) --kubeconfig $(DEV_KUBECONFIG); \
	else \
		kind create cluster --name $(KIND_CLUSTER) --config deploy/dev/kind-config.yaml --kubeconfig $(DEV_KUBECONFIG); \
	fi
	$(KUBECTL_DEV) apply -f deploy/kubernetes/rbac.yaml
	ADMIN_KUBECONFIG=$(DEV_KUBECONFIG) AGENT_KUBECONFIG=$(AGENT_KUBECONFIG) KIND_CONTEXT=$(KIND_CONTEXT) \
		bash hack/dev-agent-kubeconfig.sh

.PHONY: dev-down
dev-down: kind-guard ## Delete the kind cluster upsera-lab and its dev kubeconfigs
	kind delete cluster --name $(KIND_CLUSTER) --kubeconfig $(DEV_KUBECONFIG)
	rm -f $(DEV_KUBECONFIG) $(AGENT_KUBECONFIG)

.PHONY: dev-collector
dev-collector: ## Run the OpenTelemetry Collector (Datadog exporter) on localhost:4317; needs DD_API_KEY and DD_SITE
	@test -n "$$DD_API_KEY" || { echo "DD_API_KEY is not set; export it before 'make dev-collector'" >&2; exit 1; }
	@test -n "$$DD_SITE" || { echo "DD_SITE is not set (e.g. datadoghq.com); export it before 'make dev-collector'" >&2; exit 1; }
	docker run --rm --name upsera-otel-collector -p 127.0.0.1:4317:4317 \
		-e DD_API_KEY -e DD_SITE \
		-v $(CURDIR)/deploy/dev/otel-collector.yaml:/etc/otelcol-contrib/config.yaml:ro \
		$(OTELCOL_IMAGE)

.PHONY: dev-collector-local
dev-collector-local: ## Run a collector that prints what it receives (no Datadog, no keys) on localhost:4317
	docker run --rm --name upsera-otel-collector -p 127.0.0.1:4317:4317 \
		-v $(CURDIR)/deploy/dev/otel-collector.local.yaml:/etc/otelcol-contrib/config.yaml:ro \
		$(OTELCOL_IMAGE)

.PHONY: verify-telemetry
verify-telemetry: ## Check without Datadog that the agent's span and metric reach a local collector (needs docker)
	OTELCOL_IMAGE=$(OTELCOL_IMAGE) bash hack/verify-telemetry-local.sh

.PHONY: dev-run
dev-run: kind-guard ## Run agent-server (read-only ServiceAccount identity) against the dev config, exporting OTLP to localhost:4317
	@test -f $(AGENT_KUBECONFIG) || { echo "$(AGENT_KUBECONFIG) not found; run 'make dev-up' first" >&2; exit 1; }
	KUBECONFIG=$(AGENT_KUBECONFIG) OTEL_EXPORTER_OTLP_ENDPOINT=$(OTEL_ENDPOINT) \
		go run ./cmd/agent-server --config deploy/dev/config.dev.yaml

.PHONY: check-datadog
check-datadog: ## Verify Datadog keys are valid and the application key is read-only
	bash hack/datadog-readonly-check.sh

.PHONY: docker
docker: ## Build the container image
	docker build -t upsera-agent:dev .

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin dist coverage.out test/results
