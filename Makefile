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

.PHONY: build lint fmt clean install test test-race test-integration docker-build docker-push deploy

build:
	go build -trimpath -ldflags="$(LDFLAGS)" -o $(BINARY) .

test:
	go test ./...

test-race:
	go test -race ./...

# Requires a real GCP project and credentials (loads ../.env). See test/integration_test.go.
test-integration:
	go test -tags integration ./test/...

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

# Deploys the shared authenticated server. One-time GCP setup (APIs, OAuth
# client, secrets, service account) is documented in the README; this target
# only rolls out a new revision. Non-secret runtime settings live in
# deploy/cloudrun.env (copy deploy/cloudrun.env.example); the file is folded
# into --set-env-vars because gcloud forbids combining --env-vars-file with
# --set-secrets.
deploy: docker-push
	gcloud run deploy $(SERVICE) \
		--project $(GCP_PROJECT) \
		--region $(GCP_REGION) \
		--image $(IMAGE) \
		--service-account $(SERVICE)@$(GCP_PROJECT).iam.gserviceaccount.com \
		--allow-unauthenticated \
		--min-instances 0 \
		--max-instances 3 \
		--memory 512Mi \
		--args run \
		--set-env-vars "^@^$$(grep -v '^\s*\#' deploy/cloudrun.env | grep -v '^\s*$$' | sed 's/: /=/' | paste -sd'@' -)" \
		--set-secrets AUTH_GOOGLE_CLIENT_SECRET=mcp-obs-google-client-secret:latest,AUTH_TOKEN_KEY=mcp-obs-token-key:latest
