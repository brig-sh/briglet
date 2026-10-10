# briglet builds two binaries: briglet for the Mac and briglet-guest, the
# static linux/arm64 supervisor every pod VM runs.
GO ?= go

.PHONY: all briglet guest test
all: briglet guest

briglet:
	$(GO) build -o bin/briglet ./cmd/briglet

guest:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 $(GO) build -trimpath -o bin/briglet-guest ./cmd/briglet-guest

test:
	$(GO) test ./...
	GOOS=linux GOARCH=arm64 $(GO) vet ./internal/guest ./cmd/briglet-guest
