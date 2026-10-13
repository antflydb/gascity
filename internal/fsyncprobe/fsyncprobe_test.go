package fsyncprobe

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

var probeFileName = regexp.MustCompile(`^\.gc-fsync-probe-\d+-\d+$`)

func probeFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".gc-fsync-probe-") {
			names = append(names, e.Name())
		}
	}
	return names
}

// TestMeasureFinishedReportsMeasuredTimeAndRemovesFile covers the finished
// path: the sync is timed, the probe file is a 4 KiB regular dotfile in the
// data-dir root while the sync runs, and it is gone afterwards.
func TestMeasureFinishedReportsMeasuredTimeAndRemovesFile(t *testing.T) {
	dir := t.TempDir()
	var seen []string
	var seenSize int64
	var seenRegular bool
	res := Measure(dir, func(f *os.File) error {
		seen = probeFiles(t, dir)
		if info, err := f.Stat(); err == nil {
			seenSize = info.Size()
			seenRegular = info.Mode().IsRegular()
		}
		time.Sleep(40 * time.Millisecond)
		return nil
	})
	if res.Status != StatusFinished {
		t.Fatalf("Status = %q (error %q), want %q", res.Status, res.Error, StatusFinished)
	}
	if res.FsyncMS < 40 {
		t.Fatalf("FsyncMS = %d, want >= 40 (the fake sync slept 40ms)", res.FsyncMS)
	}
	if len(seen) != 1 || !probeFileName.MatchString(seen[0]) {
		t.Fatalf("probe files during sync = %v, want exactly one matching %s", seen, probeFileName)
	}
	if !seenRegular || seenSize != 4096 {
		t.Fatalf("probe file during sync: regular=%v size=%d, want regular 4096 bytes", seenRegular, seenSize)
	}
	if left := probeFiles(t, dir); len(left) != 0 {
		t.Fatalf("probe files left after a finished sync: %v", left)
	}
}

// TestMeasureErrorCases covers the status=error inputs: a missing path, a
// regular file instead of a directory, and a sync that fails.
func TestMeasureErrorCases(t *testing.T) {
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := map[string]struct {
		dir  string
		sync func(*os.File) error
	}{
		"missing path": {dir: filepath.Join(t.TempDir(), "absent"), sync: func(f *os.File) error { return f.Sync() }},
		"not a dir":    {dir: notDir, sync: func(f *os.File) error { return f.Sync() }},
		"sync fails":   {dir: t.TempDir(), sync: func(*os.File) error { return errors.New("injected sync failure") }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			res := Measure(tc.dir, tc.sync)
			if res.Status != StatusError {
				t.Fatalf("Status = %q, want %q", res.Status, StatusError)
			}
			if res.Error == "" {
				t.Fatal("Error is empty; status=error must say why")
			}
		})
	}
}

// TestProbeWithWorkerFinished runs a real detached worker (the test binary
// re-executed in worker mode) and expects the measurement back.
func TestProbeWithWorkerFinished(t *testing.T) {
	dir := t.TempDir()
	res := ProbeWithWorker(dir, 10*time.Second, helperWorker(t, "measure", dir, ""))
	if res.Status != StatusFinished {
		t.Fatalf("Status = %q (error %q), want %q", res.Status, res.Error, StatusFinished)
	}
	if res.FsyncMS < 0 {
		t.Fatalf("FsyncMS = %d, want >= 0", res.FsyncMS)
	}
}

// TestProbeWithWorkerBoundHoldsForCaller is the proof of the binding trap: a
// worker that never answers (a D-state fsync) must not hold the caller past
// the bound, and the worker must still be alive when the caller returns.
func TestProbeWithWorkerBoundHoldsForCaller(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "worker.pid")
	bound := 300 * time.Millisecond

	start := time.Now()
	res := ProbeWithWorker(dir, bound, helperWorker(t, "hang", dir, pidFile))
	elapsed := time.Since(start)

	if res.Status != StatusUnfinished {
		t.Fatalf("Status = %q (error %q), want %q", res.Status, res.Error, StatusUnfinished)
	}
	if res.FsyncMS != bound.Milliseconds() {
		t.Fatalf("FsyncMS = %d, want the bound %d", res.FsyncMS, bound.Milliseconds())
	}
	if elapsed < bound || elapsed > bound+2*time.Second {
		t.Fatalf("returned after %v, want within [%v, %v]", elapsed, bound, bound+2*time.Second)
	}

	pid := waitForPID(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("worker %d is gone after the caller returned: %v (it must be left running, detached)", pid, err)
	}
	ownSID, _ := unix.Getsid(0)
	if sid, err := unix.Getsid(pid); err != nil || sid == ownSID {
		t.Fatalf("worker %d session = %d (err %v), want a session other than the caller's %d (Setsid)", pid, sid, err, ownSID)
	}
}

// TestProbeWithWorkerSpawnFailureIsError reports a worker that cannot start
// as status=error rather than a silent success.
func TestProbeWithWorkerSpawnFailureIsError(t *testing.T) {
	res := ProbeWithWorker(t.TempDir(), time.Second, func(string) *exec.Cmd {
		return exec.Command(filepath.Join(t.TempDir(), "no-such-binary"))
	})
	if res.Status != StatusError || res.Error == "" {
		t.Fatalf("got %+v, want status=error with a message", res)
	}
}

// TestProbeWithWorkerMissingDirIsErrorWithoutSpawning checks the directory
// precondition in the caller, so a bad path never costs a worker.
func TestProbeWithWorkerMissingDirIsErrorWithoutSpawning(t *testing.T) {
	spawned := false
	res := ProbeWithWorker(filepath.Join(t.TempDir(), "absent"), time.Second, func(string) *exec.Cmd {
		spawned = true
		return exec.Command("true")
	})
	if res.Status != StatusError {
		t.Fatalf("Status = %q, want %q", res.Status, StatusError)
	}
	if spawned {
		t.Fatal("a worker was spawned for a missing directory")
	}
}

// helperWorker returns a worker factory that re-executes the test binary in
// the named helper mode (the helper-process pattern).
func helperWorker(t *testing.T, mode, dir, pidFile string) func(string) *exec.Cmd {
	t.Helper()
	return func(string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		cmd.Env = append(os.Environ(),
			"FSYNCPROBE_HELPER_MODE="+mode,
			"FSYNCPROBE_HELPER_DIR="+dir,
			"FSYNCPROBE_HELPER_PIDFILE="+pidFile,
		)
		return cmd
	}
}

// TestHelperProcess is the re-exec target, not a test: it only acts when the
// helper env is set.
func TestHelperProcess(_ *testing.T) {
	mode := os.Getenv("FSYNCPROBE_HELPER_MODE")
	if mode == "" {
		return
	}
	switch mode {
	case "measure":
		if err := RunWorker(os.Getenv("FSYNCPROBE_HELPER_DIR"), os.NewFile(ResultFD, "result")); err != nil {
			os.Exit(2)
		}
	case "hang":
		_ = os.WriteFile(os.Getenv("FSYNCPROBE_HELPER_PIDFILE"), []byte(strconv.Itoa(os.Getpid())), 0o644)
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func waitForPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("worker never wrote its pid to %s", pidFile)
	return 0
}
