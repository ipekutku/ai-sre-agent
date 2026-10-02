.PHONY: fmt fmt-check vet test ci up down

COMPOSE := docker compose -f deploy/docker-compose.yml

## fmt: format all Go source files in place
fmt:
	gofmt -w .

## fmt-check: fail if any Go source file is not gofmt-formatted
fmt-check:
	@unformatted="$$(gofmt -l .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "The following files need gofmt:"; \
		echo "$$unformatted"; \
		exit 1; \
	fi

## vet: run go vet on all packages
vet:
	go vet ./...

## test: run all tests
test:
	go test ./...

## ci: run the same checks as CI
ci: fmt-check vet test

## up: build and start the local environment (services + Prometheus)
up:
	$(COMPOSE) up --build -d

## down: stop the local environment and remove its containers
down:
	$(COMPOSE) down
