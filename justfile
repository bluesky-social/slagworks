# slagworks justfile

# Deployment-specific recipes, when a checkout carries them.
import? 'deploy.just'

# Run unit + component tests locally. Database-backed tests need Docker.
test *args:
    go test ./... {{args}}

# --- Dev NetBox (persistent local instance for development) ---

netbox_compose := "dev/netbox/docker-compose.yml"
netbox_token := "0123456789abcdef0123456789abcdef01234567"

# Start dev NetBox stack (first start runs migrations, ~2min)
netbox-up:
    docker compose -f {{netbox_compose}} up -d
    @echo "NetBox starting at http://localhost:8000 (admin/admin)"
    @echo "API token: {{netbox_token}}"

# Stop dev NetBox stack (preserves data)
netbox-down:
    docker compose -f {{netbox_compose}} down

# Destroy dev NetBox stack and all data, then recreate
netbox-reset:
    docker compose -f {{netbox_compose}} down -v
    docker compose -f {{netbox_compose}} up -d
    @echo "NetBox reset -- starting fresh at http://localhost:8000"

# Show dev NetBox container status
netbox-status:
    docker compose -f {{netbox_compose}} ps

# Tail dev NetBox logs
netbox-logs *args:
    docker compose -f {{netbox_compose}} logs {{args}}

# Run linting
lint:
    golangci-lint run ./...

# Build all binaries (local platform, compile check)
build:
    go build -ldflags="-X main.version=$(git rev-parse HEAD)" ./...

# Build and install the slagctl binary locally
install:
    go install -ldflags="-X main.version=$(git rev-parse HEAD)" ./cmd/slagctl

# Regenerate the slagctl command reference from the cobra tree
gen-cli-docs:
    go run ./cmd/slagctl/docgen docs/user/slagctl-reference.md

# Fail if the committed slagctl reference is stale (used by CI)
check-cli-docs:
    #!/usr/bin/env bash
    set -euo pipefail
    go run ./cmd/slagctl/docgen /tmp/slagctl-reference-check.md
    diff -q /tmp/slagctl-reference-check.md docs/user/slagctl-reference.md \
      || { echo "slagctl reference is stale: run 'just gen-cli-docs' and commit"; exit 1; }

# Build the PXE+squashfs image (requires Linux host or Docker).
# SLAGWORKS_RECOVERY_AUTHORIZED_KEYS must name the recovery sshd's keys file.
image-build:
    bash scripts/image-build/build.sh
