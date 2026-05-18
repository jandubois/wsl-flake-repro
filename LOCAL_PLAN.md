# Local Windows run plan

Step-by-step instructions for a Claude Code session running on Jan's local Windows machine. The purpose: compare the wsl.exe flake rate on a single physical machine against today's GitHub Actions cells, to tell whether the recent run of low-rate days is environment-wide or specific to GHA's runner fleet.

Read [EXPERIMENTS.md](./EXPERIMENTS.md) for the investigation history if you want context. This document focuses on what to do.

## Prerequisites

Check, do not assume:

```pwsh
go version              # Need Go 1.22+
wsl.exe --version       # Need WSL2 with at least one distro
wsl.exe --list --quiet  # Pick a distro that has bash; print to Jan
```

If no distro has bash, install bash inside one Jan picks (most have it; Alpine does not by default — `wsl.exe -d <distro> -- /sbin/apk add bash`).

Report the distro name back to Jan and confirm before continuing — do not pick blindly.

## Build the harness

```pwsh
go build -o wsl-flake-repro.exe .
```

If it fails, stop and surface the error.

## Run scenarios

Two scenarios mirroring GHA cells. Run them in order, not in parallel — we want clean per-machine measurements without the load of two concurrent runs interfering.

Create `local-results/` (gitignored) for the TSVs.

```pwsh
New-Item -ItemType Directory -Force -Path local-results | Out-Null
```

### Scenario 1: plain serial baseline

```pwsh
.\wsl-flake-repro.exe `
  --distro <distro> `
  --mode plain `
  --iters 30000 `
  --out local-results/plain.tsv
```

Expected wall clock: 30k iters × 50–150ms per call ≈ 25–75 min. Print a status line at the start and check on it occasionally with `Get-Content local-results/plain.tsv -Tail 1` so Jan can see progress.

### Scenario 2: retry serial baseline (after scenario 1 finishes)

```pwsh
.\wsl-flake-repro.exe `
  --distro <distro> `
  --mode retry `
  --max-retries 10 `
  --iters 30000 `
  --out local-results/retry.tsv
```

Similar wall clock.

## Analyze each scenario when it finishes

For both TSVs, compute:

```pwsh
# How many flake events; what attempt patterns appeared
Import-Csv local-results/plain.tsv -Delimiter "`t" |
  Where-Object { [int]$_.retries -gt 0 -or $_.class -ne "OK" } |
  Select-Object iter, duration_ms, exit_code, stdout_len, stderr_len, class, attempts, first_failed_err, first_failed_stderr |
  Format-Table -AutoSize
```

Get totals:

```pwsh
$tsv = Import-Csv local-results/plain.tsv -Delimiter "`t"
"total:   $($tsv.Count)"
"OK:      $(($tsv | Where-Object class -eq 'OK').Count)"
"EMPTY:   $(($tsv | Where-Object class -eq 'EMPTY_STDOUT_NIL_ERR').Count)"
"ERR:     $(($tsv | Where-Object class -eq 'ERR').Count)"
"TIMEOUT: $(($tsv | Where-Object class -eq 'TIMEOUT').Count)"
```

For the retry TSV, also count events that needed retries:

```pwsh
$tsv = Import-Csv local-results/retry.tsv -Delimiter "`t"
"retries-fired: $(($tsv | Where-Object { [int]$_.retries -gt 0 }).Count)"
"attempt-patterns:"
$tsv | Where-Object { [int]$_.retries -gt 0 } | Group-Object attempts |
  Select-Object Count, Name | Format-Table -AutoSize
```

## What to report back

For each scenario, report:

- total iters
- event count by class (OK / EMPTY_STDOUT_NIL_ERR / ERR / TIMEOUT / other)
- per-event details for any non-OK rows: iter, duration, attempts, first_failed_err, **first_failed_stderr** (this is the field we have never seen — capture it carefully)

Also report the runner identity for cross-reference:

```pwsh
wsl.exe --version
Get-CimInstance Win32_OperatingSystem | Select-Object Caption, Version, BuildNumber
$env:COMPUTERNAME
```

## Decision tree

Once both scenarios finish:

- **Both 0 events.** Today is a low-rate day everywhere. Tell Jan; don't extend the experiment without his go-ahead.
- **Plain shows events, retry shows 0.** Suspicious — retry mode is somehow suppressing the bug. Capture an event row from plain (especially `first_failed_stderr`) and surface it.
- **Both show events.** Best case — we have a reproducer. Capture every non-OK row in full, with special attention to any `first_failed_stderr` for exit-1 events. This is data the GHA-only investigation could not collect.
- **Either shows TIMEOUT events.** Surface the row immediately and stop — the per-attempt context fix from commit `043bfdc` should prevent cascading TIMEOUTs, so any TIMEOUT means a single attempt actually took >30s. That's a real wsl.exe hang, distinct from the empty-stdout and exit-1 shapes.

## Do not

- Commit the `local-results/` TSVs. They're gitignored. Jan will decide what (if anything) is worth promoting into the repo.
- Push anything from the local machine. The remote already has the harness; the only thing this session adds is data observation.
- Re-run a scenario without asking. If a run produces an interesting event, the next step is for Jan to look at it, not for us to immediately re-run hoping for more.
- Extend iter counts past 30k unilaterally. Long runs cost real machine time on Jan's hardware.

## After both scenarios finish

Hand the results back to Jan with a short summary and the raw per-event rows. He decides whether to update EXPERIMENTS.md with the comparison or whether to run more scenarios.
