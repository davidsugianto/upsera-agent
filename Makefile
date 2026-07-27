BINARY_SERVER := bin/agent-server
BINARY_CLI    := bin/agent-cli
PKG           := ./...

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
eval: ## Run the diagnosis benchmark against a local kind cluster
	go run ./test/harness --scenarios ./test/scenarios --report ./test/results

.PHONY: kind-up
kind-up: ## Create the local eval cluster
	kind create cluster --name triage-eval

.PHONY: kind-down
kind-down: ## Delete the local eval cluster
	kind delete cluster --name triage-eval

.PHONY: docker
docker: ## Build the container image
	docker build -t k8s-triage-agent:dev .

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin dist coverage.out test/results
