# replicanter — reflink-based Podman layer replication.
#
# The exclude_graphdriver_btrfs tag drops containers/storage's btrfs *graph
# driver* (which needs C headers we don't have); it is unrelated to our use of
# btrfs reflinks at the filesystem level — we run on the overlay graph driver.
GO_TAGS := exclude_graphdriver_btrfs
BIN     := bin/replicanter

.PHONY: build test vet tidy clean

build:
	go build -tags '$(GO_TAGS)' -o $(BIN) ./cmd/replicanter

test:
	go test -tags '$(GO_TAGS)' ./...

vet:
	go vet -tags '$(GO_TAGS)' ./...

tidy:
	go mod tidy

clean:
	rm -rf bin
