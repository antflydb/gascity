// Package fsyncprobe times one fsync on the filesystem that holds a Dolt data
// directory, with a bound that holds for the caller.
//
// A thread stuck in an uninterruptible fsync keeps its process alive and keeps
// every inherited pipe open, so a caller that waited on the measuring process
// (or on a shell pipe it shares) would wait out the stall it is trying to
// detect. Probe therefore measures in a detached worker process, reads the
// result under a deadline, and returns without ever waiting for the worker.
package fsyncprobe

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Probe outcomes. Finished and unfinished are both measurements; error means
// no measurement was possible.
const (
	StatusFinished   = "finished"
	StatusUnfinished = "unfinished"
	StatusError      = "error"
)

// ResultFD is the descriptor the detached worker writes its result to.
const ResultFD = 3

const (
	probePrefix = ".gc-fsync-probe-"
	probeBytes  = 4096
)

// Result is the outcome of one probe.
type Result struct {
	// Status is StatusFinished, StatusUnfinished or StatusError.
	Status string `json:"status"`
	// FsyncMS is the measured time for a finished probe and the bound for an
	// unfinished one.
	FsyncMS int64 `json:"fsync_ms"`
	// Error says why a probe could not measure; set only for StatusError.
	Error string `json:"error,omitempty"`
}

// Measure appends 4 KiB to a fresh dotfile in the root of dir, times syncFn on
// it, and removes the file. The file sits in the data-dir root so it shares a
// filesystem with the databases. A leftover file from a killed worker is
// harmless.
func Measure(dir string, syncFn func(*os.File) error) Result {
	if err := requireDir(dir); err != nil {
		return errorResult(err)
	}
	name := fmt.Sprintf("%s%d-%d", probePrefix, os.Getpid(), time.Now().UnixNano())
	path := dir + string(os.PathSeparator) + name
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errorResult(fmt.Errorf("create probe file: %w", err))
	}
	defer func() { _ = os.Remove(path) }()
	defer func() { _ = f.Close() }()

	start := time.Now()
	if _, err := f.Write(make([]byte, probeBytes)); err != nil {
		return errorResult(fmt.Errorf("write probe file: %w", err))
	}
	if err := syncFn(f); err != nil {
		return errorResult(fmt.Errorf("sync probe file: %w", err))
	}
	return Result{Status: StatusFinished, FsyncMS: time.Since(start).Milliseconds()}
}

// RunWorker is the detached worker's body: it measures a real fsync in dir,
// writes the Result as JSON to out and closes it.
func RunWorker(dir string, out *os.File) error {
	defer out.Close() //nolint:errcheck // the result write below reports failures
	res := Measure(dir, func(f *os.File) error { return f.Sync() })
	if err := json.NewEncoder(out).Encode(res); err != nil {
		return fmt.Errorf("write probe result: %w", err)
	}
	return nil
}

// Probe measures an fsync in dir through a detached re-exec of the running gc
// binary, and returns within timeout whether or not the fsync finishes.
func Probe(dir string, timeout time.Duration) Result {
	exe, err := os.Executable()
	if err != nil {
		return errorResult(fmt.Errorf("resolve gc executable: %w", err))
	}
	return ProbeWithWorker(dir, timeout, func(d string) *exec.Cmd {
		return exec.Command(exe, "dolt-state", "fsync-probe-worker", "--dir", d)
	})
}

// ProbeWithWorker is Probe with the worker command supplied by the caller. The
// command runs in its own session with null stdio and the result pipe as
// ResultFD. A background goroutine reaps it; the caller never waits on it, so
// a worker stuck in the kernel outlives the call and finishes on its own.
func ProbeWithWorker(dir string, timeout time.Duration, worker func(dir string) *exec.Cmd) Result {
	if err := requireDir(dir); err != nil {
		return errorResult(err)
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	rd, wr, err := os.Pipe()
	if err != nil {
		return errorResult(fmt.Errorf("create result pipe: %w", err))
	}
	defer rd.Close() //nolint:errcheck // closing also unblocks the reader goroutine

	cmd := worker(dir)
	if cmd == nil {
		_ = wr.Close()
		return errorResult(errors.New("no worker command"))
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	cmd.ExtraFiles = []*os.File{wr}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = wr.Close()
		return errorResult(fmt.Errorf("start fsync probe worker: %w", err))
	}
	_ = wr.Close()
	go func() { _ = cmd.Wait() }()

	results := make(chan Result, 1)
	go func() { results <- readResult(rd) }()
	select {
	case res := <-results:
		return res
	case <-deadline.C:
		return Result{Status: StatusUnfinished, FsyncMS: timeout.Milliseconds()}
	}
}

func readResult(r io.Reader) Result {
	var res Result
	if err := json.NewDecoder(r).Decode(&res); err != nil {
		return errorResult(fmt.Errorf("read fsync probe result: %w", err))
	}
	switch res.Status {
	case StatusFinished, StatusError:
		return res
	}
	return errorResult(fmt.Errorf("fsync probe worker reported status %q", res.Status))
}

func requireDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("probe directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("probe directory: %s is not a directory", dir)
	}
	return nil
}

func errorResult(err error) Result {
	return Result{Status: StatusError, Error: err.Error()}
}
