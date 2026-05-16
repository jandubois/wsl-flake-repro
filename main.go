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
	modePlain          mode = "plain"
	modeWSLUTF8        mode = "wsl-utf8"
	modeWaitDelay      mode = "wait-delay"       // WaitDelay = 5s
	modeWaitDelay30s   mode = "wait-delay-30s"   // does the WaitDelay value matter?
	modeStdinNull      mode = "stdin-null"
	modeCmdShim        mode = "cmd-shim"
	modePowershellShim mode = "powershell-shim"
	modeRetry          mode = "retry"
	modeCombinedOutput mode = "combined-output"  // cmd.CombinedOutput() — one buffer for both streams
	modeFileOut        mode = "file-out"         // cmd.Stdout = os.File — child writes directly to inherited fd, no goroutine copy
	modeBashPipe       mode = "bash-pipe"        // Git Bash wraps wsl.exe in $(...). Tests the 2023 cmd.exe-in-subshell pattern.
)

var allModes = []mode{
	modePlain, modeWSLUTF8, modeWaitDelay, modeWaitDelay30s,
	modeStdinNull, modeCmdShim, modePowershellShim, modeRetry,
	modeCombinedOutput, modeFileOut, modeBashPipe,
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
	// attempts lists the class of every attempt for this iter in retry
	// mode (e.g. ["EMPTY_STDOUT_NIL_ERR", "OK"]). Empty for non-retry
	// modes. A row with both EMPTY and OK in attempts proves retry
	// recovered the flake.
	attempts []string
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
		// WaitDelay 5s. With a non-zero value Go spawns an extra watchCtx
		// goroutine and adds a channel synchronization point in Wait(),
		// even on normal exit. The two flakes in the first run both
		// landed in this mode, so we suspect that extra path.
		cmd.WaitDelay = 5 * time.Second

	case modeWaitDelay30s:
		cmd = exec.CommandContext(ctx, "wsl.exe", "-d", distro, "bash", "-c", shell)
		cmd.WaitDelay = 30 * time.Second

	case modeCombinedOutput, modeFileOut:
		// Caller (runOnce) handles I/O for these modes.
		cmd = exec.CommandContext(ctx, "wsl.exe", "-d", distro, "bash", "-c", shell)

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
		// Bind the shell command to a PS variable, then pass that variable
		// to wsl.exe. PowerShell keeps backslashes literal inside single
		// quotes and passes $s as a single argument, so paths like
		// C:\Windows\System32 reach bash intact.
		ps := fmt.Sprintf(`$s = %s; & wsl.exe -d %s bash -c $s; exit $LASTEXITCODE`,
			powershellQuote(shell), distro)
		cmd = exec.CommandContext(ctx, "powershell.exe",
			"-NoProfile", "-NonInteractive", "-Command", ps)

	case modeBashPipe:
		// Git Bash captures wsl.exe via $(...) and re-emits the result.
		// The outer Go capture uses file-out (no goroutine), so any
		// empty value here was lost by bash's pipe capture, not ours.
		// Uses a fixed inner command to sidestep MSYS path-translation
		// surprises around backslashes.
		const bashPath = `C:\Program Files\Git\bin\bash.exe`
		script := fmt.Sprintf(`r=$(wsl.exe -d %s -- echo wsl-flake-marker); printf '%%s' "$r"`, distro)
		cmd = exec.CommandContext(ctx, bashPath, "-c", script)
	}

	return cmd, cleanup
}

func powershellQuote(s string) string {
	// PowerShell single-quote escapes by doubling internal single quotes.
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func runOnce(ctx context.Context, m mode, distro, shell string, terminateEach bool, iter, worker int) result {
	start := time.Now()

	if terminateEach {
		// wsl --terminate forces a cold restart of the distro on the next
		// call. The original Lima failure happened on a freshly restarted
		// distro, which a steady-state loop cannot reproduce.
		tctx, tcancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = exec.CommandContext(tctx, "wsl.exe", "--terminate", distro).Run()
		tcancel()
	}

	cmd, cleanup := buildCmd(ctx, m, distro, shell)
	defer cleanup()

	stdout, stderr, err := collectOutput(m, cmd)

	r := result{
		iter:      iter,
		worker:    worker,
		start:     start,
		duration:  time.Since(start),
		stdoutLen: len(stdout),
		stderrLen: len(stderr),
		stdout:    string(stdout),
		stderr:    string(stderr),
	}
	if cmd.ProcessState != nil {
		r.exitCode = cmd.ProcessState.ExitCode()
	} else {
		r.exitCode = -1
	}
	if err != nil {
		r.errMsg = err.Error()
	}
	r.class = classify(stdout, stderr, err)
	return r
}

// collectOutput runs cmd and returns its stdout, stderr, and run error.
// The collection mechanism varies by mode so we can isolate where bytes
// might get lost: bytes.Buffer (goroutine pipe-copy), CombinedOutput
// (single buffer), or an os.File assigned directly to cmd.Stdout
// (no goroutine — child writes straight to the inherited fd).
func collectOutput(m mode, cmd *exec.Cmd) (stdout, stderr []byte, err error) {
	switch m {
	case modeCombinedOutput:
		stdout, err = cmd.CombinedOutput()
		return stdout, nil, err

	case modeFileOut, modeBashPipe:
		// bash-pipe also exits via the file-out path so the outer Go
		// capture is the proven-reliable one; any empty result must
		// have been lost inside bash's $() capture.
		f, ferr := os.CreateTemp("", "wsl-flake-stdout-*")
		if ferr != nil {
			return nil, nil, ferr
		}
		name := f.Name()
		defer os.Remove(name)
		cmd.Stdout = f
		var stderrBuf bytes.Buffer
		cmd.Stderr = &stderrBuf
		err = cmd.Run()
		// Close before reading so any buffered writes in os.File flush
		// and the read sees the complete contents.
		_ = f.Close()
		stdout, _ = os.ReadFile(name)
		return stdout, stderrBuf.Bytes(), err

	default:
		var stdoutBuf, stderrBuf bytes.Buffer
		cmd.Stdout = &stdoutBuf
		cmd.Stderr = &stderrBuf
		err = cmd.Run()
		return stdoutBuf.Bytes(), stderrBuf.Bytes(), err
	}
}

func runWithRetry(ctx context.Context, m mode, distro, shell string, terminateEach bool, maxAttempts, iter, worker int) result {
	var last result
	var classes []string
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		last = runOnce(ctx, m, distro, shell, terminateEach, iter, worker)
		classes = append(classes, last.class)
		last.retries = attempt - 1
		if last.class == "OK" {
			break
		}
		if attempt < maxAttempts {
			time.Sleep(time.Duration(attempt) * 200 * time.Millisecond)
		}
	}
	last.attempts = classes
	return last
}

func tsvHeader(w io.Writer) {
	fmt.Fprintln(w, strings.Join([]string{
		"timestamp", "iter", "worker", "duration_ms", "exit_code",
		"stdout_len", "stderr_len", "retries", "class", "attempts",
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
		strings.Join(r.attempts, ","),
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
		outputBytes = flag.Int("bytes", 0,
			"if >0, default shell produces N bytes of stdout (ignored when --shell is set)")
		terminateEach = flag.Bool("terminate-each", false,
			"run 'wsl --terminate <distro>' before every iteration (cold restart)")
		maxRetries = flag.Int("max-retries", 3,
			"attempts per iteration in retry mode")
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
		if *outputBytes > 0 {
			// head -c N /dev/zero | tr '\0' 'x' emits exactly N 'x' bytes.
			shell = fmt.Sprintf(`head -c %d /dev/zero | tr '\0' 'x'`, *outputBytes)
		} else {
			shell = fmt.Sprintf(`wslpath -u %q`, *testPath)
		}
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
					r = runWithRetry(ctx, m, *distro, shell, *terminateEach, *maxRetries, iter, worker)
				} else {
					r = runOnce(ctx, m, *distro, shell, *terminateEach, iter, worker)
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
