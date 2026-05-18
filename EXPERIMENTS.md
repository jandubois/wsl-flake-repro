# Experiment Log

Running record of every workflow run, what it asked, what it found, and how it changed our model. Append new runs at the bottom; revise the **Current understanding** section in place.

## Current understanding

**Confirmed:**

- The bug exists and produces a consistent signature on its most common manifestation: exit code 0, stdout 0 bytes, **stderr also 0 bytes**, normal latency (~120 ms). Total silent loss of the call's output.
- That common failure shape affects every I/O collection mechanism we have tested: Go `bytes.Buffer` (goroutine pipe-copy), Go `cmd.CombinedOutput()`, Go `cmd.Stdout = os.File` (no goroutine; inherited file fd), and bash `$(...)` capture. The bug is on the *sending* side — `wsl.exe`'s relay — not on the parent's receiving side.
- For the common failure shape, a single retry recovers it. Across Runs 6-7 (54 events) and Run 9 (6 events), every empty-stdout event recovered on attempt 2.

**Qualified (Run 11 update):**

- **A second, rarer failure shape exists.** Run 11 saw one event where the first attempt returned a non-nil error (not deadline-exceeded) and no subsequent retries could recover. The old harness shared a 30-second parent context across retries, so we cannot distinguish "wsl.exe actually hung" from "retry implementation poisoned itself" for that event. New harness gives each attempt its own context; the next such event will be diagnosable.

**Refuted / ruled out:**

- *Go's `WaitDelay` machinery causes it.* Initial 2-flake clustering in `wait-delay` mode at n=24k was within sampling noise; Run 4 showed identical rates across `plain` / `wait-delay-5s` / `wait-delay-30s`.
- *Go's pipe-copy goroutine causes it.* `file-out` (no goroutine) flakes at the same rate as `bytes.Buffer`-based modes when sampled to sufficient n (Run 5).

**Reduced to "we can't tell":**

- *`terminate-each` suppresses the bug.* Run 9 saw 0/128k under terminate-each vs many events under other scenarios in the same fleet conditions. Run 10 tried to confirm with a same-run baseline — and saw 0/300k under serial-baseline too. So the Run 9 terminate-each signal looks like a low-rate day, not a real suppression effect. Cannot confirm or refute terminate-each as a mitigation.
- *Concurrent contention reduces the rate.* Same story — Run 9's parallel-4 rate of 1/50k vs serial 1/6.4k looked striking, but the baseline rate has since moved by 50× across runs. We can't trust any rate comparison across runs.

**The bigger issue surfaced by Run 10:** the bug rate is wildly non-stationary across runs, with at least 50× variation observed (Run 7 serial: 47/300k = 1/6400; Run 10 serial: 0/300k = <1/300k, same code, same call shape). We have a genuine bug but no reliable on-demand reproducer. Comparisons across runs are unreliable; only same-run comparisons are trustworthy, and they require enough events on a high-rate day to be meaningful.

**Open questions:**

- What controls the per-day rate? Runner fleet rotation is the leading suspect.
- Is there a way to detect "high-rate" conditions and only run comparison experiments then?
- Mechanistic hypothesis (unproven): `wsl.exe`'s user-mode relay swallows exceptions in the CATCH_LOG paths at thread boundaries. Both stdout and stderr relay threads die on the same event, producing exactly this signature. Not yet verified against MS WSL source.

**Mechanistic hypothesis (unproven):** `wsl.exe`'s user-mode relay copies bytes from the in-distro process's vsock output to the Windows-side handle the spawn inherited. The CATCH_LOG paths in `src/windows/common/relay.cpp` swallow exceptions at the relay thread boundary — if both the stdout and stderr relay threads die on the same event (consistent with the stderr-also-empty observation), we'd see exactly this signature. Not yet confirmed against MS WSL source step-through.

## Cumulative sample

Counts cover all completed cells through Run 10. Pipe-mode includes any
mode that captures stdout via a pipe or via bash `$(...)`; file-out is
listed separately for historical reasons (Run 4 hypothesis it might be
immune, since refuted).

| capture path                | iters     | events | aggregate rate |
| --------------------------- | --------- | ------ | -------------- |
| pipe-mode (Go bytes.Buffer + CombinedOutput + bash-pipe + Go retry) | ~1,087,000 | 97     | ~1/11,000 |
| file-out (Go inherited fd)  | 60,000    | 12     | 1/5,000        |
| terminate-each (any mode)   | 398,000   | 0      | <1/398,000     |

Aggregate rates are roughly meaningful only for capture-mechanism
comparisons. The per-run variance is so large that a single aggregate
rate hides the underlying instability.

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

### Run 10 — same-run baseline vs terminate-each (workflow run [25986011686](https://github.com/jandubois/wsl-flake-repro/actions/runs/25986011686))

**Question:** Is terminate-each's near-zero rate real, or was it within-day fleet variance compared to a different-day serial baseline?

**Matrix:** 10 runners × 2 scenarios × 30k iters serial each. Both scenarios in the same workflow run so fleet conditions are identical. `retry-serial-baseline` (no terminate) is the control; `retry-terminate-each` is the treatment. Job timeout bumped to 360 min because terminate-each at 30k iters takes ~4.5 hours per cell.

**Results:** **Zero events across both scenarios.** 19 of 20 cells finished; one terminate-each cell (runner 7) hit the 360-min timeout and was cancelled.

| scenario              | iters   | events |
| --------------------- | ------- | ------ |
| retry-serial-baseline | 300,000 | 0      |
| retry-terminate-each  | 270,000 | 0      |

**Learned:** Today was a low-rate day across the entire windows-latest fleet. The serial-baseline at 0/300k is incompatible with Run 7's serial-baseline at 47/300k under any single-rate model — rate moved by at least 50×. Same code, same harness, same call shape, different day.

**Changed:** Run 9's "terminate-each suppresses the bug" finding can no longer be claimed as a real effect — today's serial-baseline was also zero. Same applies to Run 9's "parallel-4 has lower rate than serial." Both look like fleet variance, not mode-specific effects. We have a bug we can sometimes reproduce but cannot reproduce on demand.

### Run 11 — rerun with runner identification (workflow run [25999304336](https://github.com/jandubois/wsl-flake-repro/actions/runs/25999304336))

**Question:** Repeat Run 10's same-run comparison; capture per-cell runner identity (image, WSL version, computer name) in case fleet variance correlates with anything visible.

**Matrix:** Identical to Run 10. New per-cell step writes `runner-info.txt` to the artifact.

**Results:**

| scenario              | iters   | events |
| --------------------- | ------- | ------ |
| retry-serial-baseline | 300,000 | 0      |
| retry-terminate-each  | 300,000 | **1**, unrecovered |

The single event broke the streak — and broke it in a way nothing else had:

```
iter 15244, runner 8, duration 51ms (last attempt), exit -1
attempts: ERR, TIMEOUT, TIMEOUT, TIMEOUT, TIMEOUT, TIMEOUT,
          TIMEOUT, TIMEOUT, TIMEOUT, TIMEOUT
err: context deadline exceeded
```

First attempt: `ERR` (non-nil error other than `context.DeadlineExceeded`). Next nine attempts: `TIMEOUT`.

**Two findings, one investigation flaw, one new failure mode:**

1. **The harness shared a 30-second parent context across all retry attempts.** A slow first attempt that exhausted the context poisoned every subsequent retry — they hit `context.DeadlineExceeded` immediately. So the `TIMEOUT × 9` pattern doesn't tell us the underlying wsl.exe behavior; it tells us the retry implementation was broken. Fixed by giving each attempt its own context (commit after Run 11). Per-attempt `first_failed_err` column added to capture the first attempt's err on future events.

2. **A new wsl.exe failure shape exists beyond exit-0/empty-stdout.** The first attempt's error was *something* — non-nil but not deadline-exceeded. We lost the actual error message because the old harness only kept the last attempt's err. With the new harness we will see it next time.

**Runner-info verdict:** every cell stamped identical (Windows Server 2025 Datacenter, image `20260510.128.1`, WSL 2.7.3.0, computer_name `runnervmp4gaq`). The capture doesn't discriminate today's fleet members. Useful only as a baseline for future runs that might show heterogeneous fields.

**Changed:** The "retry catches every flake" claim from Runs 6-7 has to be qualified — those runs captured only the empty-stdout failure shape. A retry-blocking failure shape exists and we now know to watch for it.
