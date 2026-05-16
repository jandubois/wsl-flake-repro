// wsl-flake-repro hammers `wsl.exe -d <distro> bash -c "<cmd>"` in a loop
// and counts how often it returns exit-0 with empty stdout.
//
// Lima's pkg/driver/wsl2/vm_windows.go hits this race when invoking
// `wsl.exe -d <distro> bash -c "wslpath -u <path>" <path>`: stdout
// occasionally comes back empty with no error, and Lima then silently
// runs `bash -c ""` and hangs forever. See microsoft/WSL#4082 for the
// general class of bug. The harness lets us measure the rate under
// different mitigations.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type mode string

const (
	modePlain         mode = "plain"
	modeWSLUTF8       mode = "wsl-utf8"
	modeWaitDelay     mode = "wait-delay"
	modeStdinNull     mode = "stdin-null"
	modeCmdShim       mode = "cmd-shim"
	modePowershellShim mode = "powershell-shim"
	modeRetry         mode = "retry"
)

var allModes = []mode{
	modePlain, modeWSLUTF8, modeWaitDelay, modeStdinNull,
	modeCmdShim, modePowershellShim, modeRetry,
}

type result struct {
	iter     int
	worker   int
	start    time.Time
	duration time.Duration
	exitCode int
	stdoutLen int
	stderrLen int
	stdout   string
	stderr   string
	errMsg   string
	class    string
	retries  int
}

func classify(stdout, stderr []byte, err error) string {
	switch {
	case err != nil && errors.Is(err, context.Canceled):
		return "CANCELED"
	case err != nil && errors.Is(err, context.DeadlineExceeded):
		return "TIMEOUT"
	case err != nil:
		return "ERR"
	case len(bytes.TrimSpace(stdout)) == 0:
		return "EMPTY_STDOUT_NIL_ERR" // the bug
	default:
		return "OK"
	}
}

// buildCmd constructs the exec.Cmd for the requested mode.
// The "shell" string is what gets passed to bash -c inside the distro.
func buildCmd(ctx context.Context, m mode, distro, shell string) (*exec.Cmd, func()) {
	cleanup := func() {}
	var cmd *exec.Cmd

	switch m {
	case modePlain, modeRetry:
		cmd = exec.CommandContext(ctx, "wsl.exe", "-d", distro, "bash", "-c", shell)

	case modeWSLUTF8:
		cmd = exec.CommandContext(ctx, "wsl.exe", "-d", distro, "bash", "-c", shell)
		cmd.Env = append(os.Environ(), "WSL_UTF8=1")

	case modeWaitDelay:
		cmd = exec.CommandContext(ctx, "wsl.exe", "-d", distro, "bash", "-c", shell)
		// Give the relay extra time to drain after the subprocess exits.
		// Requires Go 1.20+; this is the closest thing to a direct fix for
		// the EOF-before-drain race in WSL's relay.cpp.
		cmd.WaitDelay = 5 * time.Second

	case modeStdinNull:
		cmd = exec.CommandContext(ctx, "wsl.exe", "-d", distro, "bash", "-c", shell)
		nul, err := os.Open(os.DevNull)
		if err == nil {
			cmd.Stdin = nul
			cleanup = func() { _ = nul.Close() }
		}

	case modeCmdShim:
		// Wrap through cmd.exe — your 2023 rancher-desktop comment hedged
		// that powershell.exe might be more reliable than cmd.exe; let's
		// actually measure it.
		cmd = exec.CommandContext(ctx, "cmd.exe", "/c",
			"wsl.exe", "-d", distro, "bash", "-c", shell)

	case modePowershellShim:
		ps := fmt.Sprintf(`wsl.exe -d %s bash -c %s`, distro, powershellQuote(shell))
		cmd = exec.CommandContext(ctx, "powershell.exe",
			"-NoProfile", "-NonInteractive", "-Command", ps)
	}

	return cmd, cleanup
}

func powershellQuote(s string) string {
	// PowerShell single-quote escapes by doubling internal single quotes.
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func runOnce(ctx context.Context, m mode, distro, shell string, iter, worker int) result {
	start := time.Now()
	cmd, cleanup := buildCmd(ctx, m, distro, shell)
	defer cleanup()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	r := result{
		iter:      iter,
		worker:    worker,
		start:     start,
		duration:  time.Since(start),
		stdoutLen: stdout.Len(),
		stderrLen: stderr.Len(),
		stdout:    stdout.String(),
		stderr:    stderr.String(),
	}
	if cmd.ProcessState != nil {
		r.exitCode = cmd.ProcessState.ExitCode()
	} else {
		r.exitCode = -1
	}
	if err != nil {
		r.errMsg = err.Error()
	}
	r.class = classify(stdout.Bytes(), stderr.Bytes(), err)
	return r
}

func runWithRetry(ctx context.Context, m mode, distro, shell string, iter, worker int) result {
	const maxAttempts = 3
	var last result
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		last = runOnce(ctx, m, distro, shell, iter, worker)
		last.retries = attempt - 1
		if last.class == "OK" {
			return last
		}
		if attempt < maxAttempts {
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
	}
	return last
}

func tsvHeader(w io.Writer) {
	fmt.Fprintln(w, strings.Join([]string{
		"timestamp", "iter", "worker", "duration_ms", "exit_code",
		"stdout_len", "stderr_len", "retries", "class",
		"stdout_preview", "stderr_preview", "err",
	}, "\t"))
}

func tsvRow(w io.Writer, r result) {
	preview := func(s string) string {
		s = strings.ReplaceAll(s, "\t", `\t`)
		s = strings.ReplaceAll(s, "\n", `\n`)
		s = strings.ReplaceAll(s, "\r", `\r`)
		if len(s) > 200 {
			s = s[:200] + "..."
		}
		return s
	}
	fmt.Fprintln(w, strings.Join([]string{
		r.start.UTC().Format(time.RFC3339Nano),
		fmt.Sprintf("%d", r.iter),
		fmt.Sprintf("%d", r.worker),
		fmt.Sprintf("%d", r.duration.Milliseconds()),
		fmt.Sprintf("%d", r.exitCode),
		fmt.Sprintf("%d", r.stdoutLen),
		fmt.Sprintf("%d", r.stderrLen),
		fmt.Sprintf("%d", r.retries),
		r.class,
		preview(r.stdout),
		preview(r.stderr),
		preview(r.errMsg),
	}, "\t"))
}

func main() {
	var (
		modeFlag = flag.String("mode", string(modePlain),
			"test mode: "+strings.Join(modeNames(), ", "))
		distro   = flag.String("distro", "", "WSL distro name (required)")
		iters    = flag.Int("iters", 1000, "total iterations")
		parallel = flag.Int("parallel", 1, "concurrent workers")
		sleep    = flag.Duration("sleep", 0, "sleep between iterations per worker")
		testPath = flag.String("test-path", `C:\Windows\System32`,
			"path to translate via wslpath")
		shellOverride = flag.String("shell", "",
			"override the inner shell command (default: wslpath -u <test-path>)")
		outPath = flag.String("out", "", "output TSV path (default: stdout)")
		timeout = flag.Duration("timeout", 30*time.Second, "per-invocation timeout")
		quiet   = flag.Bool("quiet", false, "suppress progress log to stderr")
	)
	flag.Parse()

	if *distro == "" {
		fmt.Fprintln(os.Stderr, "fatal: --distro is required")
		os.Exit(2)
	}

	m := mode(*modeFlag)
	if !validMode(m) {
		fmt.Fprintf(os.Stderr, "fatal: unknown --mode %q (valid: %s)\n",
			*modeFlag, strings.Join(modeNames(), ", "))
		os.Exit(2)
	}

	shell := *shellOverride
	if shell == "" {
		shell = fmt.Sprintf(`wslpath -u %q`, *testPath)
	}

	out := os.Stdout
	if *outPath != "" {
		f, err := os.Create(*outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		out = f
	}

	var outMu sync.Mutex
	tsvHeader(out)

	var (
		iterCounter atomic.Int64
		okCount     atomic.Int64
		emptyCount  atomic.Int64
		errCount    atomic.Int64
		otherCount  atomic.Int64
		durations   []int64
		durMu       sync.Mutex
	)

	startWall := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < *parallel; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for {
				iter := int(iterCounter.Add(1))
				if iter > *iters {
					return
				}

				ctx, cancel := context.WithTimeout(context.Background(), *timeout)
				var r result
				if m == modeRetry {
					r = runWithRetry(ctx, m, *distro, shell, iter, worker)
				} else {
					r = runOnce(ctx, m, *distro, shell, iter, worker)
				}
				cancel()

				outMu.Lock()
				tsvRow(out, r)
				outMu.Unlock()

				durMu.Lock()
				durations = append(durations, r.duration.Milliseconds())
				durMu.Unlock()

				switch r.class {
				case "OK":
					okCount.Add(1)
				case "EMPTY_STDOUT_NIL_ERR":
					emptyCount.Add(1)
					if !*quiet {
						fmt.Fprintf(os.Stderr,
							"[FLAKE] iter=%d worker=%d duration=%dms exit=%d stderr_len=%d\n",
							r.iter, r.worker, r.duration.Milliseconds(), r.exitCode, r.stderrLen)
					}
				case "ERR", "TIMEOUT", "CANCELED":
					errCount.Add(1)
				default:
					otherCount.Add(1)
				}

				if !*quiet && iter%100 == 0 {
					fmt.Fprintf(os.Stderr,
						"progress: %d/%d (ok=%d empty=%d err=%d)\n",
						iter, *iters, okCount.Load(), emptyCount.Load(), errCount.Load())
				}

				if *sleep > 0 {
					time.Sleep(*sleep)
				}
			}
		}(w)
	}
	wg.Wait()

	elapsed := time.Since(startWall)

	// Summary to stderr (TSV stays clean for downstream tooling).
	total := okCount.Load() + emptyCount.Load() + errCount.Load() + otherCount.Load()
	flakeRate := 0.0
	if total > 0 {
		flakeRate = float64(emptyCount.Load()) / float64(total) * 100
	}

	durMu.Lock()
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	var p50, p95, p99 int64
	if n := len(durations); n > 0 {
		p50 = durations[n*50/100]
		p95 = durations[n*95/100]
		p99 = durations[n*99/100]
	}
	durMu.Unlock()

	fmt.Fprintf(os.Stderr, `
=== summary ===
mode:        %s
distro:      %s
shell:       %s
parallel:    %d
elapsed:     %s
total:       %d
ok:          %d
empty:       %d  (%.3f%% flake rate)
err:         %d
other:       %d
duration ms: p50=%d p95=%d p99=%d
`,
		m, *distro, shell, *parallel, elapsed,
		total, okCount.Load(), emptyCount.Load(), flakeRate,
		errCount.Load(), otherCount.Load(),
		p50, p95, p99)

	if emptyCount.Load() > 0 {
		os.Exit(3) // distinct exit code so CI can detect a flake reproduction
	}
}

func modeNames() []string {
	out := make([]string, len(allModes))
	for i, m := range allModes {
		out[i] = string(m)
	}
	return out
}

func validMode(m mode) bool {
	for _, x := range allModes {
		if m == x {
			return true
		}
	}
	return false
}
