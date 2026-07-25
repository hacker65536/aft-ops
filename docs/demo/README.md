# demo — offline demo mode and the README recordings

`aft-ops --demo <fixture.json>` runs the whole tool against a local file
instead of AWS. It needs no credentials, no network, and no AFT account. That
serves two purposes: it is how the GIFs in the top-level README are recorded,
and it is how someone can try the tool before pointing it at a real management
account.

```bash
aft-ops --demo docs/demo/fixture.json                    # the TUI
aft-ops --demo docs/demo/fixture.json pipeline list
aft-ops --demo docs/demo/fixture.json pipeline show payments-stg
aft-ops --demo docs/demo/fixture.json pipeline logs payments-stg --summary
```

`AFT_OPS_DEMO=<path>` does the same without the flag, and
`AFT_OPS_DEMO_LATENCY=<duration>` overrides the fixture's per-call latency so
one fixture can be paced differently per recording.

| File | Role |
| --- | --- |
| `fixture.json` | The fake data. Hand-authored; contains no real account, name, or log |
| `cli.tape` → `cli.gif` | CLI triage: list, filter, JSON, show, logs, release dry-run |
| `tui.tape` → `tui.gif` | TUI drill-down: list → executions → actions → log |
| `tui-release.tape` → `tui-release.gif` | Multi-select Release change, watched to completion |
| `watch.tape` → `watch.gif` | `pipeline list --watch` |
| `record.sh` | The driver: builds a throwaway binary, isolates the environment, runs `vhs` |

## How the substitution works

The fakes are installed at the **AWS SDK client boundary** (`internal/demo`).
They implement the same narrow interfaces the core services already depend on
— `pipeline.API`, `pipeline.StartAPI`, `logs.CodeBuildAPI`, `logs.LogsAPI`,
`account.Source` — so everything above the adapter layer runs unchanged:
normalization, the disk cache, the batch engine with its rate limit, sorting,
both renderers, the whole TUI. Nothing in the tool is special-cased for the
demo, which is what makes one fixture enough to record every scenario.

Two consequences worth knowing:

- **A Release change in demo mode really does start a run.**
  `StartPipelineExecution` prepends an in-flight execution to the fixture in
  memory, and it finishes `release.takes` later. That is why the release
  recording can follow the rows from InProgress to gone rather than miming it.
  The file on disk is never modified.
- **The cache behaves normally.** The fixture's `identity.profile` becomes the
  cache scope, so demo data can never be served under a real profile's scope,
  and the second command in a session legitimately shows `cached 8s ago`.

## The fixture format

Every time is **relative to the moment the fixture is loaded**. A committed
fixture with absolute timestamps would read "3 weeks ago" on every later
recording, and the point of the demo is to look like a live account.

```jsonc
{
  "schema_version": 1,
  "identity": { "account": "…", "region": "…", "profile": "…" },
  "latency": "40ms",              // slept before every fake API call
  "release": { "takes": "28s", "log": "apply-changes" },
  "other_pipelines": ["aft-account-request", …],   // filtered out of the inventory
  "accounts":  [ { "id": "…", "name": "…", "email": "…" } ],
  "pipelines": [ {
    "account_id": "100000000005",   // → 100000000005-customizations-pipeline
    "executions": [ {               // newest first
      "id": "…", "status": "Failed",
      "started_ago": "22m",         // start = load time − this
      "took": "3m12s",              // terminal runs only
      "completes_in": "25s",        // in-flight runs: finish this long after load
      "completes_as": "Succeeded",  // …as this status (default Succeeded)
      "revision": { "action_name": "…", "revision_id": "…", "summary": "…", "url": "…" },
      "actions": [ {
        "stage": "Account-Customizations", "name": "Apply", "status": "Failed",
        "starts_after": "1m15s", "takes": "1m53s",
        "build_id": "aft-account-customizations:<uuid>",  // CodeBuild actions
        "revision_id": "<sha>",                            // source actions instead
        "summary": "…", "error": "…",
        "log": "error-iam-exists"    // key into the shared "logs" map
      } ]
    } ]
  } ],
  "logs": { "error-iam-exists": ["line", …] }
}
```

Notes for anyone editing it:

- `completes_in` is what makes the `--watch` and TUI-poll recordings show the
  auto-refresh doing its actual job. Without it there is nothing to wait for.
- A source action must carry `revision_id` (a bare commit SHA), never
  `build_id`. The colon in a build id is what tells the two apart downstream;
  a SHA with a colon in it would be fed to `BatchGetBuilds`.
- Log bodies are shared by key so the fixture stays small. Each one must keep
  the real AFT shape: the CodeBuild agent preamble, then `terraform init &&
  terraform apply` as a **single** buildspec command, then the terraform
  output. If init and apply were separate commands, the agent line between
  them would truncate the terraform view (see `logs.ExtractTerraform`).
- `internal/demo/demo_test.go` runs against this file, so a fixture that
  drifts out of shape fails the build rather than a recording.

## Re-recording

Needs [`vhs`](https://github.com/charmbracelet/vhs) (`brew install vhs`),
`ffmpeg`, and `go`.

```bash
make demo                                   # all four
bash docs/demo/record.sh docs/demo/cli.tape # one
```

`record.sh` points `XDG_CONFIG_HOME` / `XDG_CACHE_HOME` / `XDG_STATE_HOME` at
a scratch directory and clears every AWS environment variable, so the
recorder's own config — which names a real AWS profile — can never appear on
screen, each tape starts from a cold cache, and a demo path that tried to
reach AWS would fail instead of quietly succeeding against the recorder's
account.

Tuning: the look is the `Set` block at the top of each tape, the pacing is its
`Sleep` values. The terminal has to be wide enough for the widest output in
the tape (`pipeline show` is ~151 columns; columns ≈ `Width / FontSize`) or
the last column wraps.
