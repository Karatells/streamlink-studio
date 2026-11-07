VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)
APP     := streamlink-studio
DIST    := dist

.PHONY: build run test vet release clean

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(APP) .

run: build
	STUDIO_DATA=./data ./$(APP)

vet:
	gofmt -l . ; go vet ./...

# linux/amd64 + linux/arm64 archives and checksums, as expected by install.sh
release: clean
	mkdir -p $(DIST)
	for arch in amd64 arm64; do \
	  CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/$(APP) . && \
	  tar -C $(DIST) -czf $(DIST)/$(APP)_linux_$$arch.tar.gz $(APP) && rm $(DIST)/$(APP); \
	done
	cd $(DIST) && sha256sum *.tar.gz > checksums.txt

clean:
	rm -rf $(DIST) $(APP)
