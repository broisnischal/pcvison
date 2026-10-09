BIN := plugin/bin/pc

.PHONY: build test install uninstall clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o $(BIN) ./cmd/pc

test:
	go vet ./...
	go test ./...

install: build
	bash install.sh

uninstall:
	bash install.sh --uninstall

clean:
	rm -f $(BIN)
