# Experiment Log

Running record of every workflow run, what it asked, what it found, and how it changed our model. Append new runs at the bottom; revise the **Current understanding** section in place.

## Current understanding

There appear to be **two distinct failure modes**, not one.

### Mode A: empty stdout (the original bug)

- Signature: exit 0, stdout 0 bytes, stderr 0 bytes, normal latency (~120 ms). Total silent loss of the call's output.
- Affects every I/O collection mechanism tested — `bytes.Buffer`, `CombinedOutput`, file-out (inherited fd), bash `$(...)`. The byte loss is on the *sending* side, in `wsl.exe`'s user-mode relay; not on the parent's receiving side.
- Historical rate ~1/4k on GHA windows-latest; varies widely across runs and days.
- **Recent GHA rate: low but non-zero.** Run 15 captured 1/300k in plain mode. Rate has dropped ~75× from the historical baseline, but the bug still fires.
- **Never observed on Jan's local workstation.** Across all local runs (Run 14, 14b, 15b), zero events in 180k+ iters. Could be hardware-class dependent (constrained cloud VMs vs abundant-resource workstation), specific to GHA's Hyper-V configuration, or another factor we have not isolated.
- One retry recovers reliably whenever this shape fires (all observed Mode A events recovered on attempt 2).

### Mode B: exit 1 (newer, rarer)

- Signature: exit 1, stdout 0 bytes, stderr 0 bytes, recovering attempt ~22 seconds. The captured `first_failed_stderr` is empty — bash printed nothing before exiting.
- Observed rate ~1/300k.
- 3 of 3 observed events under terminate-each; zero under serial-baseline or plain-baseline. Strong correlation with cold-restart conditions, weak sample size.
- Needs two retries (recovers on attempt 3 in both observed cases), with a ~22-second recovering attempt. Likely mechanism: terminate signals the distro to shut down, the next wsl.exe call races a still-shutting-down distro, wsl.exe fails to attach, attempt 3 waits for WSL's internal timeout to clear the state.

### Cross-cutting

- A 2-attempt retry budget is insufficient for Mode B. Mitigation specs should allow at least 3.
- The harness has been progressively de-bugged through this investigation: per-attempt context (commit 043bfdc) and per-attempt stderr capture (commit c0649dc) were both needed to characterize Mode B.

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
| pipe-mode, GHA (Go bytes.Buffer + CombinedOutput + bash-pipe + Go retry) | ~1,687,000 | 97     | ~1/17,000 |
| pipe-mode, local Windows (plain + retry, 2026-05-18) | 60,000    | 0      | <1/60,000 |
| file-out (Go inherited fd)  | 60,000    | 12     | 1/5,000        |
| terminate-each (any mode)   | ~1,238,000  | 3 (all Mode B) | ~1/410,000 |

Aggregate rates are roughly meaningful only for capture-mechanism
comparisons. The per-run variance is so large that a single aggregate
rate hides the underlying instability — especially the post-Run-9
silence in Mode A.

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

### Run 12 — rerun with per-attempt contexts + first_failed_err (workflow run [26009188480](https://github.com/jandubois/wsl-flake-repro/actions/runs/26009188480))

**Question:** With the harness fix in place (per-attempt context; first_failed_err column), what does the other failure shape actually look like?

**Matrix:** Identical to Runs 10-11.

**Results:**

| scenario              | iters   | events |
| --------------------- | ------- | ------ |
| retry-serial-baseline | 300,000 | 0      |
| retry-terminate-each  | 300,000 | **1**, recovered |

The single event:
```
iter 12666, runner 5
attempts: ERR, ERR, OK
final attempt duration: 22,770 ms
first_failed_err: exit status 1
```

**Learned:**

1. The harness fix works — no cascading TIMEOUTs this round. Each attempt got its own 30s context, so attempts 2 and 3 ran cleanly instead of inheriting an exhausted parent.
2. The non-empty-stdout failure shape is `exit status 1`, not a hang. The bash invocation inside `wsl.exe` exited non-zero. We still don't capture per-attempt stderr, so we don't know *why* bash exited 1.
3. Recovery needed TWO retries (attempt 3 was the OK one). Every flake before Run 11 had recovered on attempt 2. The "single retry always recovers" claim from Runs 6-7 was measuring only the empty-stdout shape; the exit-1 shape behaves differently and sometimes needs more retries.
4. The recovering attempt took 22.77 seconds — 40× slower than a normal terminate-each iter (~550ms). Likely the post-terminate distro state needed time to settle after the prior failures.
5. Empty-stdout shape: zero events for the third run in a row across the combined 1.5M sample (Runs 10-12).

**Changed:** "Retry recovers on attempt 2" is now "retry recovers, sometimes on attempt 2, sometimes on attempt 3, in our small sample." Mitigation specifications should not hard-code a max-retry of 2. Per-attempt stderr capture is the next obvious diagnostic gap: we'd like to know what bash actually said when it exited 1.

### Run 13 — environmental control + first_failed_stderr (workflow run [26064103744](https://github.com/jandubois/wsl-flake-repro/actions/runs/26064103744))

**Question:** Is the recent low rate environmental or specific to our test changes? Capture the first-failed attempt's stderr so we can finally see what bash said on exit-1 events.

**Matrix:** 10 runners × 3 scenarios = 30 cells. Added `plain-baseline` (no retry wrapper) alongside the two retry scenarios.

**Results:**

| scenario              | iters   | events |
| --------------------- | ------- | ------ |
| plain-baseline        | 300,000 | 0      |
| retry-serial-baseline | 300,000 | 0      |
| retry-terminate-each  | 270,000 | **1**, recovered |

One terminate-each cell cancelled at the 360-min timeout (same slow-cell behavior as Runs 10-12).

The single event:
```
iter 18118, runner 2, terminate-each
attempts: ERR, ERR, OK
final attempt duration: 22.32 seconds
first_failed_err:    exit status 1
first_failed_stderr: (empty)
```

**Learned:**

1. **The low rate is environment-wide, not retry-mode specific.** plain-baseline at 0/300k in the *same workflow run* as retry-serial-baseline at 0/300k closes that hypothesis. Both modes capture wsl.exe stdout the same way — only retry adds the retry loop. Same observable rate means the wrapper isn't doing anything special.
2. **First-failed-stderr finally captured — and it's empty.** Bash exited 1 with no stderr output. That rules out a "bash printed an error we missed" theory and points at wsl.exe itself or the distro state, not bash's exit logic.
3. **The exit-1 events now look like a distinct, reproducible failure mode.** Runs 12 and 13 match almost exactly: both terminate-each, both ERR,ERR,OK, both recovered on attempt 3, both with ~22-second recovering attempts (22.77s and 22.32s). 3 of 3 observed exit-1 events have been under terminate-each. The mechanism is plausibly: terminate signals the distro to shut down, the next call races the shutdown, wsl.exe fails to attach, and the third attempt waits for whatever WSL-internal timeout clears the state.

**Changed:** We probably have *two* distinct failure modes, not one. The empty-stdout shape (variable rate, mode-agnostic, single-retry recovery) and the exit-1 shape (~1/300k, terminate-each only, two-retry recovery with ~22s settle time). EXPERIMENTS.md should stop treating them as one bug.

### Run 14 — local Windows comparison (2026-05-18, Jan's DESKTOP-R0D3F8V)

**Question:** Is the recent zero-event streak GHA-fleet-specific or environment-wide?

**Setup:**

- Host: DESKTOP-R0D3F8V (Windows 11 Pro, build 26100.8457)
- WSL: 2.7.3.0, kernel 6.6.114.1-1 — same as GHA fleet today
- Distro: Ubuntu
- CPU: Intel Xeon E5-2650L v3 (2014, Haswell-EP, 12C/12T @ 1.80 GHz) — substantially slower than the modern hardware behind GHA runners
- Power plan: Balanced; Defender real-time protection: on

**Matrix:** plain @ 30k iters serial; retry @ 30k iters with `--max-retries 10`. Both run on the same machine, one after the other.

**Results:**

| scenario | iters  | events | wall clock | avg/call |
| -------- | ------ | ------ | ---------- | -------- |
| plain    | 30,000 | 0      | 130.9 min  | 261.2 ms |
| retry    | 30,000 | 0      | 131.8 min  | 262.8 ms |

**Learned:**

1. **The low rate is not GHA-specific.** Same calendar day, same WSL version, same zero rate locally — environment-wide, not fleet-side.
2. **Local at 262 ms/call shows zero events.** Earlier theories tying GHA's flake rate specifically to per-call latency weakened, but the comparison is limited: this machine has never flaked, so it cannot rule speed in or out as a driver. A machine that has historically produced events under one latency profile would be needed to test the latency claim properly.
3. **WSL version is the most plausible remaining lever for Mode A's variability.** Both environments today are on WSL 2.7.3.0. A controlled test with an older WSL service version on the same physical machine would tell us whether Microsoft fixed (or masked) the empty-stdout bug in a recent service update.

**Changed:** "GHA fleet rotation" demoted as the leading explanation for run-to-run rate variance. WSL service version moved up.

### Run 14b — local latency sweep across power/Defender configs (2026-05-18)

**Question:** Does the local machine's slow ~262 ms/call latency reflect the hardware, the power plan, or Defender's I/O hooking?

**Method:** Short timing probe (no full 30k run) under three configurations on DESKTOP-R0D3F8V.

**Results:**

| config                              | min ms | avg ms | max ms |
| ----------------------------------- | ------ | ------ | ------ |
| Baseline (Balanced + Defender on)   | 176    | 261    | 3978   |
| High Performance + Defender on      | 167.5  | 176    | 184.4  |
| High Performance + Defender off     | 156.6  | 163    | 170.7  |

**Learned:**

1. **Power plan dominates.** Switching Balanced → High Performance dropped average latency by ~33%. Most of the local-vs-GHA latency gap was the laptop CPU clocking down on idle, not the hardware itself.
2. **Defender adds ~13 ms (~7%) on top.** Real-time scanning hooks process spawn and stdio. Small in averages, but it does alter the timing windows around each call.
3. **Variance collapsed.** Baseline had 3,978 ms outliers (presumably warm-state interruptions when background services woke up). Post-tuning, every warm call is within ±10 ms of the average. If the bug is sensitive to specific timing windows, a stable-timing configuration may or may not still hit them.

**Decision:** Did not run a full 30k under the tuned config today. GHA also showed 0 events today, so a same-day local run would just record another 0/0. The "does config affect rate" question is testable only on an event-positive day; deferred until events return.

### Run 15 — Mode A returns; harness-regression check (HEAD vs d55d70d) (2026-05-19)

**Question:** Are we still running the same tests as Run 4 (which saw 7 events in 30k)? Sanity check by running the d55d70d harness against today's matrix alongside HEAD.

**Setup:** Two parallel workflow dispatches.

- HEAD harness: workflow run [26080236389](https://github.com/jandubois/wsl-flake-repro/actions/runs/26080236389) — 30 cells (10 runners × 3 scenarios), full HEAD harness.
- Old harness (d55d70d, the Run 4 code): workflow run [26080233967](https://github.com/jandubois/wsl-flake-repro/actions/runs/26080233967) — same matrix; the workflow checks out main.go at d55d70d before building.

**Results:**

HEAD run:

| scenario              | iters   | events |
| --------------------- | ------- | ------ |
| plain-baseline        | 300,000 | **1** (Mode A) |
| retry-serial-baseline | 300,000 | 0      |
| retry-terminate-each  | 300,000 | 0      |

The Mode A event: `plain-baseline` runner 2, iter 1636, duration 126 ms, exit 0, stdout 0 bytes, stderr 0 bytes. Classic signature.

Old-harness run: **failed to start.** All 30 cells errored at the new "swap in older harness" workflow step because PowerShell parsed bash-style `2>/dev/null` as `Out-File -FilePath null`. Fixed by adding `shell: bash` to the step; re-dispatch pending.

**Learned:**

1. **Mode A is not gone, just rare.** First empty-stdout event in many runs. At 1/300k today versus ~1/4k historically, the rate has dropped ~75× but stayed non-zero. The earlier streak of zeros was a low-rate window, not extinction.
2. **plain mode still observes Mode A.** Same code as Run 4, same conditions, the test reproduces the bug when conditions cooperate. Test integrity is not in question.
3. **Retry mode showed zero with the same 300k iters.** At a 1/300k underlying rate, P(retry sees 0 | rate=1/300k, n=300k) ≈ 37%, so consistent with chance. No basis yet for claiming retry mode behaves differently from plain on the underlying call.
4. **The old-harness sanity check is still useful** because all the post-Run-4 harness changes touch only the retry path. The plain-mode call shape is byte-identical between d55d70d and HEAD. Once the workflow swap step is fixed, the comparison still answers "would old code see events under today's conditions?"

**Changed:** "Mode A may have been fixed upstream" is no longer the best read; the rate is just low. EXPERIMENTS.md should describe Mode A as "low but observable" rather than "currently dormant."

### Run 15b — local full sweep, High Performance + Defender off (2026-05-19)

**Question:** Does the tuned-config baseline (High Performance power plan, Defender real-time scanning disabled) change the local flake rate?

**Setup:** Same host (DESKTOP-R0D3F8V), same WSL 2.7.3.0, same Ubuntu distro. Re-ran the LOCAL_PLAN scenarios under the tuned config and compared against the prior Run 14 baseline.

**Results:**

| run    | config                            | iters  | events | wall (min) | avg ms/call |
| ------ | --------------------------------- | ------ | ------ | ---------- | ----------- |
| plain  | Balanced + Defender on            | 30,000 | 0      | 130.9      | 261.2       |
| retry  | Balanced + Defender on            | 30,000 | 0      | 131.8      | 262.8       |
| plain  | High Performance + Defender off   | 30,000 | 0      | 79.9       | 159.3       |
| retry  | High Performance + Defender off   | 30,000 | 0      | 79.9       | 159.3       |

**Learned:**

1. **Local rate stayed at zero across all four configurations.** 120k iters, zero events.
2. **Latency dropped 39% in wall clock under the tuned config** (130.9 min → 79.9 min). Power plan accounted for ~33 pp; Defender ~6 pp.
3. **Per-call timing collapsed to a tight band.** Typical range went from 176–300 ms to 134–180 ms. Cold-start outliers (~4 s) persisted in both configs.

**Important caveat on what local zeros tell us:**

The local machine has *never* shown Mode A in any run, under any config. We do not have a high-rate baseline on this hardware. The Run 15b zeros do not prove "power plan and Defender are irrelevant" — they prove "neither config triggers the bug on a machine that does not flake anyway."

Every confirmed Mode A event captured in this investigation has been on GHA windows-latest. The 2023 cmd.exe-from-bash observation was on a different physical machine (no longer available for testing). Jan's current workstation — high core count, abundant memory, low contention — is qualitatively different hardware from GHA's smaller, more constrained VMs. The bug may correlate with constrained cloud-VM conditions (oversubscribed CPU, slower I/O, shared-tenant scheduling) that the workstation does not reproduce regardless of power settings.

**Changed:** Removed the earlier "power plan / Defender ruled out" claim from Current understanding. The honest summary: those factors are untestable until we have a machine that produces baseline events.
