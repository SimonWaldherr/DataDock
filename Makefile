.PHONY: run run-memory build test

ADDR ?= :8080
DB ?= datadock.db

run:
	go run . -addr $(ADDR) -db $(DB)

run-memory:
	go run . -addr $(ADDR) -db :memory:

build:
	go build -o datadock .

test:
	go test ./...
