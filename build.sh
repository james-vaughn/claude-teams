#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

go build -o bin/team-server ./cmd/team-server
go build -o bin/team-mcp ./cmd/team-mcp

echo "Built bin/team-server and bin/team-mcp"
