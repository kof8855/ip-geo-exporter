BINARY_NAME=ip-geo-exporter
GO_SRC=$(shell find . -name "*.go" -not -path "./vendor/*")
BUILD_FLAGS=-ldflags="-s -w"

.PHONY: all build clean test run

all: build

build:
	go build $(BUILD_FLAGS) -o $(BINARY_NAME) .

clean:
	rm -f $(BINARY_NAME)
	rm -rf /tmp/ip-geo-exporter*

test:
	go test ./...

run: build
	sudo ./$(BINARY_NAME) --geoip-db=/usr/share/GeoIP/GeoLite2-City.mmdb

# Cross-compilation targets
build-linux-amd64:
	GOOS=linux GOARCH=amd64 go build $(BUILD_FLAGS) -o $(BINARY_NAME)-linux-amd64 .

build-linux-arm64:
	GOOS=linux GOARCH=arm64 go build $(BUILD_FLAGS) -o $(BINARY_NAME)-linux-arm64 .

# Release helpers
release: build-linux-amd64 build-linux-arm64
	ls -lh $(BINARY_NAME)-*

# Install to system (requires sudo)
install: build
	install -m 0755 $(BINARY_NAME) /usr/local/bin/$(BINARY_NAME)
	install -m 0644 deploy/$(BINARY_NAME).service /etc/systemd/system/
	systemctl daemon-reload
	@echo "Binary installed. Run: sudo systemctl enable --now $(BINARY_NAME)"
