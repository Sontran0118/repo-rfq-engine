.PHONY: all test test-go test-web build run seed lint clean

# web/node_modules vendors a Go package; keep the walk on our own trees.
# The api and store suites share one MySQL database, so packages must not run
# concurrently: -p 1 keeps one package's cleanup out of another's data.
PKGS ?= ./cmd/... ./internal/...

DSN ?= rfq:rfq@tcp(127.0.0.1:3306)/rfq_engine?parseTime=true&loc=UTC
ADDR ?= :8080

all: lint test build

## Run the full suite: Go unit + MySQL integration + Angular.
test: test-go test-web

test-go:
	go test $(PKGS) -p 1 -cover

test-race:
	go test $(PKGS) -p 1 -race

test-web:
	cd web && npm test -- --watch=false --browsers=ChromeHeadless

lint:
	gofmt -l cmd internal | tee /dev/stderr | (! read)
	go vet $(PKGS)

build:
	go build -o rfq-server ./cmd/server
	cd web && npm run build

## Start the API. Add seed=1 on a fresh database.
run:
	go run ./cmd/server -addr "$(ADDR)" -dsn "$(DSN)"

seed:
	go run ./cmd/server -addr "$(ADDR)" -dsn "$(DSN)" -seed

## Start the Angular dev server against a running API.
web:
	cd web && npm start

clean:
	rm -f rfq-server
	rm -rf web/dist web/.angular
