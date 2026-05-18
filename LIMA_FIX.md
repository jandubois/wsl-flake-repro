# Lima fix: eliminate the wslpath round-trip in provisionVM

**Background:** [EXPERIMENTS.md](./EXPERIMENTS.md). The short version: `wsl.exe` silently returns exit-0 with empty stdout at a rate that ranges from ~1/4,000 to undetectable (variable across runners and days). Every code path that captures `wsl.exe`'s stdout is exposed. Lima's `provisionVM` captures stdout from a wslpath call, then dispatches the boot script using the empty result, which hangs the boot.

This document targets a separate refactor session. Code coordinates use a SHA, not line numbers, because the surrounding file is actively being restructured.

## Reference

- **Lima repo:** `github.com/lima-vm/lima`
- **File:** `pkg/driver/wsl2/vm_windows.go`
- **Last-modified SHA for this file:** `89cb69d2f22724eb38e3a78e8b24139b44aa9207` ("wsl2: stop spinning in for { <-ctx.Done() } after instance stop", 2026-04-27). Use this commit to compare if the function has moved.
- **Function:** `provisionVM(ctx context.Context, instanceDir, instanceName, distroName string, errCh chan<- error) error`

## What the buggy logic does today

`provisionVM` builds a bash script from the embedded `lima-init.TEMPLATE`, then needs to execute it inside the distro. It does this in three steps:

1. **Writes the script to a Windows tempfile** (`os.CreateTemp("", "lima-wsl2-boot-*.sh")`), keeping the Windows path.
2. **Asks `wsl.exe` to translate the Windows path to a Linux path** by running `wsl.exe -d <distro> bash -c "wslpath -u <winpath>" <winpath>` and capturing stdout via `.Output()`. The captured string is trimmed and used as the Linux-side script path.
3. **Runs the boot script** by spawning `wsl.exe -d <distro> bash -c <linuxpath>` in a goroutine and capturing the combined output.

Current code (verbatim from the reference SHA — find the equivalent in HEAD if it has moved):

```go
limaBootFile, err := os.CreateTemp("", "lima-wsl2-boot-*.sh")
// ... writes limaBootB, closes file, captures Name() ...
bootFileWSLPath := strconv.Quote(limaBootFileWinPath)
limaBootFilePathOnLinuxB, err := exec.CommandContext(
    ctx,
    "wsl.exe",
    "-d",
    distroName,
    "bash",
    "-c",
    fmt.Sprintf("wslpath -u %s", bootFileWSLPath),
    bootFileWSLPath,
).Output()
if err != nil {
    os.RemoveAll(limaBootFileWinPath)
    return fmt.Errorf("failed to run wslpath command: %w", err)
}
limaBootFileLinuxPath := strings.TrimSpace(string(limaBootFilePathOnLinuxB))
go func() {
    cmd := exec.CommandContext(
        ctx,
        "wsl.exe",
        "-d",
        distroName,
        "bash",
        "-c",
        limaBootFileLinuxPath,
    )
    out, err := cmd.CombinedOutput()
    os.RemoveAll(limaBootFileWinPath)
    // ...
}()
```

## How the bug manifests

Step 2's `.Output()` call occasionally returns `nil` error with empty stdout. `TrimSpace` then produces an empty string. Step 3 spawns `wsl.exe -d <distro> bash -c ""`, which exits 0 without running anything. The distro never boots; the calling test eventually hits its timeout. We've observed this in CI as `bats/tests/33-lima-controllers/limavm-running.bats` test 78 hanging for 5 minutes.

The bug lives in `wsl.exe`'s relay between the in-distro process and the parent's handle. It's not Go-specific (bash `$(...)` shows the same), not a goroutine race (raw file inheritance shows it too), and not specific to `wslpath`. Every callsite that captures `wsl.exe` stdout is exposed. Lima's case is severe because the empty result is silently used as a command, escalating "lost output" to "no boot."

## The fix

Replace the tempfile + wslpath + second-spawn chain with a single `wsl.exe` invocation that reads the script from stdin:

```go
cmd := exec.CommandContext(ctx, "wsl.exe", "-d", distroName, "bash")
cmd.Stdin = bytes.NewReader(limaBootB)
go func() {
    out, err := cmd.CombinedOutput()
    if err != nil {
        errCh <- fmt.Errorf(
            "error running wslCommand that executes boot.sh (%v): %w, "+
                "check /var/log/lima-init.log for more details (out=%q)",
            cmd.Args, err, string(out))
    }
    // ... existing post-exec logic for ctx.Done / stopVM ...
}()
```

Bash with no script-file argument reads from stdin. The tempfile, the `wslpath` invocation, the path-translation logic, and the `os.RemoveAll` cleanup all disappear. The total surface area of `wsl.exe` calls in `provisionVM` drops from two to one. The remaining call still captures stdout via `CombinedOutput()` and is still exposed to the same relay bug — but on its own it only causes a one-off "boot.sh output went missing in the log," not "VM never boots." The retry workaround for capture flakes (see EXPERIMENTS.md) is orthogonal and can stack on top of this if Lima ever wants it.

### Imports

`bytes.NewReader` requires importing the `bytes` package if it isn't already.

### Inner wslpath stays

`lima-init.TEMPLATE` (embedded as `limaBoot`) calls `/usr/bin/wslpath '{{.CIDataPath}}'` inside the distro. That wslpath runs *in the distro*, not via `wsl.exe` from Go, so it's not subject to the bug. Leave it.

## Alternatives considered, ruled out

- **Retry on empty stdout.** Works, but treats the symptom. The whole tempfile + wslpath dance is unnecessary; removing it removes the failure mode for this callsite entirely. Retry remains a fine general-purpose mitigation for other callsites that genuinely need to capture stdout.
- **Write the tempfile inside the distro via `\\\\wsl$\\<distro>\\...`** so no translation is needed. Works, but introduces a UNC-path dependency for a use case that doesn't need persistent files at all. Stdin is simpler.
- **Compute the Linux path from the Windows path arithmetically** (`C:\...` → `/mnt/c/...`). The mapping depends on `[automount] root` in `/etc/wsl.conf` (Lima controls this, so the default holds today), but the math is fragile and silently breaks if anyone changes the config. Stdin avoids the dependency.

## Verification

After the change, the test that produced the original failure should no longer have a path-translation step that can flake:

- `rancher-sandbox/rancher-desktop-daemon` → `bats/tests/33-lima-controllers/limavm-running.bats`, test 78 (`start VM for crash recovery test`). This test stops and restarts a Lima VM; the failure mode was the restart hanging on an empty wslpath result. Run BATS Windows CI a few times to confirm.

Also worth confirming locally: a normal `lima start` from Windows still works (`lima-init.log` inside the distro should show the same boot.sh trace as before).

## Out of scope for this fix

- Other `wsl.exe` callsites in Lima that capture stdout (e.g., `getWslStatus`). They're individually less catastrophic (they read structured output that's either present or missing), but they're still exposed to the same bug. A defensive `wslExec` helper with retry + empty-stdout detection would cover them; that's a separate change.
- Upstream Microsoft WSL fix. The relay-side bug is in `microsoft/WSL/src/windows/common/relay.cpp` (CATCH_LOG paths at the relay thread boundary are the leading suspect); an upstream report is its own undertaking. The Lima stdin fix doesn't depend on it.
