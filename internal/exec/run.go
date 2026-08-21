package exec

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Run executes one command and reports what it did.
//
// It NEVER invokes a command interpreter. exec.Command is given argv[0] and
// argv[1:] as separate arguments, so `["echo", "a; rm -rf ~"]` runs echo with
// ONE argument whose text contains a semicolon and a tilde. Nothing splits it,
// nothing expands the tilde, and no glob matches — because no interpreter is
// ever handed the string.
//
// The caller is responsible for having matched the argv against a trust entry
// and for having called Resolve. This function does not consult the trust
// store: mixing the authorization decision into the spawn is what creates the
// window between them.
func Run(spec Spec) (Result, error) {
	if len(spec.Argv) == 0 {
		return Result{}, fmt.Errorf("%w: the argv is empty", ErrRefused)
	}
	if err := assertNoDeniedVars(spec.Env); err != nil {
		// The pre-spawn guard fires here too, not only in BuildEnv: a caller
		// that constructed an Env by some other route must not be able to
		// bypass it. Two call sites, one rule.
		return Result{}, err
	}

	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}

	// argv[0] and argv[1:] as SEPARATE ARGUMENTS. This is the whole of the
	// no-interpreter property at the spawn site.
	cmd := exec.Command(spec.Argv[0], spec.Argv[1:]...)
	cmd.Dir = spec.Dir
	// Env is the CONSTRUCTED allowlist. Assigning a non-nil slice is what stops
	// os/exec from falling back to the parent's whole environment — an empty
	// but non-nil slice means "no variables", while nil means "inherit
	// everything", and that distinction is the entire control.
	cmd.Env = spec.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}

	// X1: the child LEADS ITS OWN PROCESS GROUP, so the timeout can signal the
	// group rather than one pid.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// C1: both streams write into the same capped writer, which is what
	// interleaves them in write order.
	capture := &captureWriter{}

	// X3: ONE os.Pipe, owned by this function, shared by both streams.
	//
	// cmd.StdoutPipe() is deliberately NOT used. Its documented contract is
	// that cmd.Wait() closes the pipe, which races the copy goroutine — and
	// worse, it would take the close out of this function's hands at exactly
	// the moment X3 needs to force it. Owning the fds means the read loop can
	// be bounded by the deadline: a killed child's SURVIVING GRANDCHILD holds
	// the write end open forever, and a wait-on-EOF there is the classic hang
	// this stage must not ship.
	//
	// A single pipe for both streams is also what makes C1's interleaving
	// exact: the kernel orders the writes, rather than two readers racing to
	// append to a shared buffer.
	pr, pw, err := os.Pipe()
	if err != nil {
		return Result{}, fmt.Errorf("creating the capture pipe: %w", err)
	}
	cmd.Stdout = pw
	cmd.Stderr = pw

	// SplitStdout gives stdout its OWN pipe, owned here for the same reason the
	// shared one is: the read loop must be closable, so a killed child's
	// surviving grandchild cannot hold the drain goroutine open forever.
	//
	// Two owned pipes rather than an io.MultiWriter: assigning a non-*os.File
	// writer makes os/exec create a pipe and a copy goroutine of its own, and
	// cmd.Wait() then waits for that goroutine — which is exactly the
	// unbounded-EOF hang X3 exists to avoid, reintroduced through a different
	// door.
	var (
		outCapture = capture
		opr, opw   *os.File
	)
	if spec.SplitStdout {
		opr, opw, err = os.Pipe()
		if err != nil {
			pr.Close()
			pw.Close()
			return Result{}, fmt.Errorf("creating the stdout pipe: %w", err)
		}
		outCapture = &captureWriter{}
		cmd.Stdout = opw
	}

	// Stdin is DATA, fed and then closed — never a command, and never parsed,
	// split, or expanded on the way. It rides an os.Pipe THIS FUNCTION OWNS for
	// the reason the captures do: handing os/exec a plain io.Reader makes
	// cmd.Wait() wait on a copy goroutine, and a child that never reads its
	// input would then block the wait on a write that will never complete.
	// Owning the pipe puts the write on a goroutine nothing waits for; when the
	// child exits or is killed, the write fails and the goroutine ends.
	var ipr, ipw *os.File
	if len(spec.Stdin) > 0 {
		ipr, ipw, err = os.Pipe()
		if err != nil {
			pr.Close()
			pw.Close()
			if opw != nil {
				opr.Close()
				opw.Close()
			}
			return Result{}, fmt.Errorf("creating the stdin pipe: %w", err)
		}
		cmd.Stdin = ipr
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		if opw != nil {
			opr.Close()
			opw.Close()
		}
		if ipw != nil {
			ipr.Close()
			ipw.Close()
		}
		return Result{}, fmt.Errorf("%w: cannot start %s: %v", ErrRefused, Render(spec.Argv[0]), err)
	}

	// The parent's copy of every write end is closed IMMEDIATELY after the fork,
	// and its copy of the stdin READ end with them. The child holds its own;
	// keeping ours open would mean the read end never sees EOF even after the
	// child exits, which is the same hang X3 is about, self-inflicted.
	pw.Close()
	if opw != nil {
		opw.Close()
	}
	if ipr != nil {
		ipr.Close()
		go func() {
			// Nothing waits for this. A child that reads its input to EOF makes
			// it finish immediately; one that ignores stdin makes the write fail
			// as soon as the child exits or is killed, and the goroutine ends
			// then. Neither case can hold up the wait below.
			ipw.Write(spec.Stdin)
			ipw.Close()
		}()
	}

	// The pgid is the child's pid, because Setpgid made it a group leader. It
	// is captured HERE, before any wait: after the process is reaped its pid is
	// gone, and signalling a reaped pid's group is how an implementation ends
	// up killing an unrelated process that reused the number.
	pgid := cmd.Process.Pid

	// Drain the pipe into the capped writer. The goroutine ends when the read
	// end sees EOF (every writer closed) or when this function closes it.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		io.Copy(capture, pr)
	}()

	// The split stdout gets its own drain, on the same terms.
	outDrained := make(chan struct{})
	if opr == nil {
		close(outDrained)
	} else {
		go func() {
			defer close(outDrained)
			io.Copy(outCapture, opr)
		}()
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	var (
		runErr   error
		timedOut bool
		reason   string
	)

	select {
	case runErr = <-waited:
		// The child exited on its own. Give the drain goroutine a bounded
		// moment to finish copying what is already in the pipe — the SAME
		// grandchild-holds-the-write-end hazard applies here, so even the
		// happy path's wait is bounded and never unbounded on EOF. A check
		// that exits while leaving a daemonized child behind is ordinary, not
		// exotic, and it must not wedge the engine.
		select {
		case <-drained:
		case <-time.After(killGrace):
			pr.Close()
		}
		select {
		case <-outDrained:
		case <-time.After(killGrace):
			if opr != nil {
				opr.Close()
			}
		}

	case <-time.After(timeout):
		timedOut = true
		// X2: signal THE GROUP, not the pid. The negative pgid is what reaches
		// a build's compilers and a test runner's servers. SIGTERM first, so a
		// well-behaved process can flush and exit cleanly.
		_ = syscall.Kill(-pgid, syscall.SIGTERM)

		select {
		case runErr = <-waited:
			// Exited on the TERM.
		case <-time.After(killGrace):
			// X2: then SIGKILL the group. Nothing survives this that the
			// kernel can reach.
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			select {
			case runErr = <-waited:
			case <-time.After(killGrace):
				// The direct child is unreapable (a kernel-level stuck state).
				// Returning with what was captured beats blocking the engine
				// forever on a process that will never be waited.
			}
		}

		// X3: the read end is CLOSED after the kill, and the runner does NOT
		// block waiting for grandchildren that inherited the write end. The
		// close is what unblocks io.Copy; without it a surviving grandchild
		// holding the write end open would keep the drain goroutine — and
		// therefore this function — alive indefinitely.
		pr.Close()
		if opr != nil {
			opr.Close()
		}

		// X4: the reason names the timeout AND the limit, so an operator can
		// tell "this check is slow" from "this check hangs".
		reason = fmt.Sprintf("the command exceeded its %s timeout and its process group was killed", timeout)
	}

	elapsed := time.Since(start)
	output, truncated := capture.result()

	// The structured stream is capped by the SAME rule the diagnostic one is: a
	// reply something intends to parse is not exempt from §5.5's bound, and a
	// truncated one is reported truncated rather than handed on as though it
	// were whole.
	var stdout string
	if spec.SplitStdout {
		var outTruncated bool
		stdout, outTruncated = outCapture.result()
		truncated = truncated || outTruncated
	}

	return Result{
		Argv:       append([]string(nil), spec.Argv...),
		Exit:       exitCodeOf(runErr),
		DurationMS: elapsed.Milliseconds(),
		Output:     output,
		Stdout:     stdout,
		Truncated:  truncated,
		TimedOut:   timedOut,
		Reason:     reason,
	}, nil
}
