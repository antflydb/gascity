package tmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// The process probes behind IsRuntimeRunning answer yes, no, or unknown. An
// unknown (ps missing, fork refused, a deadline, any exit status other than
// ps's and pgrep's "no match") must never read as "no": a start path that
// recycles a session on "no" would kill a live agent. These tests inject the
// runner, so no ps or pgrep ever runs.

// fakeProc is one row of fakeProcessTable.
type fakeProc struct {
	comm     string
	args     string
	children []string
}

// probeFault replaces a query's answer. block makes the query wait for its
// context; otherwise err, then code, decide the result.
type probeFault struct {
	code  int
	err   error
	block bool
}

// fakeProcessTable is an injected process runner over a fake process table. It
// answers ps -p <pid> -o comm=/args= and pgrep -P <pid> the way the real tools
// do (exit 1 for no match) and records every query.
type fakeProcessTable struct {
	mu    sync.Mutex
	procs map[string]fakeProc
	// faults is keyed "<kind> <pid>", kind one of comm, args, pgrep.
	faults map[string]probeFault
	// anyFault, if set, replaces every query.
	anyFault *probeFault

	queries  []string
	blocked  int
	deadline []time.Time
}

func newFakeProcessTable(procs map[string]fakeProc) *fakeProcessTable {
	return &fakeProcessTable{procs: procs, faults: map[string]probeFault{}}
}

func (f *fakeProcessTable) run(ctx context.Context, name string, args ...string) ([]byte, int, error) {
	kind, pid := "", ""
	switch {
	case name == "ps" && slices.Contains(args, "comm="):
		kind = "comm"
		pid = args[slices.Index(args, "-p")+1]
	case name == "ps" && slices.Contains(args, "args="):
		kind = "args"
		pid = args[slices.Index(args, "-p")+1]
	case name == "pgrep":
		kind = "pgrep"
		pid = args[slices.Index(args, "-P")+1]
	default:
		return nil, 0, fmt.Errorf("fakeProcessTable: unexpected command %s %v", name, args)
	}

	f.mu.Lock()
	f.queries = append(f.queries, kind+" "+pid)
	if dl, ok := ctx.Deadline(); ok {
		f.deadline = append(f.deadline, dl)
	}
	fault, faulted := f.faults[kind+" "+pid]
	if f.anyFault != nil {
		fault, faulted = *f.anyFault, true
	}
	if faulted && fault.block {
		f.blocked++
	}
	f.mu.Unlock()

	if faulted {
		if fault.block {
			<-ctx.Done()
			return nil, 0, ctx.Err()
		}
		return nil, fault.code, fault.err
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	proc, ok := f.procs[pid]
	switch kind {
	case "comm":
		if !ok {
			return nil, 1, nil
		}
		return []byte(proc.comm + "\n"), 0, nil
	case "args":
		if !ok {
			return nil, 1, nil
		}
		return []byte(proc.args + "\n"), 0, nil
	default:
		if !ok || len(proc.children) == 0 {
			return nil, 1, nil
		}
		return []byte(strings.Join(proc.children, "\n") + "\n"), 0, nil
	}
}

func (f *fakeProcessTable) queryCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.queries)
}

// scriptedPanes answers the pane queries behind GetPaneCommand and GetPanePID
// and counts every tmux call.
type scriptedPanes struct {
	mu         sync.Mutex
	calls      [][]string
	command    string
	commandErr error
	pid        string
	pidErr     error
}

func (s *scriptedPanes) execute(args []string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, slices.Clone(args))
	if tmuxSubcommand(args) != "list-panes" {
		return "", nil
	}
	format := args[len(args)-1]
	switch {
	case strings.Contains(format, "#{pane_current_command}"):
		if s.commandErr != nil {
			return "", s.commandErr
		}
		return "agent\t" + s.command, nil
	case strings.Contains(format, "#{pane_pid}"):
		if s.pidErr != nil {
			return "", s.pidErr
		}
		return "agent\t" + s.pid, nil
	}
	return "agent\t", nil
}

func (s *scriptedPanes) executeCtx(_ context.Context, args []string) (string, error) {
	return s.execute(args)
}

func (s *scriptedPanes) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

// probeTmux builds a Tmux whose pane queries and process queries are both
// scripted.
func probeTmux(panes *scriptedPanes, table *fakeProcessTable, bound time.Duration) *Tmux {
	return &Tmux{
		cfg:              Config{SocketName: "x"},
		exec:             panes,
		processProbe:     table.run,
		processWalkBound: bound,
	}
}

// shellPane is a pane whose command is a shell with pid 100.
func shellPane() *scriptedPanes {
	return &scriptedPanes{command: "bash", pid: "100"}
}

func assertUnknown(t *testing.T, got bool, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("probe = (%v, nil), want unknown (an error)", got)
	}
	if got {
		t.Errorf("probe = (true, %v), want false alongside the unknown error", err)
	}
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Errorf("unknown error = %v, want runtime.ErrRuntimeUnavailable", err)
	}
	if errors.Is(err, runtime.ErrSessionExists) {
		t.Errorf("unknown error = %v, must not read as runtime.ErrSessionExists", err)
	}
	if runtime.IsSessionGone(err) {
		t.Errorf("runtime.IsSessionGone(%v) = true; an unknown must not read as gone", err)
	}
}

// ps and pgrep exit 1 when nothing matches: a definitive no.
func TestIsRuntimeRunningWithError_ExitOneIsDefinitiveNo(t *testing.T) {
	// A shell pane whose pgrep finds no children.
	table := newFakeProcessTable(map[string]fakeProc{"100": {comm: "bash", args: "bash"}})
	tm := probeTmux(shellPane(), table, 0)
	got, err := tm.IsRuntimeRunningWithError("s", []string{"claude"})
	if got || err != nil {
		t.Fatalf("IsRuntimeRunningWithError = (%v, %v), want (false, nil)", got, err)
	}

	// An unrecognized pane command whose own ps and pgrep both answer exit 1.
	panes := &scriptedPanes{command: "2.1.30", pid: "100"}
	tm = probeTmux(panes, newFakeProcessTable(nil), 0)
	got, err = tm.IsRuntimeRunningWithError("s", []string{"claude"})
	if got || err != nil {
		t.Fatalf("unrecognized pane: IsRuntimeRunningWithError = (%v, %v), want (false, nil)", got, err)
	}
}

func TestIsRuntimeRunningWithError_AnythingButExitOneOrZeroIsUnknown(t *testing.T) {
	eagain := &os.PathError{Op: "fork/exec", Path: "/usr/bin/pgrep", Err: syscall.EAGAIN}
	cases := []struct {
		name  string
		fault probeFault
		cause error
	}{
		{"exit 2", probeFault{code: 2}, nil},
		{"exit 3", probeFault{code: 3}, nil},
		{"exit 127", probeFault{code: 127}, nil},
		{"executable not found", probeFault{err: exec.ErrNotFound}, exec.ErrNotFound},
		{"fork refused", probeFault{err: eagain}, syscall.EAGAIN},
		{"deadline", probeFault{err: context.DeadlineExceeded}, context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run("pgrep "+tc.name, func(t *testing.T) {
			table := newFakeProcessTable(map[string]fakeProc{"100": {comm: "bash", args: "bash"}})
			table.faults["pgrep 100"] = tc.fault
			tm := probeTmux(shellPane(), table, 0)
			got, err := tm.IsRuntimeRunningWithError("s", []string{"claude"})
			assertUnknown(t, got, err)
			if tc.cause != nil && !errors.Is(err, tc.cause) {
				t.Errorf("unknown error = %v, want the cause %v kept for errors.Is", err, tc.cause)
			}
		})
		for _, kind := range []string{"comm", "args"} {
			t.Run("ps "+kind+" "+tc.name, func(t *testing.T) {
				// An unrecognized pane command is checked with ps first, then
				// its descendants (none: pgrep exit 1).
				table := newFakeProcessTable(map[string]fakeProc{"100": {comm: "2.1.30", args: "2.1.30"}})
				table.faults[kind+" 100"] = tc.fault
				tm := probeTmux(&scriptedPanes{command: "2.1.30", pid: "100"}, table, 0)
				got, err := tm.IsRuntimeRunningWithError("s", []string{"claude"})
				assertUnknown(t, got, err)
				if tc.cause != nil && !errors.Is(err, tc.cause) {
					t.Errorf("unknown error = %v, want the cause %v kept for errors.Is", err, tc.cause)
				}
			})
		}
	}
}

// A match anywhere is yes, even when another query in the walk was unknown.
func TestIsRuntimeRunningWithError_MatchBeatsUnknown(t *testing.T) {
	table := newFakeProcessTable(map[string]fakeProc{
		"100": {comm: "bash", args: "bash", children: []string{"101", "102"}},
		"101": {comm: "sleep", args: "sleep 1"},
		"102": {comm: "claude", args: "claude"},
	})
	table.faults["comm 101"] = probeFault{code: 2} // visited first, unknown
	tm := probeTmux(shellPane(), table, 0)
	got, err := tm.IsRuntimeRunningWithError("s", []string{"claude"})
	if !got || err != nil {
		t.Fatalf("IsRuntimeRunningWithError = (%v, %v), want (true, nil)", got, err)
	}
}

// An unknown below a sibling that matches deeper still ends in yes.
func TestIsRuntimeRunningWithError_DeepMatchBeatsUnknownSibling(t *testing.T) {
	table := newFakeProcessTable(map[string]fakeProc{
		"100": {comm: "bash", args: "bash", children: []string{"101", "102"}},
		"101": {comm: "sleep", args: "sleep 1"},
		"102": {comm: "sh", args: "sh", children: []string{"103"}},
		"103": {comm: "claude", args: "claude"},
	})
	table.faults["pgrep 101"] = probeFault{code: 2}
	tm := probeTmux(shellPane(), table, 0)
	got, err := tm.IsRuntimeRunningWithError("s", []string{"claude"})
	if !got || err != nil {
		t.Fatalf("IsRuntimeRunningWithError = (%v, %v), want (true, nil)", got, err)
	}
}

// With no match anywhere, one unknown makes the whole walk unknown.
func TestIsRuntimeRunningWithError_NoMatchWithUnknownIsUnknown(t *testing.T) {
	table := newFakeProcessTable(map[string]fakeProc{
		"100": {comm: "bash", args: "bash", children: []string{"101", "102"}},
		"101": {comm: "sleep", args: "sleep 1"},
		"102": {comm: "vim", args: "vim"},
	})
	table.faults["pgrep 101"] = probeFault{code: 2}
	tm := probeTmux(shellPane(), table, 0)
	got, err := tm.IsRuntimeRunningWithError("s", []string{"claude"})
	assertUnknown(t, got, err)
}

// The depth cap ends a walk that otherwise answered everything: a definitive
// no, not an unknown.
func TestHasDescendantWithNames_DepthCapIsDefinitiveNo(t *testing.T) {
	procs := map[string]fakeProc{}
	const chain = maxProcessDescendantDepth + 5
	for i := 1; i <= chain; i++ {
		p := fakeProc{comm: "sleep", args: "sleep 1"}
		if i < chain {
			p.children = []string{strconv.Itoa(i + 1)}
		}
		procs[strconv.Itoa(i)] = p
	}
	table := newFakeProcessTable(procs)
	tm := &Tmux{processProbe: table.run}
	got, err := tm.hasDescendantWithNames(context.Background(), "1", []string{"claude"}, 0)
	if got || err != nil {
		t.Fatalf("hasDescendantWithNames = (%v, %v), want (false, nil) at the depth cap", got, err)
	}

	before := table.queryCount()
	got, err = tm.hasDescendantWithNames(context.Background(), "1", []string{"claude"}, maxProcessDescendantDepth+1)
	if got || err != nil {
		t.Fatalf("past the cap: hasDescendantWithNames = (%v, %v), want (false, nil)", got, err)
	}
	if after := table.queryCount(); after != before {
		t.Errorf("past the cap ran %d more queries, want none", after-before)
	}
}

func TestHasDescendantWithNames_EmptyNamesRunNothing(t *testing.T) {
	table := newFakeProcessTable(nil)
	tm := &Tmux{processProbe: table.run}
	for _, names := range [][]string{nil, {}} {
		got, err := tm.hasDescendantWithNames(context.Background(), "1", names, 0)
		if got || err != nil {
			t.Fatalf("hasDescendantWithNames(%v) = (%v, %v), want (false, nil)", names, got, err)
		}
	}
	if n := table.queryCount(); n != 0 {
		t.Errorf("ran %d queries for empty names, want none", n)
	}
}

// Every blocked query waits on the walk's one deadline, and the queries after
// it are skipped: a hung ps costs one bound, not one bound per process.
func TestIsRuntimeRunningWithError_OneDeadlinePerWalk(t *testing.T) {
	const bound = 100 * time.Millisecond
	table := newFakeProcessTable(map[string]fakeProc{
		"100": {comm: "bash", args: "bash", children: []string{"101", "102", "103"}},
		"101": {comm: "sleep", args: "sleep", children: []string{"104"}},
		"102": {comm: "sleep", args: "sleep"},
		"103": {comm: "sleep", args: "sleep"},
		"104": {comm: "sleep", args: "sleep"},
	})
	for _, pid := range []string{"101", "102", "103", "104"} {
		table.faults["comm "+pid] = probeFault{block: true}
	}
	tm := probeTmux(shellPane(), table, bound)

	start := time.Now()
	got, err := tm.IsRuntimeRunningWithError("s", []string{"claude"})
	elapsed := time.Since(start)

	assertUnknown(t, got, err)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("unknown error = %v, want the deadline kept as its cause", err)
	}
	if table.blocked != 1 {
		t.Errorf("%d queries blocked on the deadline, want exactly 1 (the rest skipped); queries = %v", table.blocked, table.queries)
	}
	if elapsed > 5*bound {
		t.Errorf("walk took %v, want about one bound (%v)", elapsed, bound)
	}
}

// Every query carries the walk's deadline, and the default bound is 10s.
func TestIsRuntimeRunningWithError_QueriesCarryTheWalkDeadline(t *testing.T) {
	table := newFakeProcessTable(map[string]fakeProc{
		"100": {comm: "bash", args: "bash", children: []string{"101"}},
		"101": {comm: "sleep", args: "sleep"},
	})
	tm := probeTmux(shellPane(), table, 0)
	if _, err := tm.IsRuntimeRunningWithError("s", []string{"claude"}); err != nil {
		t.Fatalf("IsRuntimeRunningWithError = %v", err)
	}
	if len(table.deadline) == 0 {
		t.Fatal("no query carried a deadline")
	}
	for _, dl := range table.deadline {
		if dl != table.deadline[0] {
			t.Fatalf("queries carry different deadlines %v and %v, want one per walk", table.deadline[0], dl)
		}
	}
	if defaultProcessWalkBound != 10*time.Second {
		t.Errorf("defaultProcessWalkBound = %v, want 10s", defaultProcessWalkBound)
	}
	if left := time.Until(table.deadline[0]); left <= 5*time.Second || left > defaultProcessWalkBound {
		t.Errorf("walk deadline is %v away, want within (5s, %v]", left, defaultProcessWalkBound)
	}
}

// Each top-level walk gets its own deadline: one walk's expiry does not poison
// the next.
func TestIsRuntimeRunningWithError_DeadlineIsPerWalkNotPerTmux(t *testing.T) {
	table := newFakeProcessTable(map[string]fakeProc{"100": {comm: "bash", args: "bash"}})
	table.faults["pgrep 100"] = probeFault{block: true}
	tm := probeTmux(shellPane(), table, 20*time.Millisecond)
	if got, err := tm.IsRuntimeRunningWithError("s", []string{"claude"}); err == nil {
		t.Fatalf("first walk = (%v, nil), want unknown", got)
	}
	delete(table.faults, "pgrep 100")
	got, err := tm.IsRuntimeRunningWithError("s", []string{"claude"})
	if got || err != nil {
		t.Fatalf("second walk = (%v, %v), want (false, nil)", got, err)
	}
}

// The bool probe folds unknown to false.
func TestIsRuntimeRunning_BoolFoldsUnknownToFalse(t *testing.T) {
	table := newFakeProcessTable(map[string]fakeProc{"100": {comm: "bash", args: "bash"}})
	table.faults["pgrep 100"] = probeFault{err: exec.ErrNotFound}
	tm := probeTmux(shellPane(), table, 0)
	if tm.IsRuntimeRunning("s", []string{"claude"}) {
		t.Fatal("IsRuntimeRunning = true on an unknown probe, want false")
	}

	table = newFakeProcessTable(map[string]fakeProc{
		"100": {comm: "bash", args: "bash", children: []string{"101"}},
		"101": {comm: "claude", args: "claude"},
	})
	tm = probeTmux(shellPane(), table, 0)
	if !tm.IsRuntimeRunning("s", []string{"claude"}) {
		t.Fatal("IsRuntimeRunning = false with a claude child, want true")
	}
}

func TestIsRuntimeRunning_BoolBoundsAHungProbe(t *testing.T) {
	const bound = 50 * time.Millisecond
	table := newFakeProcessTable(map[string]fakeProc{"100": {comm: "bash", args: "bash"}})
	table.faults["pgrep 100"] = probeFault{block: true}
	tm := probeTmux(shellPane(), table, bound)
	start := time.Now()
	if tm.IsRuntimeRunning("s", []string{"claude"}) {
		t.Fatal("IsRuntimeRunning = true, want false")
	}
	if elapsed := time.Since(start); elapsed > 5*bound {
		t.Errorf("IsRuntimeRunning took %v, want about one bound (%v)", elapsed, bound)
	}
}

func TestIsRuntimeRunningWithError_EmptyProcessNamesRunNothing(t *testing.T) {
	panes := shellPane()
	table := newFakeProcessTable(nil)
	tm := probeTmux(panes, table, 0)
	for _, names := range [][]string{nil, {}} {
		got, err := tm.IsRuntimeRunningWithError("s", names)
		if got || err != nil {
			t.Fatalf("IsRuntimeRunningWithError(%v) = (%v, %v), want (false, nil)", names, got, err)
		}
	}
	if panes.callCount() != 0 || table.queryCount() != 0 {
		t.Errorf("ran %d tmux calls and %d process queries for empty names, want none", panes.callCount(), table.queryCount())
	}
}

// A pane query that says the session (or the server) is not there is a
// definitive no; any other failure is unknown.
func TestIsRuntimeRunningWithError_PaneQueryErrors(t *testing.T) {
	definite := []struct {
		name string
		err  error
	}{
		{"session not found", ErrSessionNotFound},
		{"wrapped session not found", fmt.Errorf("list-panes: %w", ErrSessionNotFound)},
		{"no server", ErrNoServer},
	}
	for _, tc := range definite {
		for _, which := range []string{"command", "pid"} {
			t.Run(tc.name+" from "+which, func(t *testing.T) {
				panes := shellPane()
				if which == "command" {
					panes.commandErr = tc.err
				} else {
					panes.pidErr = tc.err
				}
				table := newFakeProcessTable(nil)
				tm := probeTmux(panes, table, 0)
				got, err := tm.IsRuntimeRunningWithError("s", []string{"claude"})
				if got || err != nil {
					t.Fatalf("IsRuntimeRunningWithError = (%v, %v), want (false, nil)", got, err)
				}
				if n := table.queryCount(); n != 0 {
					t.Errorf("ran %d process queries after the session was gone, want none", n)
				}
			})
		}
	}

	unknown := []struct {
		name  string
		build func() *scriptedPanes
	}{
		{"command query failed", func() *scriptedPanes {
			p := shellPane()
			p.commandErr = errors.New("tmux: exit status 1: protocol error")
			return p
		}},
		{"pid query failed", func() *scriptedPanes {
			p := shellPane()
			p.pidErr = errors.New("tmux: exit status 1: protocol error")
			return p
		}},
		{"empty command", func() *scriptedPanes { return &scriptedPanes{command: "", pid: "100"} }},
		{"empty pid", func() *scriptedPanes { return &scriptedPanes{command: "bash", pid: ""} }},
		{"timed out", func() *scriptedPanes {
			p := shellPane()
			p.commandErr = context.DeadlineExceeded
			return p
		}},
	}
	for _, tc := range unknown {
		t.Run(tc.name, func(t *testing.T) {
			tm := probeTmux(tc.build(), newFakeProcessTable(nil), 0)
			got, err := tm.IsRuntimeRunningWithError("s", []string{"claude"})
			assertUnknown(t, got, err)
		})
	}
}

// A direct pane-command match needs no process query at all.
func TestIsRuntimeRunningWithError_DirectCommandMatch(t *testing.T) {
	table := newFakeProcessTable(nil)
	tm := probeTmux(&scriptedPanes{command: "claude", pid: "100"}, table, 0)
	got, err := tm.IsRuntimeRunningWithError("s", []string{"claude"})
	if !got || err != nil {
		t.Fatalf("IsRuntimeRunningWithError = (%v, %v), want (true, nil)", got, err)
	}
	if n := table.queryCount(); n != 0 {
		t.Errorf("ran %d process queries, want none", n)
	}
}

// IsSessionRunningWithError: has-session folds "not found" and "no server" to a
// definitive no; any other has-session failure is unknown; a pane-state error
// still reports running.
func TestIsSessionRunningWithError(t *testing.T) {
	t.Run("session gone", func(t *testing.T) {
		for _, gone := range []error{ErrSessionNotFound, ErrNoServer} {
			tm := &Tmux{cfg: Config{SocketName: "x"}, exec: &fakeExecutor{errs: []error{gone}}}
			got, err := tm.IsSessionRunningWithError("runner")
			if got || err != nil {
				t.Fatalf("IsSessionRunningWithError(%v) = (%v, %v), want (false, nil)", gone, got, err)
			}
		}
	})
	t.Run("has-session failed", func(t *testing.T) {
		tm := &Tmux{cfg: Config{SocketName: "x"}, exec: &fakeExecutor{errs: []error{errors.New("tmux: exit status 1: protocol error")}}}
		got, err := tm.IsSessionRunningWithError("runner")
		assertUnknown(t, got, err)
	})
	t.Run("dead pane", func(t *testing.T) {
		tm := &Tmux{cfg: Config{SocketName: "x"}, exec: &fakeExecutor{outs: []string{"", "pane\t1"}}}
		got, err := tm.IsSessionRunningWithError("runner")
		if got || err != nil {
			t.Fatalf("IsSessionRunningWithError = (%v, %v), want (false, nil)", got, err)
		}
	})
	t.Run("live pane", func(t *testing.T) {
		tm := &Tmux{cfg: Config{SocketName: "x"}, exec: &fakeExecutor{outs: []string{"", "pane\t0"}}}
		got, err := tm.IsSessionRunningWithError("runner")
		if !got || err != nil {
			t.Fatalf("IsSessionRunningWithError = (%v, %v), want (true, nil)", got, err)
		}
	})
	t.Run("pane state unreadable stays running", func(t *testing.T) {
		tm := &Tmux{cfg: Config{SocketName: "x"}, exec: &fakeExecutor{outs: []string{""}, errs: []error{nil, ErrNoServer}}}
		got, err := tm.IsSessionRunningWithError("runner")
		if !got || err != nil {
			t.Fatalf("IsSessionRunningWithError = (%v, %v), want (true, nil)", got, err)
		}
	})
}

// The unknown error names the session and ErrRuntimeUnavailable, never the
// cause text: causes routinely say "not found" or "no tmux server running",
// which runtime.IsSessionGone reads as the session being gone.
func TestUnknownProbeError_NeverReadsAsGone(t *testing.T) {
	causes := []string{
		"session not found",
		"not running",
		`exec: "ps": executable file not found in $PATH`,
		"no tmux server running on /tmp/tmux-1000/default",
	}
	for _, cause := range causes {
		t.Run(cause, func(t *testing.T) {
			table := newFakeProcessTable(map[string]fakeProc{"100": {comm: "bash", args: "bash"}})
			table.faults["pgrep 100"] = probeFault{err: errors.New(cause)}
			tm := probeTmux(shellPane(), table, 0)
			got, err := tm.IsRuntimeRunningWithError("gc-sess", []string{"claude"})
			assertUnknown(t, got, err)
			if strings.Contains(err.Error(), cause) {
				t.Errorf("error %q repeats the cause %q", err, cause)
			}
			if !strings.Contains(err.Error(), "gc-sess") {
				t.Errorf("error %q does not name the session", err)
			}
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Errorf("error %v does not wrap runtime.ErrRuntimeUnavailable", err)
			}

			// The same through the session probe.
			stm := &Tmux{cfg: Config{SocketName: "x"}, exec: &fakeExecutor{errs: []error{errors.New(cause)}}}
			got, err = stm.IsSessionRunningWithError("gc-sess")
			assertUnknown(t, got, err)
			if strings.Contains(err.Error(), cause) {
				t.Errorf("session probe error %q repeats the cause %q", err, cause)
			}
		})
	}
}

// Fixed quartet runtime.IsSessionGone matches as gone, spelled out so a change
// to its list fails here too.
func TestUnknownProbeError_AvoidsEverySessionGoneSubstring(t *testing.T) {
	table := newFakeProcessTable(map[string]fakeProc{"100": {comm: "bash", args: "bash"}})
	table.faults["pgrep 100"] = probeFault{code: 2}
	tm := probeTmux(shellPane(), table, 0)
	_, err := tm.IsRuntimeRunningWithError("gc-sess", []string{"claude"})
	if err == nil {
		t.Fatal("want an unknown error")
	}
	for _, sub := range []string{"session not found", "not running", "not found", "no tmux server running"} {
		if strings.Contains(strings.ToLower(err.Error()), sub) {
			t.Errorf("error %q contains %q, which runtime.IsSessionGone reads as gone", err, sub)
		}
	}
}

// The bool IsSessionRunning keeps its behavior: unknown is false.
func TestIsSessionRunning_BoolUnchanged(t *testing.T) {
	tm := &Tmux{cfg: Config{SocketName: "x"}, exec: &fakeExecutor{errs: []error{errors.New("boom")}}}
	if tm.IsSessionRunning("runner") {
		t.Fatal("IsSessionRunning = true when has-session failed, want false")
	}
}
