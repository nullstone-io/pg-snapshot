NAME    := pg-snapshot
IMAGE   := nullstone/$(NAME)
VERSION ?= latest

.PHONY: build test fmt vet check image push acc acc-up acc-run acc-down

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-X main.version=$(VERSION)" -o ./bin/pgsnap ./cmd/pgsnap

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

check: fmt vet test

# One tag, not a matrix. Customers extend this image to add their migration tool, so a second
# variant would be a second thing for them to choose between for no benefit.
#
#   make image                  -> nullstone/pg-snapshot:latest
#   make push VERSION=v1.0.0    -> builds and pushes nullstone/pg-snapshot:v1.0.0
#
# VERSION is also stamped into the binary, so `pgsnap version` reports what the manifest records.
image:
	docker build -t $(IMAGE):$(VERSION) --build-arg VERSION=$(VERSION) .

push: image
	docker push $(IMAGE):$(VERSION)

# Acceptance tests run against a real postgres. Unit tests cover everything that does not need
# one, so this stays out of the default `test` target.
acc: acc-up acc-run acc-down

acc-up:
	cd acc && docker-compose -p $(NAME)-acc up -d db

acc-run:
	ACC=1 go test ./acc/...

acc-down:
	cd acc && docker-compose -p $(NAME)-acc down
