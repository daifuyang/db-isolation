# db-isolation Makefile
#
# Goals:
#   make build         build all three binaries into ./bin/
#   make build-linux   cross-compile linux/amd64 for ECS deploy
#   make test          run unit tests with -race
#   make itest         run integration tests (requires MySQL)
#   make tidy          go mod tidy
#   make vet           go vet
#   make install       install binaries into /usr/local/bin (root)
#   make install-svc   install systemd unit and create runtime dirs
#   make install-pm2   create runtime dirs in pm2 mode (no systemd unit)
#   make deploy HOST=user@host      rsync + pm2 reload on the ECS
#   make smoke-pm2     run the post-deploy smoke test (needs MySQL on the box)

GO ?= go
PKG := github.com/daifuyang/db-isolation
BIN_DIR := bin
BINS := db-isolation dbi mcp

.PHONY: all build build-linux test itest tidy vet fmt \
        install install-svc install-pm2 deploy smoke-pm2 clean

all: build

$(BIN_DIR):
	mkdir -p $(BIN_DIR)

build: $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/db-isolation ./cmd/server
	$(GO) build -o $(BIN_DIR)/dbi         ./cmd/dbi
	$(GO) build -o $(BIN_DIR)/mcp         ./cmd/mcp

# Cross-compile stripped linux/amd64 binaries for the ECS. The script
# `scripts/deploy-aliyun.sh` calls the same flags directly.
build-linux:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
	    $(GO) build -trimpath -ldflags='-s -w' \
	    -o $(BIN_DIR)/db-isolation.linux-amd64 ./cmd/server
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
	    $(GO) build -trimpath -ldflags='-s -w' \
	    -o $(BIN_DIR)/dbi.linux-amd64         ./cmd/dbi
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
	    $(GO) build -trimpath -ldflags='-s -w' \
	    -o $(BIN_DIR)/mcp.linux-amd64         ./cmd/mcp

test:
	$(GO) test ./... -race -count=1

itest:
	@test -n "$$DB_ISOLATION_TEST_MYSQL_DSN" || \
		(echo "set DB_ISOLATION_TEST_MYSQL_DSN to enable integration tests" && exit 1)
	DB_ISOLATION_TEST_MYSQL_DSN="$$DB_ISOLATION_TEST_MYSQL_DSN" \
		$(GO) test ./internal/mysqlx/... -race -count=1 -v

tidy:
	$(GO) mod tidy

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

install: build
	install -m 0755 $(BIN_DIR)/db-isolation /usr/local/bin/db-isolation
	install -m 0755 $(BIN_DIR)/dbi         /usr/local/bin/dbi
	install -m 0755 $(BIN_DIR)/mcp         /usr/local/bin/db-isolation-mcp

install-svc: install
	install -m 0644 scripts/db-isolation.service /etc/systemd/system/db-isolation.service
	bash scripts/install.sh

install-pm2: install
	# Run as root. Sets up runtime dirs in /etc, /var for pm2 mode.
	bash scripts/install.sh --pm2

# Cross-build + rsync + pm2 reload on the remote. Usage:
#   make deploy HOST=root@dbi.example.com
deploy: build-linux
	@test -n "$(HOST)" || (echo "set HOST=user@host" && exit 1)
	bash scripts/deploy-aliyun.sh "$(HOST)"

# Post-deploy verification. Run from the ECS after deploy.
smoke-pm2:
	bash scripts/smoke-pm2.sh

clean:
	rm -rf $(BIN_DIR)