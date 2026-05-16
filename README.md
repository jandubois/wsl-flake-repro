# wsl-flake-repro

Hammers `wsl.exe -d <distro> bash -c "<cmd>"` in a loop to measure how often it returns exit 0 with empty stdout — the silent failure described in [microsoft/WSL#4082](https://github.com/microsoft/WSL/issues/4082).

Lima's WSL2 driver hits this race in `provisionVM` when it invokes `wsl.exe -d <distro> bash -c "wslpath -u <path>"`. When stdout comes back empty, Lima then runs `bash -c ""` and the VM never boots. Every `wsl.exe` call that captures a short-lived subprocess's stdout plausibly shares the exposure. This harness measures the rate so we can compare mitigations against numbers, not guesses.

## Build

```sh
GOOS=windows GOARCH=amd64 go build -o wsl-flake-repro.exe .
```

## Run

```sh
./wsl-flake-repro.exe --distro Ubuntu --mode plain --iters 1000 --out plain.tsv
```

`--distro <name>` is required and must match an installed WSL distro.

## Modes

| mode              | what it tests                                                                                |
| ----------------- | -------------------------------------------------------------------------------------------- |
| `plain`           | baseline — same call shape as Lima's `provisionVM`                                           |
| `wsl-utf8`        | sets `WSL_UTF8=1` in the env                                                                 |
| `wait-delay`      | sets `cmd.WaitDelay = 5s` so the relay has time to drain after the subprocess exits          |
| `stdin-null`      | redirects stdin from `/dev/null` instead of leaving it unset                                 |
| `cmd-shim`        | wraps the call through `cmd.exe /c`                                                          |
| `powershell-shim` | wraps the call through `powershell.exe -Command`                                             |
| `retry`           | re-runs up to 3 times on empty stdout; reports how many tries were needed                    |

Additional flags:

- `--parallel N` — N concurrent workers (test whether contention raises the rate)
- `--sleep DUR` — sleep between iterations per worker
- `--test-path PATH` — Windows path passed to `wslpath -u` (default `C:\Windows\System32`)
- `--shell CMD` — override the inner bash command entirely
- `--timeout DUR` — per-invocation timeout (default 30s)
- `--quiet` — suppress per-flake and progress lines on stderr

## Output

One TSV row per invocation on stdout (or `--out`):

```
timestamp  iter  worker  duration_ms  exit_code  stdout_len  stderr_len  retries  class  stdout_preview  stderr_preview  err
```

`class` values:

- `OK` — non-empty stdout, no error
- `EMPTY_STDOUT_NIL_ERR` — the bug
- `ERR` — exec returned an error
- `TIMEOUT`, `CANCELED` — context fired

Summary stats print to stderr at the end. The harness exits 3 if it reproduced the bug.

## CI

`.github/workflows/repro.yaml` runs each mode on `windows-latest` and uploads the TSVs as artifacts. Trigger it from the Actions tab when comparing mitigations.
