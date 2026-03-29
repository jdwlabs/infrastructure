#!/bin/bash

# scenario-runner.sh
# Usage: ./scenario-runner.sh <number>

set -e

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SCENARIOS_DIR="$SCRIPT_DIR/scaling_tests"
TFVARS_FILE="$SCRIPT_DIR/../terraform/terraform.tfvars"
BOOTSTRAP_DIR="$SCRIPT_DIR/../bootstrap"
BOOTSTRAP_BIN="$BOOTSTRAP_DIR/build/talops"

if [ $# -eq 0 ]; then
    echo -e "${RED}Error: No scenario number specified${NC}"
    echo "Usage: $0 <number>"
    echo ""
    echo "Available scenarios:"
    ls -1 "$SCENARIOS_DIR"/[0-9]*_*.tfvars 2>/dev/null | while read f; do
        num=$(basename "$f" | cut -d'_' -f1 | sed 's/^0//')
        name=$(basename "$f" | sed 's/^[0-9]*_//' | sed 's/\.tfvars$//')
        echo "  $num - $name"
    done
    exit 1
fi

NUM="$1"

# Find scenario file by number
SCENARIO_FILE=$(ls -1 "$SCENARIOS_DIR"/[0-9]*_*.tfvars 2>/dev/null | while read f; do
    file_num=$(basename "$f" | cut -d'_' -f1 | sed 's/^0//')
    if [ "$file_num" = "$NUM" ]; then
        echo "$f"
        break
    fi
done)

if [ -z "$SCENARIO_FILE" ]; then
    echo -e "${RED}Error: No scenario found for number: $NUM${NC}"
    exit 1
fi

# Ensure bootstrap binary exists
if [ ! -f "$BOOTSTRAP_BIN" ]; then
    echo -e "${YELLOW}Building bootstrap binary...${NC}"
    make -C "$BOOTSTRAP_DIR" build
fi

echo -e "${YELLOW}=== Scenario Runner ===${NC}"
echo "Scenario $NUM: $(basename "$SCENARIO_FILE")"
echo ""

echo -e "${YELLOW}[1/3] Copying to terraform.tfvars...${NC}"
cp "$SCENARIO_FILE" "$TFVARS_FILE"
echo -e "${GREEN}✓ Done${NC}"

echo -e "${YELLOW}[2/3] Destroying cluster...${NC}"
"$BOOTSTRAP_BIN" down --auto-approve --force --terraform-dir "$SCRIPT_DIR/../terraform" --tfvars "$TFVARS_FILE"
echo -e "${GREEN}✓ Done${NC}"

echo -e "${YELLOW}[3/3] Deploying cluster...${NC}"
"$BOOTSTRAP_BIN" up --auto-approve --terraform-dir "$SCRIPT_DIR/../terraform" --tfvars "$TFVARS_FILE"
echo -e "${GREEN}✓ Done${NC}"

echo ""
echo -e "${GREEN}=== Scenario $NUM completed ===${NC}"
