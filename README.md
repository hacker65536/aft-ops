# AFT Operations Toolkit (`aft-ops`)

Operate [AWS Control Tower Account Factory for Terraform (AFT)](https://github.com/aws-ia/terraform-aws-control_tower_account_factory)
resources at scale from the command line: check hundreds of per-account
customizations pipelines at a glance, find where they fail, and trigger
Release changes — with rate-limit-aware batching, caching, and both a
scriptable CLI and an interactive TUI.

> Status: early development. See [docs/requirements.md](docs/requirements.md)
> and [docs/design.md](docs/design.md).

## Demo

Every recording below runs against a local fixture — no AWS account, no
credentials, no network. You can reproduce any of them with
`aft-ops --demo docs/demo/fixture.json` (see [docs/demo](docs/demo)).

**Triage from the command line** — every account pipeline at a glance, filter
to what is broken, and get the terraform error out of a 200-line CodeBuild log
without opening the console:

![CLI demo](docs/demo/cli.gif)

**Drill down in the TUI** — pipeline list → executions → actions → log, with
each build's terraform verdict fetched lazily and the CodeBuild noise stripped
out of the log:

![TUI demo](docs/demo/tui.gif)

**Release change on many pipelines at once** — `space` to select, `x` to
confirm against freshly refetched status, then the list polls the released
rows until nothing is running:

![Release demo](docs/demo/tui-release.gif)

**Watch what is still going** — `--watch` refetches only the rows that can
still change, and says how many that was:

![Watch demo](docs/demo/watch.gif)

## Install

```bash
brew install hacker65536/tap/aft-ops
```

Or grab a binary from the [releases page](https://github.com/hacker65536/aft-ops/releases),
or build from source:

```bash
go install github.com/hacker65536/aft-ops/cmd/aft-ops@latest
```

## Quick start

```bash
go build -o aft-ops ./cmd/aft-ops

# try it without an AWS account: everything below works against a fixture
./aft-ops --demo docs/demo/fixture.json

# interactive TUI
./aft-ops

# list all account pipelines with their latest status
./aft-ops pipeline list

# failed ones only, as JSON (stable schema for automation / AI agents)
./aft-ops pipeline list --status Failed -o json

# keep watching while something is running
./aft-ops pipeline list --watch --interval 30s

# drill into one pipeline
./aft-ops pipeline show my-account
./aft-ops pipeline executions my-account --actions
./aft-ops pipeline logs my-account --execution <execution-id> --summary

# re-release everything that failed (dry-run first)
./aft-ops pipeline release --status Failed --dry-run
./aft-ops pipeline release --status Failed

# analyze API call rates / throttling from recorded metrics
./aft-ops metrics show
```

Every command that talks to AWS first prints the target it resolved to, so a
stray `AWS_PROFILE` can never send an operation to the wrong account:

```
aws: account 123456789012 · region ap-northeast-1 · profile my-aft-management-profile
```

## TUI

`aft-ops tui` follows the CodePipeline data model down four screens:

```
Pipeline list ──▶ Executions ──▶ Actions ──▶ Log
```

- vim-style navigation: `h` back · `l`/`enter` drill in · `j`/`k` move · `g`/`G` top/bottom.
  A header dots indicator (`••••`) shows the current depth
- `v` from any level jumps straight to the most relevant log — the failed
  action's, else the last build's
- Log view renders the terraform portion by default (`m` cycles
  terraform / raw / summary) and supports less-style search: `/` to search,
  `n`/`N` for next/previous match
- Actions show each action's terraform verdict (`Apply complete! ...` /
  `Error: ...`) fetched lazily from its build log, with plan-colored counts
- `space` multi-select + `x` triggers batch Release change (guarded by
  `release.max_targets`); the confirm screen re-checks the targets' current
  status before you commit
- While any pipeline is running, the list auto-refreshes just those rows every
  `tui.poll_interval` (default 30s, `0` disables it) and stops once everything
  is terminal
- Immutable data (completed builds' logs, finished executions' actions) is
  cached in-session; execution history is served within
  `cache.executions_ttl` (default 15m) — `r`/`R` force a refresh

## Configuration

`~/.config/aft-ops/config.yaml` (all keys optional; flags > `AFT_OPS_*` env > file > defaults):

```yaml
profile: my-aft-management-profile
region: ap-northeast-1
account_source: aft-dynamodb   # aft-dynamodb | organizations | static

# Which shared config file `profile` is looked up in. Unset (the default)
# leaves this to the SDK: $AWS_CONFIG_FILE if set, otherwise ~/.aws/config.
# Set it when you keep one config file per AWS organization — it ties the
# profile to the file that defines it, so switching shells cannot leave a
# configured profile pointing at a file that never had it.
# A value here (or --aws-config-file) overrides $AWS_CONFIG_FILE.
aws_config_file: ~/.aws/config-sandbox

batch:
  concurrency: 10
  rps: 8

cache:
  status_ttl: 10m        # latest-status cache; 0 = always fan out
  executions_ttl: 15m    # in-session execution-history memo

release:
  max_targets: 50
  skip_in_progress: true

tui:
  poll_interval: 30s     # auto-refresh of running pipelines (also --watch's default)
```

Release operations never trust the status cache: `--status` refetches the
whole inventory before deciding what to release, and explicitly named targets
are refetched individually, so neither the selection nor the in-progress skip
is made on minutes-old data.

## License

[MIT](LICENSE)
