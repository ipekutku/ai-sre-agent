.PHONY: fmt fmt-check vet test ci up down fault-inventory-latency fault-inventory-errors fault-clear eval eval-repeat

COMPOSE := docker compose -f deploy/docker-compose.yml
INVENTORY_ADMIN := http://127.0.0.1:9081
FAULT_LATENCY_MS ?= 800
FAULT_ERROR_PERCENT ?= 50
# Where the scenario runner sends the agent's spans: Tempo's OTLP receiver, published on
# 127.0.0.1. Set it to empty to run investigations untraced.
AGENT_TRACES_ENDPOINT ?= http://127.0.0.1:4318/v1/traces
RUNNER_ENV := OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=$(AGENT_TRACES_ENDPOINT)

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

## up: build and start the local environment (services, Prometheus, Tempo, Loki, Grafana)
up:
	$(COMPOSE) up --build -d

## down: stop the local environment and remove its containers
down:
	$(COMPOSE) down

## fault-inventory-latency: make inventory-api respond in FAULT_LATENCY_MS (default 800).
##                          Replaces any active fault.
fault-inventory-latency:
	@curl -fsS -X PUT -H 'Content-Type: application/json' \
		-d '{"latency_ms": $(FAULT_LATENCY_MS)}' $(INVENTORY_ADMIN)/fault

## fault-inventory-errors: make FAULT_ERROR_PERCENT (default 50) of inventory-api requests
##                         fail with 500. Replaces any active fault.
fault-inventory-errors:
	@curl -fsS -X PUT -H 'Content-Type: application/json' \
		-d '{"error_percent": $(FAULT_ERROR_PERCENT)}' $(INVENTORY_ADMIN)/fault

## fault-clear: restore inventory-api's normal behaviour (baseline latency, no errors)
fault-clear:
	@curl -fsS -X DELETE $(INVENTORY_ADMIN)/fault

## eval: start the environment and run the inventory-latency scenario end to end
##       (needs ANTHROPIC_API_KEY; pass runner flags with EVAL_ARGS="-effort high").
##       The investigation is traced to Tempo; the report prints its trace ID.
SCENARIO ?= scenarios/inventory-latency
eval: up
	$(RUNNER_ENV) go run ./cmd/scenario-runner -scenario $(SCENARIO) $(EVAL_ARGS)

## eval-repeat: run SCENARIO RUNS times (default 3) and report how many passed; stops at
##              the first setup error. Waits COOLDOWN seconds (default 300) between runs so
##              each run's fault shows up as a separate spike in the shared metrics and traces.
##              The runner is built first because go run reports every failure as exit 1,
##              hiding the difference between FAIL (1) and setup error (2).
RUNS ?= 3
COOLDOWN ?= 300
eval-repeat: up
	@case "$(RUNS)" in ''|*[!0-9]*|0) echo "RUNS must be a positive integer"; exit 2;; esac; \
	case "$(COOLDOWN)" in ''|*[!0-9]*) echo "COOLDOWN must be a whole number of seconds"; exit 2;; esac; \
	dir="$$(mktemp -d)"; trap 'rm -rf "$$dir"' EXIT; \
	go build -o "$$dir/scenario-runner" ./cmd/scenario-runner || exit 2; \
	pass=0; \
	for i in $$(seq 1 $(RUNS)); do \
		if [ $$i -gt 1 ] && [ $(COOLDOWN) -gt 0 ]; then echo "=== cooling down for $(COOLDOWN)s"; sleep $(COOLDOWN); fi; \
		echo "=== $(SCENARIO): run $$i of $(RUNS)"; \
		$(RUNNER_ENV) "$$dir/scenario-runner" -scenario $(SCENARIO) $(EVAL_ARGS); code=$$?; \
		if [ $$code -eq 0 ]; then pass=$$((pass + 1)); \
		elif [ $$code -ne 1 ]; then echo "=== run $$i: setup error (exit $$code); stopping"; exit $$code; fi; \
	done; \
	echo "=== $(SCENARIO): $$pass of $(RUNS) runs passed"; \
	[ $$pass -eq $(RUNS) ]
