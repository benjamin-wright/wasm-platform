# Cluster name — matches the name in k3d-config.yaml
CLUSTER_NAME     ?= wasm-platform

# Directory and path for the kubeconfig written during cluster-up
KUBECONFIG_DIR   ?= $(HOME)/.scratch
KUBECONFIG_PATH  ?= $(KUBECONFIG_DIR)/$(CLUSTER_NAME).yaml

# Registry settings
REGISTRY_NAME    ?= $(CLUSTER_NAME)-registry.localhost
REGISTRY_PORT    ?= 5001
IMAGE_TAG        ?= latest

.PHONY: hello
hello:
	cargo build \
		--manifest-path examples/hello-world/Cargo.toml \
		--target wasm32-wasip2 \
		--release

.PHONY: run
run: hello
	cargo run --manifest-path components/execution-host/Cargo.toml

.PHONY: test
test:
	curl -X POST \
		-H "Content-Type: application/json" \
		-d '{"method": "GET", "path": "/hello", "body": "world"}' \
		http://localhost:3000/execute

.PHONY: docker-build
docker-build: hello ## Build the execution-host container image.
	docker build \
		--build-context wasm=target/wasm32-wasip2/release \
		-f components/execution-host/Dockerfile \
		-t $(REGISTRY_NAME)/execution-host:$(IMAGE_TAG) \
		.


##@ Cluster

.PHONY: cluster-up
cluster-up: ## Create the local k3d cluster and registry, then write kubeconfig to KUBECONFIG_PATH.
	@echo "Creating kubeconfig directory $(KUBECONFIG_DIR) …"
	@mkdir -p "$(KUBECONFIG_DIR)"
	@echo "Creating k3d cluster '$(CLUSTER_NAME)' …"
	k3d cluster create $(CLUSTER_NAME) \
		--registry-create $(REGISTRY_NAME):0.0.0.0:$(REGISTRY_PORT) \
		--kubeconfig-update-default=false \
		-p "80:80@loadbalancer" \
		--wait;
	@echo "Writing kubeconfig to $(KUBECONFIG_PATH) …"
	k3d kubeconfig get "$(CLUSTER_NAME)" > "$(KUBECONFIG_PATH)"
	@echo ""
	@echo "Cluster is ready. Run the following (or use direnv) to target it:"
	@echo "  export KUBECONFIG=$(KUBECONFIG_PATH)"
	@echo ""
	@KUBECONFIG="$(KUBECONFIG_PATH)" kubectl get nodes

.PHONY: cluster-down
cluster-down: ## Tear down the local k3d cluster and registry.
	@echo "Deleting k3d cluster '$(CLUSTER_NAME)' …"
	k3d cluster delete "$(CLUSTER_NAME)"
	@echo "Cluster '$(CLUSTER_NAME)' deleted."