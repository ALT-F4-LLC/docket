package exec

import (
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/testsupport"
)

// The tests in this package need real child processes: the properties under
// test — process groups, inherited pipes, environment construction, argv
// delivery — are properties of an actual spawn and cannot be faked.
//
// The witness is a small Go program compiled once per test run into a temp
// directory. A COMPILED GO BINARY rather than a script, because a script needs
// an interpreter to run it, and this package's central claim is that no
// interpreter is ever involved. Using one in the tests would make the tests
// prove something weaker than the code promises.

// witnessSource is the helper program. It reports what it received, so a test
// can assert on argv delivery and environment construction from the CHILD's
// point of view — which is the only vantage point that proves anything.
const witnessSource = `package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func main() {
	mode := os.Getenv("WITNESS_MODE")
	switch mode {
	case "env":
		// Dump the environment, one variable per line, for the set-equality
		// assertion.
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
	case "spawn-grandchild":
		// Spawn a long-lived grandchild that OUTLIVES this process, then
		// report its pid and exit. This is the process-escape case: killing
		// only the direct child would leave the grandchild running.
		self, _ := os.Executable()
		c := exec.Command(self)
		c.Env = append(os.Environ(), "WITNESS_MODE=sleep-forever")
		if err := c.Start(); err != nil {
			fmt.Println("SPAWN-FAILED", err)
			os.Exit(1)
		}
		fmt.Println("GRANDCHILD", c.Process.Pid)
		os.Stdout.Sync()
		// Sleep long enough that the timeout fires while this process is
		// still alive and the grandchild is still held.
		time.Sleep(10 * time.Minute)
	case "spawn-grandchild-then-exit":
		// The pipe-hang case: spawn a grandchild that INHERITS THE PIPES and
		// outlives this process, then exit immediately. A runner that waits
		// for EOF on the capture pipe hangs here forever, because the
		// grandchild holds the write end open.
		self, _ := os.Executable()
		c := exec.Command(self)
		c.Env = append(os.Environ(), "WITNESS_MODE=sleep-forever")
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		if err := c.Start(); err != nil {
			os.Exit(1)
		}
		fmt.Println("PARENT-EXITING")
		os.Exit(0)
	case "sleep-forever":
		time.Sleep(10 * time.Minute)
	case "flood":
		// Emit far past the capture cap, forever, to prove the reader stops
		// consuming rather than spinning.
		line := strings.Repeat("x", 1024)
		for i := 0; ; i++ {
			fmt.Println(line)
			if i > 2_000_000 {
				return
			}
		}
	case "interleave":
		// Alternate between the two streams so write order is checkable.
		fmt.Fprintln(os.Stdout, "one")
		os.Stdout.Sync()
		fmt.Fprintln(os.Stderr, "two")
		os.Stderr.Sync()
		fmt.Fprintln(os.Stdout, "three")
		os.Stdout.Sync()
		fmt.Fprintln(os.Stderr, "four")
		os.Stderr.Sync()
	case "exit":
		code, _ := strconv.Atoi(os.Getenv("WITNESS_EXIT"))
		os.Exit(code)
	case "count":
		// Increment a counter file and exit with the code the Nth run wants.
		// This drives the flaky re-run table: the same command produces
		// different exit codes on successive attempts, which is precisely what
		// "flaky" names.
		path := os.Getenv("WITNESS_COUNT_FILE")
		b, _ := os.ReadFile(path)
		n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		n++
		os.WriteFile(path, []byte(strconv.Itoa(n)), 0o600)
		fmt.Println("ATTEMPT", n)
		passOn, _ := strconv.Atoi(os.Getenv("WITNESS_PASS_ON"))
		if passOn > 0 && n >= passOn {
			os.Exit(0)
		}
		os.Exit(1)
	case "sentinel":
		// Write a sentinel file, proving THIS binary ran. The containment
		// table uses it to assert that a refused command did not execute:
		// asserting on a return value alone would pass against an
		// implementation that ran the command and then reported a refusal.
		os.WriteFile(os.Getenv("WITNESS_SENTINEL"), []byte("ran"), 0o600)
		fmt.Println("SENTINEL-WRITTEN")
	default:
		// Report argv verbatim, framed so a test can assert exact element
		// boundaries. The frame is a NUL-delimited block rather than one line
		// per element, because an argument may itself contain a newline — and
		// a line-based frame would make the newline row of the no-interpreter
		// table unassertable, which is the row that matters most.
		fmt.Print("ARGV-BEGIN\x00")
		for _, a := range os.Args[1:] {
			fmt.Printf("%s\x00", a)
		}
		fmt.Print("ARGV-END")
	}
}
`

// buildWitness compiles the witness program and returns its path. The result is
// cached per test run, since compiling once is enough.
var witnessPath string

func witness(t *testing.T) string {
	t.Helper()
	if witnessPath != "" {
		return witnessPath
	}

	dir, err := os.MkdirTemp("", "docket-witness-")
	testsupport.Must(t, err, "creating the witness directory: %v", err)
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(witnessSource), 0o600); err != nil {
		t.Fatalf("writing the witness source: %v", err)
	}
	// A module file, so the build does not inherit this repo's module context.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module witness\n\ngo 1.26.0\n"), 0o600); err != nil {
		t.Fatalf("writing the witness go.mod: %v", err)
	}

	out := filepath.Join(dir, "witness")
	if err := goBuild(dir, out, src); err != nil {
		t.Skipf("cannot build the witness binary (no Go toolchain available?): %v", err)
	}

	witnessPath = out
	return out
}

// goBuild shells out to the Go toolchain. It is the one place these tests
// invoke a compiler, and it happens at TEST SETUP, never through the package
// under test.
func goBuild(dir, out, src string) error {
	gobin := filepath.Join(runtime.GOROOT(), "bin", "go")
	if _, err := os.Stat(gobin); err != nil {
		gobin = "go"
	}
	cmd := osexec.Command(gobin, "build", "-o", out, src)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOFLAGS=", "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, b)
	}
	return nil
}

// witnessSpec builds a Spec that runs the witness in a given mode.
func witnessSpec(t *testing.T, mode string, args ...string) Spec {
	t.Helper()
	env, err := BuildEnv(EnvPolicy{Gate: "checks", Repo: t.TempDir()})
	testsupport.Must(t, err, "BuildEnv: %v", err)
	if mode != "" {
		env = append(env, "WITNESS_MODE="+mode)
	}
	return Spec{
		Argv: append([]string{witness(t)}, args...),
		Dir:  t.TempDir(),
		Env:  env,
	}
}

// parseArgs pulls the NUL-delimited argv block out of witness output.
//
// NUL is the one byte that cannot appear inside an argv element — execve
// terminates each element with it — so it is the only safe delimiter here. That
// is the same reasoning that makes the trust store hash a JSON encoding rather
// than a joined string: no OTHER delimiter is safe, because an element can
// contain it.
func parseArgs(output string) []string {
	_, rest, ok := strings.Cut(output, "ARGV-BEGIN\x00")
	if !ok {
		return nil
	}
	block, _, ok := strings.Cut(rest, "ARGV-END")
	if !ok {
		return nil
	}
	block = strings.TrimSuffix(block, "\x00")
	if block == "" {
		return nil
	}
	return strings.Split(block, "\x00")
}
