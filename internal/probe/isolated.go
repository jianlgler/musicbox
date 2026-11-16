package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ChildArg is the first argument that makes a binary run as a probe child instead of its normal
// job. The worker re-runs its own executable with it: one binary, always the same version.
const ChildArg = "__probe-child"

const (
	DefaultTimeout = 30 * time.Second
	maxStdout      = 16 << 20 // a 10 MiB cover is ~13.4 MiB in base64, plus the tags
	maxStderr      = 4 << 10
)

// ErrTimeout means the parser ran past its deadline and was killed. The same bytes would hang
// the same way again, so the caller should treat it as permanent.
var ErrTimeout = errors.New("parser timed out and was killed")

// ErrCrashed means the child exited abnormally (panic, signal, out of memory)
var ErrCrashed = errors.New("parser crashed")

// Isolated runs Read in a child process, so a parse that hangs can be killed and one that
// crashes takes only the child down.
type Isolated struct {
	Executable string        // the binary to run with ChildArg (the worker itself)
	Timeout    time.Duration // per file
	TempDir    string        // TMPDIR for the child: TagLib keeps its compiled-WASM cache there
}

// Probe parses path in a child process. path must be alone in its directory (see Read).
func (p Isolated) Probe(ctx context.Context, path string) (Result, error) {
	timeout := p.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, p.Executable, ChildArg, path) // killed (SIGKILL) when ctx ends
	cmd.WaitDelay = time.Second                                   // don't wait forever for pipes after the kill
	// An empty environment: the worker's holds DATABASE_URL and the S3 keys, and the child
	// handles hostile input. It gets only where to cache the compiled WASM, and a soft memory cap.
	cmd.Env = []string{"TMPDIR=" + p.TempDir, "GOMEMLIMIT=256MiB", "GOMAXPROCS=1"}
	cmd.Dir = "/"
	cmd.Stdin = nil
	// If the worker dies, the kernel kills the child too: no orphaned parsers left running
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}

	var stdout, stderr limitedBuffer
	stdout.max, stderr.max = maxStdout, maxStderr
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return Result{}, ErrTimeout
	}
	if err != nil {
		return Result{}, fmt.Errorf("%w: %v: %s", ErrCrashed, err, lastLine(stderr.String()))
	}
	// Checked before parsing: cut-off JSON wouldn't parse anyway, but this says why
	if stdout.overflow {
		return Result{}, fmt.Errorf("%w: output larger than %d bytes", ErrCrashed, maxStdout)
	}

	var out childOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return Result{}, fmt.Errorf("%w: unreadable output: %v", ErrCrashed, err)
	}
	if out.Unreadable {
		return Result{}, ErrUnreadable
	}
	if out.Error != "" {
		return Result{}, errors.New(out.Error)
	}
	// Re-check what came back: never trust the child more than the file it parsed
	if out.Result.Cover != nil {
		out.Result.Cover, out.Result.CoverType, out.Result.CoverRejected = Cover(out.Result.Cover)
	}
	return out.Result, nil
}

type childOutput struct {
	Result     Result `json:"result"`
	Unreadable bool   `json:"unreadable,omitempty"`
	Error      string `json:"error,omitempty"`
}

// ChildMain is the child's whole program: parse one file, print JSON, exit. Exit status 0 means
// the JSON is valid, even when the file wasn't; anything else is a crash.
func ChildMain(args []string) int {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: "+ChildArg+" <file>")
		return 2
	}
	r, err := Read(args[0])
	out := childOutput{Result: r}
	switch {
	case errors.Is(err, ErrUnreadable):
		out = childOutput{Unreadable: true}
	case err != nil:
		out = childOutput{Error: err.Error()}
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// limitedBuffer keeps at most max bytes and remembers whether more arrived, so a child flooding
// its output can't use up the worker's memory
//
// The buffer is a named field, not embedded: embedding would promote bytes.Buffer's ReadFrom,
// and io.Copy (which os/exec uses to drain the pipe) calls ReadFrom when it exists, skipping
// Write and its limit entirely. TestIsolatedOutputFlood caught exactly that.
type limitedBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.max - b.buf.Len(); n > room {
		b.overflow = true
		p = p[:max(room, 0)]
	}
	b.buf.Write(p)
	// Report the full length as written: a short count without an error breaks io.Writer's
	// contract, the copy from the pipe stops, and the child would block on a full pipe
	return n, nil
}

func (b *limitedBuffer) Bytes() []byte  { return b.buf.Bytes() }
func (b *limitedBuffer) String() string { return b.buf.String() }

// lastLine returns the end of a panic message: the useful part, without the whole stack trace
func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := range lines {
		if strings.HasPrefix(lines[i], "panic:") || strings.HasPrefix(lines[i], "fatal error:") {
			return lines[i]
		}
	}
	return lines[len(lines)-1]
}

var _ io.Writer = (*limitedBuffer)(nil)
