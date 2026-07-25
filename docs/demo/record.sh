#!/usr/bin/env bash
# Record the aft-ops terminal demos:
#   docs/demo/cli.tape          -> docs/demo/cli.gif          (CLI triage)
#   docs/demo/tui.tape          -> docs/demo/tui.gif          (TUI drill-down)
#   docs/demo/tui-release.tape  -> docs/demo/tui-release.gif  (batch Release change)
#   docs/demo/watch.tape        -> docs/demo/watch.gif        (pipeline list --watch)
#
# Offline and deterministic: a throwaway binary is built and driven against
# the committed fixture via --demo, so no AWS account, credentials, or
# network are involved.
#
# The recorder's own environment is isolated too. XDG_CONFIG_HOME /
# XDG_CACHE_HOME / XDG_STATE_HOME are pointed at a scratch dir (internal/
# config/paths.go honors all three), so the operator's real ~/.config/
# aft-ops/config.yaml — which names an actual AWS profile — can never end up
# on screen, and each tape starts from an empty cache.
#
# Requires: go, vhs (brew install vhs), ffmpeg.
#
# Usage: bash docs/demo/record.sh [tape ...]   # default: all tapes
set -euo pipefail

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo"

command -v vhs >/dev/null 2>&1 || { echo "vhs not found (brew install vhs)" >&2; exit 1; }

tapes=("$@")
if [ ${#tapes[@]} -eq 0 ]; then
    tapes=(
        docs/demo/cli.tape
        docs/demo/tui.tape
        docs/demo/tui-release.tape
        docs/demo/watch.tape
    )
fi

bindir="$(mktemp -d)"
work=""
trap 'rm -rf "$bindir" "$work"' EXIT
go build -o "$bindir/aft-ops" ./cmd/aft-ops
export PATH="$bindir:$PATH"

# No credentials reachable: if a demo path ever tried to call AWS, it would
# fail here rather than quietly succeed against the recorder's account.
unset AWS_PROFILE AWS_REGION AWS_DEFAULT_REGION
unset AWS_ACCESS_KEY_ID AWS_SECRET_ACCESS_KEY AWS_SESSION_TOKEN
export AWS_CONFIG_FILE=/dev/null
export AWS_SHARED_CREDENTIALS_FILE=/dev/null
export AWS_EC2_METADATA_DISABLED=true

export AFT_OPS_DEMO="$repo/docs/demo/fixture.json"

for tape in "${tapes[@]}"; do
    rm -rf "$work"
    work="$(mktemp -d)"
    export WORK="$work"
    export XDG_CONFIG_HOME="$work/config"
    export XDG_CACHE_HOME="$work/cache"
    export XDG_STATE_HOME="$work/state"
    mkdir -p "$XDG_CONFIG_HOME/aft-ops"

    # The recording config. rps is raised over the shipped default so the
    # fan-out over the fixture takes about two seconds: long enough for the
    # progress indicator to be legible, short enough not to pad the GIF.
    cat >"$XDG_CONFIG_HOME/aft-ops/config.yaml" <<'YAML'
region: ap-northeast-1

batch:
  concurrency: 12
  rps: 30

cache:
  status_ttl: 10m
  executions_ttl: 15m

release:
  max_targets: 50
  skip_in_progress: true

tui:
  poll_interval: 6s
YAML

    vhs "$tape"
done

echo "recorded: ${tapes[*]}"
