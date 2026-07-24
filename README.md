# AFT Operations Toolkit (`aft-ops`)

Operate [AWS Control Tower Account Factory for Terraform (AFT)](https://github.com/aws-ia/terraform-aws-control_tower_account_factory)
resources at scale from the command line: check hundreds of per-account
customizations pipelines at a glance, find where they fail, and trigger
Release changes — with rate-limit-aware batching, caching, and both a
scriptable CLI and an interactive TUI.

> Status: early development (Phase 1). See [docs/requirements.md](docs/requirements.md)
> and [docs/design.md](docs/design.md).

## Quick start

```bash
go build -o aft-ops ./cmd/aft-ops

# interactive TUI
./aft-ops

# list all account pipelines with their latest status
./aft-ops pipeline list

# failed ones only, as JSON (stable schema for automation / AI agents)
./aft-ops pipeline list --status Failed -o json

# re-release everything that failed (dry-run first)
./aft-ops pipeline release --status Failed --dry-run
./aft-ops pipeline release --status Failed

# analyze API call rates / throttling from recorded metrics
./aft-ops metrics show
```

## Configuration

`~/.config/aft-ops/config.yaml` (all keys optional; flags > `AFT_OPS_*` env > file > defaults):

```yaml
profile: my-aft-management-profile
region: ap-northeast-1
account_source: aft-dynamodb   # aft-dynamodb | organizations | static

batch:
  concurrency: 10
  rps: 8

release:
  max_targets: 50
  skip_in_progress: true
```

## License

TBD (planned: MIT).
