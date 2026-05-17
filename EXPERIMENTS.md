# Experiment Log

Running record of every workflow run, what it asked, what it found, and how it changed our model. Append new runs at the bottom; revise the **Current understanding** section in place.

## Current understanding

**Confirmed:**

- The bug exists and produces a consistent signature: exit code 0, stdout 0 bytes, **stderr also 0 bytes**, normal latency (~120 ms). Total silent loss of the call's output.
- The bug affects every I/O collection mechanism we have tested: Go `bytes.Buffer` (goroutine pipe-copy), Go `cmd.CombinedOutput()`, Go `cmd.Stdout = os.File` (no goroutine; inherited file fd), and bash `$(...)` capture. Same rate within an order of magnitude across all four. The bug is on the *sending* side — `wsl.exe`'s relay — not on the parent's receiving side.
- A single retry recovers the lost call. Across all retry runs to date, no event has needed more than one retry to succeed. Per-retry failure rate is bounded below ~5% with 95% confidence.

**Refuted / ruled out:**

- *Go's `WaitDelay` machinery causes it.* Initial 2-flake clustering in `wait-delay` mode at n=24k was within sampling noise; later runs at higher n showed identical rates across `plain` / `wait-delay-5s` / `wait-delay-30s`.
- *Go's pipe-copy goroutine causes it.* `file-out` (no goroutine) flakes at the same rate as `bytes.Buffer`-based modes when sampled to sufficient n.
- *Concurrent contention amplifies it.* `parallel-4` against the same distro produced *fewer* events per call than serial (1/50k vs 1/6.4k in the same fleet conditions).
- *Distro cold restart triggers it.* `terminate-each` hint suggests cold restart may *suppress* the bug, not trigger it.

**Open questions:**

- Why does the per-call rate vary 25× across days (1/2000 to <1/128k)? Probable suspects: runner fleet rotation, WSL build differences, host load.
- Does `terminate-each` genuinely suppress the bug, or did we sample too few events to tell?
- What state inside `wsl.exe` / `wslservice.exe` / the distro VM might accumulate across warm calls and reset on terminate?
- Is the bug correlated with call frequency (calls per unit time) rather than call count?

**Mechanistic hypothesis (unproven):** `wsl.exe`'s user-mode relay copies bytes from the in-distro process's vsock output to the Windows-side handle the spawn inherited. The CATCH_LOG paths in `src/windows/common/relay.cpp` swallow exceptions at the relay thread boundary — if both the stdout and stderr relay threads die on the same event (consistent with the stderr-also-empty observation), we'd see exactly this signature. Not yet confirmed against MS WSL source step-through.

## Cumulative sample

Counts cover all completed cells through Run 9. Pipe-mode includes any
mode that captures stdout via a pipe or via bash `$(...)`; file-out is
listed separately because of the earlier (now refuted) hypothesis it
might be immune.

| capture path                | iters    | events | rate    |
| --------------------------- | -------- | ------ | ------- |
| pipe-mode (Go bytes.Buffer + CombinedOutput + bash-pipe + Go retry) | ~850,000 | 105    | ~1/8100 |
| file-out (Go inherited fd)  | 60,000   | 12     | 1/5000  |
| terminate-each (any mode)   | 128,000  | 0      | <1/128k |

## Runs

### Run 0 — original BATS failure (2026-05-15)

**Context:** `rancher-desktop-daemon` BATS run [25932521337](https://github.com/rancher-sandbox/rancher-desktop-daemon/actions/runs/25932521337), test 78 `start VM for crash recovery test` in `33-lima-controllers/limavm-running.bats`, windows-latest.

**What happened:** Lima's `provisionVM` in `pkg/driver/wsl2/vm_windows.go` invokes `wsl.exe -d <distro> bash -c "wslpath -u <winpath>"` to translate a host tempfile path to a Linux path. The hostagent log showed `[wsl.exe -d lima-test-running bash -c ]: ""` — empty path argument. Lima had received empty stdout from the wslpath call (with `nil` error), trimmed it, and silently dispatched the boot script via `bash -c ""`, which does nothing. The VM never booted and the test hung for 5 minutes.

**Learned:** The class of bug — Win32 binary stdout silently empty in a parent process — was familiar; Jan's 2023 rancher-desktop comment in `bats/tests/helpers/paths.bash` documents the same with `cmd.exe` returning empty when called from a bash subshell.

### Run 1 — first harness baseline (2026-05-17, workflow run [25951861629](https://github.com/jandubois/wsl-flake-repro/actions/runs/25951861629))

**Question:** Can we reproduce the empty-stdout event in isolation, outside Lima?

**Matrix:** 7 modes × 2000 iters serial = 14,000 calls. Modes: `plain`, `wsl-utf8`, `wait-delay`, `stdin-null`, `cmd-shim`, `powershell-shim`, `retry`.

**Results:** 2 flake events in 12,000 non-error calls. Both in `wait-delay` mode. `powershell-shim` failed 2000/2000 with mangled backslashes (a quoting bug, not a flake).

**Learned:** The bug reproduces in isolation; rate is roughly 1/6000. The clustering in `wait-delay` was suspicious.

**Changed:** First mechanistic hypothesis: WaitDelay machinery exposes a Go-side race.

### Run 2 — terminate/restart + bigger output (workflow run [25952892821](https://github.com/jandubois/wsl-flake-repro/actions/runs/25952892821))

**Question:** Do cold restarts or larger output sizes amplify the rate?

**Matrix:** 4 scenarios × all modes. Added `--terminate-each` and `--bytes 4096`. Fixed half of `powershell-shim` (handled `$()` expansion, did not fix backslash handling).

**Results:** 2 flakes total across the run; both in `wait-delay` mode again. terminate-each and bigger-output didn't change the rate.

**Learned:** Neither cold restart nor output size appears to amplify the bug. WaitDelay theory still alive after second clustering observation.

### Run 3 — high-pressure retry stress (workflow run [25954422850](https://github.com/jandubois/wsl-flake-repro/actions/runs/25954422850))

**Question:** Does parallelism raise the rate? Does retry work under load?

**Matrix:** 5 cells: serial-20k-plain, serial-20k-retry, parallel-4-20k, parallel-8-20k, parallel-8-retry-50k. 130,000 total calls.

**Results:** **Zero flake events.** Across all five cells.

**Learned:** Bug rate is highly variable. P(0 events | rate=1/6k, n=130k) ≈ 2×10⁻⁵, so either the rate had genuinely dropped or our earlier estimate was high.

**Changed:** "We have a clean rate measurement" was wrong — the rate is non-stationary across days/runs. Wait-delay-specific theory weakened (small n in earlier observations).

### Run 4 — WaitDelay isolation (workflow run [25970243609](https://github.com/jandubois/wsl-flake-repro/actions/runs/25970243609))

**Question:** Is WaitDelay specifically implicated? What about other Go I/O paths?

**Matrix:** 5 cells × 30k iters serial. Modes: `plain`, `wait-delay-5s`, `wait-delay-30s`, `combined-output`, `file-out`.

**Results:**

| mode             | events / 30k | rate    |
| ---------------- | ------------ | ------- |
| plain            | 7            | 1/4285  |
| wait-delay-5s    | 7            | 1/4285  |
| wait-delay-30s   | 8            | 1/3750  |
| combined-output  | 9            | 1/3333  |
| **file-out**     | **0**        | 0/30000 |

**Learned:** `WaitDelay` theory officially dead — same rate across plain and both WaitDelay variants. `file-out` (no goroutine) being clean looked like a strong signal that Go's pipe-copy goroutine was responsible.

**Changed:** Mechanistic theory shifted from WaitDelay to "Go's pipe-copy goroutine is the bug."

### Run 5 — confirm file-out + bash-pipe (workflow run [25972680139](https://github.com/jandubois/wsl-flake-repro/actions/runs/25972680139))

**Question:** Was file-out really immune? Does bash `$(...)` capture flake too?

**Matrix:** 2 cells. `file-out` × 30k (repeat); `bash-pipe` × 15k (Git Bash wraps `wsl.exe` in `$(...)`, outer Go captures via file-out so any empty result came from bash's pipe).

**Results:**

| scenario       | events / total | rate   |
| -------------- | -------------- | ------ |
| file-out       | 12 / 30k       | 1/2500 |
| bash-pipe      | 4 / 15k        | 1/3750 |

**Learned:** `file-out` was *not* immune — earlier 0/30k was statistical fortune (P(0 | 1/4k, n=30k) ≈ 0.5%). Bash's `$(...)` also flakes. Same rate across pipe, file, and bash capture mechanisms.

**Changed:** Pipe-copy theory dead. Bug must be on the sending side (wsl.exe's relay), not on the parent's receiving side — wsl.exe is the same binary in every mode, and the parent's choice of capture mechanism doesn't matter. Jan's earlier framing — "it's about calling wsl.exe in a pipe regardless of language" — confirmed.

### Run 6 — first retry measurement (workflow run [25974507048](https://github.com/jandubois/wsl-flake-repro/actions/runs/25974507048))

**Question:** When retry fires, does it always recover? How many attempts are typically needed?

**Matrix:** 1 cell, `retry` × 40k iters serial, `--max-retries 10`.

**Results:** 7 events. All 7 recovered on attempt 2. `attempts` column for every event: `EMPTY_STDOUT_NIL_ERR,OK`. Inter-event gaps 54s to 2048s — roughly Poisson, no clustering.

**Learned:** Single retry recovered every observed flake. n=7 is small for confidence; bounds the per-retry failure rate at ≤21% with this sample.

### Run 7 — fan-out retry across 10 runners (workflow run [25977497788](https://github.com/jandubois/wsl-flake-repro/actions/runs/25977497788))

**Question:** Tighten the bound on per-retry failure rate; check for runner-to-runner variance.

**Matrix:** 10 runners × `retry` × 30k iters = 300,000 calls, all serial, `--max-retries 10`.

**Results:** 47 events combined. **All 47 recovered on attempt 2.** Per-runner counts: 2, 5, 3, 3, 9, 3, 3, 9, 7, 3 (mean 4.7, range 2–9) — within Poisson variance for a single rate.

**Learned:** Per-retry failure rate ≤ 6% at 95% confidence. Post-retry residual failure rate < 1/100,000. No runner-to-runner anomaly. Under quiet steady-state conditions, retry recovers every flake.

### Run 8 (cancelled) — billing failure (workflow run [25982288354](https://github.com/jandubois/wsl-flake-repro/actions/runs/25982288354))

**Context:** Dispatched 20-cell matrix to test retry under contention. All 20 cells refused to start: "the job was not started because recent account payments have failed or your spending limit needs to be increased."

**Cause:** The repo was private (Claude's earlier "default to private to be safe" choice when creating it). Private repos burn the user's GitHub Actions spending cap; public repos run free on hosted runners. The 10-cell Run 7 had already consumed most of Jan's monthly quota.

**Fix:** Made repo public. Recorded in global memory under `github_repo_visibility_cost.md`.

### Run 9 — retry under contention + cold restart (workflow run [25982937293](https://github.com/jandubois/wsl-flake-repro/actions/runs/25982937293))

**Question:** Does retry hold under Lima-like conditions: concurrent `wsl.exe` calls (`parallel-4`) and fresh-restart distros (`terminate-each`)?

**Matrix:** 10 runners × 2 scenarios = 20 cells. retry-parallel-4 × 30k completed. retry-terminate-each × 25k all hit 120-min timeout (cancelled with partial data: 11k–17k iters each).

**Results:**

| scenario        | events / iters | rate     | recovery |
| --------------- | -------------- | -------- | -------- |
| parallel-4      | 6 / 300,000    | 1/50,000 | 6/6 on attempt 2 |
| terminate-each  | 0 / 128,000    | <1/128k  | n/a      |

**Learned:** Two surprises. (1) `parallel-4` rate was 8× *lower* than serial in the same-day Run 7 conditions — concurrent calls apparently *don't* amplify the bug. (2) `terminate-each` showed zero events across 128k partial iters; if the underlying rate matched the serial rate of 1/6.4k, P(0 events) ≈ e⁻²⁰ ≈ 2×10⁻⁹. Cold restart appears to suppress the bug substantially. Compensating cost: terminate adds ~430ms per call.

**Changed:** New open question — what state accumulates across warm calls that cold restart wipes? Hypothesis (unverified): something in wslservice.exe or the distro VM persists across calls and contributes to the failure mode.

**Note on artifact-name bug:** Last run's upload step referenced `matrix.scenario` instead of `matrix.scenario_name`, so all 20 artifacts collided on 10 names. Data was salvaged by downloading via artifact ID and classifying by file size (completed cells were larger than cancelled ones).

### Run 10 — same-run baseline vs terminate-each (workflow run [25986011686](https://github.com/jandubois/wsl-flake-repro/actions/runs/25986011686), in progress)

**Question:** Is terminate-each's near-zero rate real, or was it within-day fleet variance compared to a different-day serial baseline?

**Matrix:** 10 runners × 2 scenarios × 30k iters serial each. Both scenarios run in the *same workflow run* so fleet conditions are identical. `retry-serial-baseline` (no terminate) provides the control; `retry-terminate-each` (terminate before each iter) is the treatment.

**Job timeout bumped to 360 min** because terminate-each at 30k iters takes ~4.5 hours per cell.

**Result:** TBD.
