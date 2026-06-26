#!/bin/bash

# Exit immediately if a pipeline returns a non-zero status.
set -eo pipefail

CONFIG_FILE_YAML="$(pwd)/deploy.yaml"

# Define ANSI colors (disable if stdout is not a terminal)
if [ -t 1 ]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[0;33m'
    BLUE='\033[0;34m'
    MAGENTA='\033[0;35m'
    CYAN='\033[0;36m'
    BOLD='\033[1m'
    NC='\033[0m' # No Color
else
    RED=''
    GREEN=''
    YELLOW=''
    BLUE=''
    MAGENTA=''
    CYAN=''
    BOLD=''
    NC=''
fi

# Print usage instructions if arguments are insufficient
DRY_RUN=false
if [ "$1" = "--dry-run" ]; then
    DRY_RUN=true
    shift
fi

if [ "$#" -lt 2 ]; then
    echo -e "${YELLOW}Usage: $0 [--dry-run] <network> <command_or_target> [KEY=VALUE ...]${NC}"
    exit 1
fi

NETWORK="$1"
CMD_OR_TARGET="$2"
shift 2

# Validate that deploy.yaml exists
if [ ! -f "$CONFIG_FILE_YAML" ]; then
    echo -e "${RED}Error: Configuration file not found at $CONFIG_FILE_YAML${NC}"
    exit 1
fi

# Verify network exists in config
if ! yq -e ".networks.\"$NETWORK\"" "$CONFIG_FILE_YAML" >/dev/null 2>&1; then
    echo -e "${RED}Error: Network '$NETWORK' not found in $CONFIG_FILE_YAML${NC}"
    exit 1
fi

# Read network configuration
HOSTS=$(yq ".networks.\"$NETWORK\".hosts // [] | .[]" "$CONFIG_FILE_YAML" 2>/dev/null || true)
if [ -z "$HOSTS" ]; then
    echo -e "${RED}Error: No hosts specified for network '$NETWORK'.${NC}"
    exit 1
fi

KEY_FILE=$(yq ".networks.\"$NETWORK\".private_key_file // \"\"" "$CONFIG_FILE_YAML" 2>/dev/null || true)

# Merge environment variables:
# 1. Global env
# 2. Network-specific env
# 3. Command line KEY=VALUE arguments
COMBINED_ENV=$(yq "(.env // {}) * (.networks.\"$NETWORK\".env // {})" "$CONFIG_FILE_YAML" 2>/dev/null || true)

# Parse and append command line KEY=VALUE arguments
for arg in "$@"; do
    if [[ "$arg" =~ ^([a-zA-Z_][a-zA-Z0-9_]*)=(.*)$ ]]; then
        key="${BASH_REMATCH[1]}"
        val="${BASH_REMATCH[2]}"
        COMBINED_ENV=$(echo "$COMBINED_ENV" | ARG_KEY="$key" ARG_VAL="$val" yq '. + { env(ARG_KEY): env(ARG_VAL) }' 2>/dev/null || true)
    else
        echo -e "${YELLOW}Warning: Ignored invalid argument format: $arg (expected KEY=VALUE)${NC}"
    fi
done

# Resolve command or target to run
IS_TARGET=$(yq -e ".targets.\"$CMD_OR_TARGET\"" "$CONFIG_FILE_YAML" >/dev/null 2>&1 && echo "true" || echo "false")

if [ "$IS_TARGET" = "true" ]; then
    # Retrieve commands list from target
    CMDS=$(yq ".targets.\"$CMD_OR_TARGET\"[]" "$CONFIG_FILE_YAML" 2>/dev/null || true)
else
    # Verify command exists
    IS_CMD=$(yq -e ".commands.\"$CMD_OR_TARGET\"" "$CONFIG_FILE_YAML" >/dev/null 2>&1 && echo "true" || echo "false")
    if [ "$IS_CMD" = "true" ]; then
        CMDS="$CMD_OR_TARGET"
    else
        echo -e "${RED}Error: Command or target '$CMD_OR_TARGET' not found in $CONFIG_FILE_YAML${NC}"
        exit 1
    fi
fi

# Build environment export string
EXPORT_CMD=""
while IFS=$'\t' read -r key val; do
    # Escape single quotes in the value to make it safe for the remote shell
    val_escaped=$(printf '%s' "$val" | sed "s/'/'\\\\''/g")
    EXPORT_CMD="$EXPORT_CMD export $key='$val_escaped';"
done < <(echo "$COMBINED_ENV" | yq -r 'to_entries[] | .key + "\t" + .value' 2>/dev/null || true)

# Execute commands on remote hosts
for host_str in $HOSTS; do
    echo -e "${BLUE}========================================================${NC}"
    echo -e " Network: ${BOLD}${CYAN}$NETWORK${NC} | Host: ${BOLD}${YELLOW}$host_str${NC}"
    echo -e "${BLUE}========================================================${NC}"

    # Parse host and port ([user@]host[:port])
    PORT=""
    SSH_HOST="$host_str"
    if [[ "$host_str" =~ :([0-9]+)$ ]]; then
        PORT="${BASH_REMATCH[1]}"
        SSH_HOST="${host_str%:${PORT}}"
    fi

    # Build ssh argument array
    SSH_ARGS=("-o" "ConnectTimeout=10")
    if [ -t 0 ]; then
        SSH_ARGS+=("-t")
    fi
    if [ -n "$PORT" ]; then
        SSH_ARGS+=("-p" "$PORT")
    fi
    if [ -n "$KEY_FILE" ]; then
        # Expand tilde ~ to $HOME
        EXPANDED_KEY_FILE="${KEY_FILE/#\~/$HOME}"
        SSH_ARGS+=("-i" "$EXPANDED_KEY_FILE")
        # If the key file contains "_sk" or the corresponding public key is a security key (sk-*),
        # bypass the buggy macOS ssh-agent by default using IdentityAgent none.
        if [[ "$KEY_FILE" == *"_sk"* ]] || { [ -f "${EXPANDED_KEY_FILE}.pub" ] && grep -qE "^sk-" "${EXPANDED_KEY_FILE}.pub"; }; then
            SSH_ARGS+=("-o" "IdentityAgent=none")
        fi
    fi

    # Build combined command string joined with &&
    COMBINED_RUN_SCRIPT=""
    CMD_NAMES_JOINED=""
    FIRST=true
    for cmd_name in $CMDS; do
        RUN_SCRIPT=$(yq ".commands.\"$cmd_name\".run" "$CONFIG_FILE_YAML" 2>/dev/null || true)
        if [ "$RUN_SCRIPT" = "null" ]; then
            echo -e "${RED}Error: Run script for command '$cmd_name' is null or undefined.${NC}"
            exit 1
        fi
        if [ "$FIRST" = "true" ]; then
            COMBINED_RUN_SCRIPT="($RUN_SCRIPT)"
            CMD_NAMES_JOINED="$cmd_name"
            FIRST=false
        else
            COMBINED_RUN_SCRIPT="$COMBINED_RUN_SCRIPT && ($RUN_SCRIPT)"
            CMD_NAMES_JOINED="$CMD_NAMES_JOINED && $cmd_name"
        fi
    done

    echo -e "${GREEN}---> Running commands:${NC} ${BOLD}$CMD_NAMES_JOINED${NC}"
    
    if [ "$DRY_RUN" = "true" ]; then
        echo -e "${MAGENTA}[DRY RUN] Would execute:${NC}"
        echo -e "  ssh ${SSH_ARGS[*]} $SSH_HOST \"$EXPORT_CMD $COMBINED_RUN_SCRIPT\""
        SSH_STATUS=0
    else
        # Execute ssh command and capture status
        ssh "${SSH_ARGS[@]}" "$SSH_HOST" "$EXPORT_CMD $COMBINED_RUN_SCRIPT"
        SSH_STATUS=$?
    fi
    
    if [ $SSH_STATUS -ne 0 ]; then
        echo -e "${RED}Error: Deployment failed on $host_str with exit code $SSH_STATUS${NC}"
        exit $SSH_STATUS
    fi
done

echo -e "${GREEN}${BOLD}Deployment completed successfully!${NC}"