IMAGE ?= ghcr.io/iperka/unifi-drive-csi
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
KIND_CLUSTER ?= unifi-drive

.PHONY: all build test cover vulncheck release-check snapshot vet fmt lint image push deploy clean \
	kind-up kind-load kind-deploy kind-test kind-logs kind-down

all: vet test build

build: ## Build the driver binary into bin/
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/unifi-drive-csi ./cmd/unifi-drive-csi

test: ## Run unit tests (race) incl. csi-sanity conformance
	go test ./... -race -count=1

cover: ## Run tests with coverage summary
	go test ./... -count=1 -coverprofile=coverage.out
	go tool cover -func=coverage.out | tail -1

vulncheck: ## Scan for known vulnerabilities (matches CI)
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

release-check: ## Validate the GoReleaser config
	goreleaser check

snapshot: ## Local GoReleaser snapshot build (multi-arch image, no publish)
	goreleaser release --snapshot --clean --skip=sign

vet:
	go vet ./...

fmt:
	gofmt -s -w .

image: ## Build the container image
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

push: image ## Build and push the container image
	docker push $(IMAGE):$(VERSION)
	docker push $(IMAGE):latest

deploy: ## Apply all manifests (expects deploy/secret.yaml to exist)
	kubectl apply -f deploy/rbac.yaml
	kubectl apply -f deploy/csidriver.yaml
	kubectl apply -f deploy/secret.yaml
	kubectl apply -f deploy/controller.yaml
	kubectl apply -f deploy/node.yaml
	kubectl apply -f deploy/storageclass.yaml

clean:
	rm -rf bin

# --- local testing in a kind cluster ---

kind-up: ## Create the kind cluster
	kind get clusters | grep -qx $(KIND_CLUSTER) || kind create cluster --name $(KIND_CLUSTER)

kind-load: image ## Build the image and load it into kind
	kind load docker-image $(IMAGE):latest --name $(KIND_CLUSTER)

kind-deploy: kind-up kind-load ## Full deploy into kind (expects deploy/secret.yaml)
	$(MAKE) deploy

kind-test: ## Apply the example PVC + pod and watch it bind
	kubectl apply -f deploy/example-pvc.yaml
	kubectl wait --for=jsonpath='{.status.phase}'=Bound pvc/unifi-drive-test --timeout=120s
	kubectl wait --for=condition=Ready pod/unifi-drive-test --timeout=120s
	kubectl logs pod/unifi-drive-test

kind-logs: ## Tail the controller logs (where share provisioning happens)
	kubectl -n unifi-drive-csi logs deploy/unifi-drive-csi-controller -c unifi-drive-csi -f

kind-down: ## Delete the kind cluster
	kind delete cluster --name $(KIND_CLUSTER)
