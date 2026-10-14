.PHONY: fmt fmt-check vet test ci up down fault-inventory-latency fault-clear eval eval-repeat

COMPOSE := docker compose -f deploy/docker-compose.yml
INVENTORY_ADMIN := http://127.0.0.1:9081
FAULT_LATENCY_MS ?= 800

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

## fault-inventory-latency: make inventory-api respond in FAULT_LATENCY_MS (default 800)
fault-inventory-latency:
	@curl -fsS -X PUT -H 'Content-Type: application/json' \
		-d '{"latency_ms": $(FAULT_LATENCY_MS)}' $(INVENTORY_ADMIN)/fault

## fault-clear: restore inventory-api's baseline latency
fault-clear:
	@curl -fsS -X DELETE $(INVENTORY_ADMIN)/fault

## eval: start the environment and run the inventory-latency scenario end to end
##       (needs ANTHROPIC_API_KEY; pass runner flags with EVAL_ARGS="-effort high")
SCENARIO ?= scenarios/inventory-latency
eval: up
	go run ./cmd/scenario-runner -scenario $(SCENARIO) $(EVAL_ARGS)

## eval-repeat: run SCENARIO RUNS times (default 3) and report how many passed; stops at
##              the first setup error. The runner is built first because go run reports
##              every failure as exit 1, hiding the difference between FAIL (1) and setup error (2).
RUNS ?= 3
eval-repeat: up
	@case "$(RUNS)" in ''|*[!0-9]*|0) echo "RUNS must be a positive integer"; exit 2;; esac; \
	dir="$$(mktemp -d)"; trap 'rm -rf "$$dir"' EXIT; \
	go build -o "$$dir/scenario-runner" ./cmd/scenario-runner || exit 2; \
	pass=0; \
	for i in $$(seq 1 $(RUNS)); do \
		echo "=== $(SCENARIO): run $$i of $(RUNS)"; \
		"$$dir/scenario-runner" -scenario $(SCENARIO) $(EVAL_ARGS); code=$$?; \
		if [ $$code -eq 0 ]; then pass=$$((pass + 1)); \
		elif [ $$code -ne 1 ]; then echo "=== run $$i: setup error (exit $$code); stopping"; exit $$code; fi; \
	done; \
	echo "=== $(SCENARIO): $$pass of $(RUNS) runs passed"; \
	[ $$pass -eq $(RUNS) ]
