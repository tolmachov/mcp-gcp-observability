BINARY  = mcp-gcp-observability
PKG     = github.com/tolmachov/mcp-gcp-observability/internal
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS = -s -w -X $(PKG).Version=$(VERSION)

# --- Cloud Run deployment (see README "Shared server on Cloud Run") ---
# Override on the command line or in a local environment:
#   make deploy GCP_PROJECT=my-proj GCP_REGION=europe-west1
GCP_PROJECT ?= $(GCP_DEFAULT_PROJECT)
GCP_REGION  ?= europe-west1
AR_REPO     ?= mcp
SERVICE     ?= mcp-gcp-observability
IMAGE        = $(GCP_REGION)-docker.pkg.dev/$(GCP_PROJECT)/$(AR_REPO)/$(SERVICE):$(VERSION)

.PHONY: build lint fmt clean install test test-race test-deploy test-integration test-production docker-build docker-push bootstrap-check bootstrap-apply edge-check edge-apply infra-check infra-apply observability-check observability-apply deploy-check deploy-prepare deploy-cutover deploy

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .

test:
	go test ./...

test-race:
	go test -race ./...

test-deploy:
	python3 -B -m unittest discover -s deploy -p 'test_*.py' -v

# Requires a real GCP project and credentials (loads ../.env). See test/integration_test.go.
test-integration:
	go test -tags integration ./test/...

# Release gate: unlike exploratory integration runs, no skipped tool is accepted.
test-production:
	./deploy/verify-gcp.sh

lint:
	golangci-lint run

fmt:
	golangci-lint fmt

clean:
	rm -f $(BINARY)

install:
	go install -ldflags="$(LDFLAGS)" .

docker-build:
	docker build --platform linux/amd64 --build-arg VERSION=$(VERSION) -t $(IMAGE) .

docker-push: docker-build
	docker push $(IMAGE)

bootstrap-check:
	GCP_PROJECT="$(GCP_PROJECT)" GCP_REGION="$(GCP_REGION)" AR_REPO="$(AR_REPO)" SERVICE="$(SERVICE)" ./deploy/bootstrap.sh check

bootstrap-apply:
	GCP_PROJECT="$(GCP_PROJECT)" GCP_REGION="$(GCP_REGION)" AR_REPO="$(AR_REPO)" SERVICE="$(SERVICE)" ./deploy/bootstrap.sh apply

edge-check:
	GCP_PROJECT="$(GCP_PROJECT)" GCP_REGION="$(GCP_REGION)" AR_REPO="$(AR_REPO)" SERVICE="$(SERVICE)" ./deploy/edge.sh check

edge-apply:
	GCP_PROJECT="$(GCP_PROJECT)" GCP_REGION="$(GCP_REGION)" AR_REPO="$(AR_REPO)" SERVICE="$(SERVICE)" ./deploy/edge.sh apply

infra-check: bootstrap-check edge-check

infra-apply: bootstrap-apply edge-apply

observability-check:
	GCP_PROJECT="$(GCP_PROJECT)" GCP_REGION="$(GCP_REGION)" SERVICE="$(SERVICE)" ./deploy/observability.sh check

observability-apply:
	GCP_PROJECT="$(GCP_PROJECT)" GCP_REGION="$(GCP_REGION)" SERVICE="$(SERVICE)" ./deploy/observability.sh apply

deploy-check:
	GCP_PROJECT="$(GCP_PROJECT)" GCP_REGION="$(GCP_REGION)" SERVICE="$(SERVICE)" ./deploy/rollout.sh check

# Prepare a breaking release without an operator token or traffic promotion.
deploy-prepare: docker-push
	GCP_PROJECT="$(GCP_PROJECT)" GCP_REGION="$(GCP_REGION)" AR_REPO="$(AR_REPO)" SERVICE="$(SERVICE)" IMAGE="$(IMAGE)" ./deploy/rollout.sh prepare

# Route OAuth and authorize against the prepared candidate first; see RUNBOOK.
deploy-cutover:
	GCP_PROJECT="$(GCP_PROJECT)" GCP_REGION="$(GCP_REGION)" SERVICE="$(SERVICE)" ./deploy/rollout.sh cutover

# Compatible releases only: both revisions must accept the operator token.
# Promotes 10% -> 50% -> 100% after authenticated readiness and signal gates.
deploy: docker-push
	GCP_PROJECT="$(GCP_PROJECT)" GCP_REGION="$(GCP_REGION)" AR_REPO="$(AR_REPO)" SERVICE="$(SERVICE)" IMAGE="$(IMAGE)" ./deploy/rollout.sh apply
