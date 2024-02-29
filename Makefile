# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOTEST=$(GOCMD) test
GOTIDY=$(GOCMD) mod tidy
GOFMT=$(GOCMD) fmt
GOCLEAN=$(GOCMD) clean

# 项目名称
BINARY_NAME=plow

.PHONY: build test   tidy fmt

build:
	$(GOBUILD) -o $(BINARY_NAME) -v

test:
	$(GOTEST) -v ./...


tidy:
	$(GOTIDY)

lint:
	$(GOFMT) ./...

clean:
	$(GOCLEAN)
	rm -f $(BINARY_NAME)_macos $(BINARY_NAME)_linux_amd64  $(BINARY_NAME)_linux_arm64 -o $(BINARY_NAME).exe $(BINARY_NAME)