#!/bin/bash
# Shortcut script to start the mesh client daemon

# Get the directory of this script
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/.." || exit 1

# Require a subcommand (up, down, ps, etc.)
if [ -z "$1" ]; then
    echo "Usage: ./client.sh <subcommand> [flags]"
    echo "Example: ./client.sh up"
    exit 1
fi

SUBCOMMAND=$1
shift

echo "Executing mesh client $SUBCOMMAND..."
go run ./cmd/mesh "$SUBCOMMAND" -config configs/client.yml "$@"
