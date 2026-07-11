package pool

// pool_test.go exercises the worker pool semantics from the plan (Phase 3):
//   - Acquire for an idle/fresh channel spawns a slot and switch_sessions.
//   - Same-channel second Acquire serializes (blocks until Release) and reuses
//     the same process (no respawn, no drain).
//   - Different-channel reuse of a still-streaming slot drains (waits for
//     agent_end up to DRAIN_TIMEOUT, then aborts) then switch_sessions to the
//     new channel's file.
//   - All-slots-busy: an (M+1)th Acquire blocks until a slot frees.
//   - Release keeps the process warm (no kill).
//   - LRU selection of idle warm slots, with respawn on cross-channel reuse so
//     WALLE_CHANNEL stays correct.
//   - Shutdown aborts streaming slots, kills all, returns; Acquire after
//     Shutdown returns ErrPoolClosed.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"wall-e/rpc"
	"wall-e/session"
)

// fakeHandlerCfg configures a per-slot fake pi handler.
type fakeHandlerCfg struct {
	sessionFile string
	// streamDone: if non-nil, a prompt emits agent_start then waits for this
	// channel to be closed before emitting agent_end (simulating a long
	// stream). If nil, agent_end is emitted immediately after agent_start.
	streamDone chan struct{}
}

func makeHandler(cfg fakeHandlerCfg) func(f *fakePI, cmd map[string]any) {
	return func(f *fakePI, cmd map[string]any) {
		id, _ := cmd["id"].(string)
		ctype, _ := cmd["type"].(string)
		switch ctype {
		case "switch_session":
			f.writeResp(id, "switch_session", true, map[string]any{"cancelled": false})
		case "get_state":
			f.writeResp(id, "get_state", true, map[string]any{
				"data": map[string]any{
					"sessionFile": cfg.sessionFile,
					"isStreaming": false,
				},
			})
		case "prompt":
			f.writeResp(id, "prompt", true, nil)
			f.writeJSON(map[string]any{"type": "agent_start"})
			if cfg.streamDone != nil {
				done := cfg.streamDone
				go func() { <-done; f.writeJSON(map[string]any{"type": "agent_end"}) }()
			} else {
				f.writeJSON(map[string]any{"type": "agent_end"})
			}
		case "abort":
			f.writeResp(id, "abort", true, nil)
			f.writeJSON(map[string]any{"type": "agent_end"})
		case "steer", "follow_up", "compact", "bash", "set_model", "clone", "new_session":
			f.writeResp(id, ctype, true, nil)
		default:
			f.writeResp(id, ctype, false, nil)
		}
	}
}

// testPool builds a pool backed by a fakeFactory and a session.Manager over a
// temp dir. The factory spawns fakes whose handler is built per-call via
// makeHandlerFor (so each slot can have its own sessionFile / streamDone).
func testPool(t *testing.T, size int, makeHandlerFor func(slotIdx int) fakeHandlerCfg) (*Pool, *fakeFactory, *session.Manager) {
	t.Helper()
	dir := t.TempDir()
	sm, err := session.New(session.Config{SessionDir: dir})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	if err := sm.RebuildFromDir(); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	ff := newFakeFactory()
	var idxMu sync.Mutex
	idx := 0
	newClient := func(cfg rpc.Config) (*rpc.Client, error) {
		idxMu.Lock()
		i := idx
		idx++
		idxMu.Unlock()
		hcfg := makeHandlerFor(i)
		f := newFakePI()
		h := makeHandler(hcfg)
		f.start(func(pf *fakePI, cmd map[string]any) { h(pf, cmd) })
		c := rpc.NewClientFromStreams(f.stdinWriter, f.stdoutReader, cfg)
		ff.mu.Lock()
		id := ff.nextID
		ff.nextID++
		ff.fakes[id] = f
		env := make(map[string]string, len(cfg.Env))
		for k, v := range cfg.Env {
			env[k] = v
		}
		ff.envs[id] = env
		ff.mu.Unlock()
		return c, nil
	}
	p, err := New(Config{
		Size:         size,
		DrainTimeout: 100 * time.Millisecond,
		Sessions:     sm,
		RPCConfig:    rpc.Config{UIPolicy: rpc.DefaultExtensionUIPolicy()},
		NewClient:    newClient,
	})
	if err != nil {
		t.Fatalf("pool.New: %v", err)
	}
	t.Cleanup(func() {
		_ = p.Shutdown(context.Background())
		ff.closeAll()
	})
	return p, ff, sm
}

func ctxT(t *testing.T) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// waitForAgentStart reads the slot's event tap until an agent_start arrives.
func waitForAgentStart(t *testing.T, s *Slot, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				t.Fatalf("events closed before agent_start")
			}
			if ev.Type == rpc.EventAgentStart {
				return
			}
		case <-time.After(5 * time.Millisecond):
		}
	}
	t.Fatalf("did not observe agent_start within %v", timeout)
}

// --- Tests ----------------------------------------------------------------

// TestPool_Acquire_IdleChannel: fresh channel → slot spawned, prompt works,
// switch_session to the channel's current session file is sent.
func TestPool_Acquire_IdleChannel(t *testing.T) {
	p, ff, sm := testPool(t, 2, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/sess.jsonl"}
	})

	slot, err := p.Acquire(context.Background(), "chanA")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer p.Release("chanA")

	// switch_session must have been sent to the channel's current path.
	path, _ := sm.Current("chanA")
	if ff.count() != 1 {
		t.Errorf("expected 1 spawned process, got %d", ff.count())
	}
	if got := fakeContainsAny(t, ff, "switch_session"); !got {
		t.Errorf("expected switch_session to be sent")
	}
	// The switch_session line should reference the channel's session path.
	basename := filepath.Base(path)
	if !fakeContainsAny(t, ff, basename) {
		t.Errorf("expected switch_session to reference %q", basename)
	}

	// Prompt works on the acquired slot.
	ctx, cancel := ctxT(t)
	defer cancel()
	resp, err := slot.Client().Prompt(ctx, "hi", false)
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if !resp.Success {
		t.Errorf("prompt not accepted: %+v", resp)
	}
}

// TestPool_Acquire_BusySameChannel: a second Acquire for the same channel
// while the first is held blocks until Release and then returns the SAME
// underlying process (no respawn, per-channel serialization).
func TestPool_Acquire_BusySameChannel(t *testing.T) {
	p, ff, _ := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/sess.jsonl"}
	})

	slot1, err := p.Acquire(context.Background(), "chanA")
	if err != nil {
		t.Fatalf("Acquire 1: %v", err)
	}
	client1 := slot1.Client()

	type res struct {
		slot *Slot
		err  error
	}
	resCh := make(chan res, 1)
	go func() {
		s, err := p.Acquire(context.Background(), "chanA")
		resCh <- res{s, err}
	}()

	// Should still be blocked.
	select {
	case <-resCh:
		t.Fatalf("second Acquire returned before Release")
	case <-time.After(50 * time.Millisecond):
	}

	p.Release("chanA")

	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("second Acquire: %v", r.err)
		}
		if r.slot.Client() != client1 {
			t.Errorf("expected same client reused, got different")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("second Acquire did not return after Release")
	}

	// No respawn: only one process ever spawned.
	if ff.count() != 1 {
		t.Errorf("expected 1 process (reuse), got %d", ff.count())
	}
}

// TestPool_Acquire_DifferentChannel_DrainsAndSwitches: with M=1, chanA's slot
// is streaming (agent_end withheld); chanA releases; chanB acquires the same
// slot. The pool must drain (wait for agent_end within timeout → no abort)
// and then switch_session to chanB's file.
func TestPool_Acquire_DifferentChannel_DrainsAndSwitches(t *testing.T) {
	streamDone := make(chan struct{})
	p, ff, sm := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/sess.jsonl", streamDone: streamDone}
	})

	// chanA: acquire, prompt (agent_start, agent_end withheld).
	slotA, err := p.Acquire(context.Background(), "chanA")
	if err != nil {
		t.Fatalf("Acquire chanA: %v", err)
	}
	ctx, cancel := ctxT(t)
	defer cancel()
	if _, err := slotA.Client().Prompt(ctx, "msg", false); err != nil {
		t.Fatalf("Prompt chanA: %v", err)
	}
	waitForAgentStart(t, slotA, time.Second)

	// chanA still streaming → its fake has NOT received agent_end yet.
	if fakeContainsAny(t, ff, "abort") {
		t.Errorf("abort sent before drain")
	}

	// Release chanA: the slot becomes idle but the process is still streaming
	// (agent_end withheld). Reusing it for a different channel must drain.
	p.Release("chanA")

	// chanB acquires → pool reuses the slot, drains (streaming), switch_session.
	pathB, _ := sm.Current("chanB")
	basenameB := filepath.Base(pathB)

	// Inject agent_end within the drain timeout → no abort.
	go func() { close(streamDone) }()

	_, err = p.Acquire(context.Background(), "chanB")
	if err != nil {
		t.Fatalf("Acquire chanB: %v", err)
	}
	defer p.Release("chanB")

	if !fakeContainsAny(t, ff, basenameB) {
		t.Errorf("expected switch_session to chanB's file %q", basenameB)
	}
	if fakeContainsAny(t, ff, `"abort"`) {
		t.Errorf("abort was sent despite agent_end arriving within timeout")
	}
}

// TestPool_Acquire_DifferentChannel_AbortsWhenNoAgentEnd: same as above but
// agent_end never arrives → the pool aborts after DRAIN_TIMEOUT, then
// switch_sessions.
func TestPool_Acquire_DifferentChannel_AbortsWhenNoAgentEnd(t *testing.T) {
	// streamDone is never closed.
	streamDone := make(chan struct{})
	p, ff, sm := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/sess.jsonl", streamDone: streamDone}
	})

	slotA, err := p.Acquire(context.Background(), "chanA")
	if err != nil {
		t.Fatalf("Acquire chanA: %v", err)
	}
	ctx, cancel := ctxT(t)
	defer cancel()
	if _, err := slotA.Client().Prompt(ctx, "msg", false); err != nil {
		t.Fatalf("Prompt chanA: %v", err)
	}
	waitForAgentStart(t, slotA, time.Second)

	p.Release("chanA")

	pathB, _ := sm.Current("chanB")
	basenameB := filepath.Base(pathB)

	start := time.Now()
	_, err = p.Acquire(context.Background(), "chanB")
	if err != nil {
		t.Fatalf("Acquire chanB: %v", err)
	}
	defer p.Release("chanB")
	elapsed := time.Since(start)

	// Should have waited ~DRAIN_TIMEOUT (100ms) before aborting.
	if elapsed < 90*time.Millisecond {
		t.Errorf("drain did not wait for timeout; elapsed %v", elapsed)
	}
	if !fakeContainsAny(t, ff, `"abort"`) {
		t.Errorf("expected abort to be sent after drain timeout")
	}
	if !fakeContainsAny(t, ff, basenameB) {
		t.Errorf("expected switch_session to chanB's file %q after abort", basenameB)
	}
}

// TestPool_AllSlotsBusy_BlocksThenAcquires: M slots all busy; an (M+1)th
// Acquire blocks until a slot frees; then it runs. Cross-channel reuse respawns
// the freed slot so WALLE_CHANNEL is correct.
func TestPool_AllSlotsBusy_BlocksThenAcquires(t *testing.T) {
	p, ff, _ := testPool(t, 2, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/sess.jsonl"}
	})

	slotA, _ := p.Acquire(context.Background(), "chanA")
	_, _ = p.Acquire(context.Background(), "chanB")
	defer p.Release("chanB")
	defer p.Release("chanA")

	if ff.count() != 2 {
		t.Fatalf("expected 2 processes, got %d", ff.count())
	}

	type res struct {
		err error
	}
	resCh := make(chan res, 1)
	go func() {
		_, err := p.Acquire(context.Background(), "chanC")
		resCh <- res{err}
	}()

	// Blocked.
	select {
	case <-resCh:
		t.Fatalf("chanC Acquire returned before a slot freed")
	case <-time.After(50 * time.Millisecond):
	}

	// Free chanA; chanC should now proceed (reuse the idle slot).
	freed := make(chan struct{})
	go func() {
		p.Release("chanA")
		close(freed)
	}()

	select {
	case r := <-resCh:
		if r.err != nil {
			t.Fatalf("chanC Acquire: %v", r.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("chanC Acquire did not return after a slot freed")
	}

	// Ordering: chanC acquired after chanA released.
	select {
	case <-freed:
	default:
		t.Errorf("chanC acquired before chanA released")
	}

	// Cross-channel reuse respawns the freed slot so the process environment can
	// carry the new WALLE_CHANNEL value.
	if ff.count() != 3 {
		t.Errorf("expected 3 total spawned processes after cross-channel respawn, got %d", ff.count())
	}

	_ = slotA
}

// TestPool_Release_KeepsProcessAlive: Release does not kill the process; a
// subsequent same-channel Acquire reuses the same client (no respawn).
func TestPool_Release_KeepsProcessAlive(t *testing.T) {
	p, ff, _ := testPool(t, 2, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/sess.jsonl"}
	})

	slot1, _ := p.Acquire(context.Background(), "chanA")
	client1 := slot1.Client()
	p.Release("chanA")

	slot2, _ := p.Acquire(context.Background(), "chanA")
	defer p.Release("chanA")

	if slot2.Client() != client1 {
		t.Errorf("expected same client reused after Release (process kept warm)")
	}
	if ff.count() != 1 {
		t.Errorf("expected 1 process (warm reuse), got %d", ff.count())
	}
}

func TestPool_WALLEChannelEnv_SetOnSpawnAndCrossChannelRespawn(t *testing.T) {
	p, ff, _ := testPool(t, 1, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/sess.jsonl"}
	})

	chA := ChannelID(session.NewChannelID("telegram", "123456789"))
	sA, err := p.Acquire(context.Background(), chA)
	if err != nil {
		t.Fatalf("Acquire telegram: %v", err)
	}
	clientA := sA.Client()
	p.Release(chA)
	if got := ff.env(0)["WALLE_CHANNEL"]; got != "telegram:123456789" {
		t.Fatalf("first process WALLE_CHANNEL = %q, want telegram:123456789", got)
	}

	chB := ChannelID(session.NewChannelID("http", "morning-digest"))
	sB, err := p.Acquire(context.Background(), chB)
	if err != nil {
		t.Fatalf("Acquire http: %v", err)
	}
	defer p.Release(chB)
	if sB.Client() == clientA {
		t.Fatalf("cross-channel acquire reused process instead of respawning")
	}
	if ff.count() != 2 {
		t.Fatalf("spawn count = %d, want 2", ff.count())
	}
	if got := ff.env(1)["WALLE_CHANNEL"]; got != "http:morning-digest" {
		t.Fatalf("second process WALLE_CHANNEL = %q, want http:morning-digest", got)
	}
}

// TestPool_LRU_ReusesIdleSlot: with M=2 and two idle warm slots (chanA, chanB),
// a new channel chanC claims the LRU idle slot and respawns its process so the
// environment has the correct WALLE_CHANNEL.
func TestPool_LRU_ReusesIdleSlot(t *testing.T) {
	p, ff, _ := testPool(t, 2, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/sess.jsonl"}
	})

	// Warm up two slots.
	sA, _ := p.Acquire(context.Background(), "chanA")
	clientA := sA.Client()
	p.Release("chanA")
	// Small delay so chanA's lastUsed < chanB's lastUsed (LRU).
	time.Sleep(10 * time.Millisecond)
	sB, _ := p.Acquire(context.Background(), "chanB")
	clientB := sB.Client()
	p.Release("chanB")

	if ff.count() != 2 {
		t.Fatalf("expected 2 warm processes, got %d", ff.count())
	}

	// chanC reuses the LRU (chanA) slot — no spawn.
	sC, _ := p.Acquire(context.Background(), "chanC")
	defer p.Release("chanC")

	if ff.count() != 3 {
		t.Errorf("expected cross-channel respawn (3 total spawned processes), got %d", ff.count())
	}
	// Same-channel warm reuse is preserved, but cross-channel reuse intentionally
	// replaces the process to avoid a stale WALLE_CHANNEL.
	if sC.Client() == clientA || sC.Client() == clientB {
		t.Errorf("chanC reused a previous client despite needing a new WALLE_CHANNEL")
	}
	_ = sA
	_ = sB
}

// TestPool_Shutdown_DrainsAll: Shutdown aborts streaming slots, waits up to
// DRAIN_TIMEOUT, kills all processes, and returns. Subsequent Acquire fails.
func TestPool_Shutdown_DrainsAll(t *testing.T) {
	streamDone := make(chan struct{})
	p, ff, _ := testPool(t, 2, func(i int) fakeHandlerCfg {
		return fakeHandlerCfg{sessionFile: "/fake/sess.jsonl", streamDone: streamDone}
	})

	// Acquire two slots and start streaming (agent_end withheld).
	sA, _ := p.Acquire(context.Background(), "chanA")
	sB, _ := p.Acquire(context.Background(), "chanB")
	ctx, cancel := ctxT(t)
	defer cancel()
	if _, err := sA.Client().Prompt(ctx, "msg", false); err != nil {
		t.Fatalf("Prompt A: %v", err)
	}
	if _, err := sB.Client().Prompt(ctx, "msg", false); err != nil {
		t.Fatalf("Prompt B: %v", err)
	}
	waitForAgentStart(t, sA, time.Second)
	waitForAgentStart(t, sB, time.Second)
	p.Release("chanA")
	p.Release("chanB")

	// Both slots idle but streaming. Shutdown must abort both.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutCancel()
	if err := p.Shutdown(shutCtx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// Each fake should have received an abort.
	abortCount := 0
	for _, f := range ff.all() {
		if f.Contains(`"abort"`) {
			abortCount++
		}
	}
	if abortCount != 2 {
		t.Errorf("expected 2 aborts, got %d", abortCount)
	}

	// Acquire after Shutdown fails.
	if _, err := p.Acquire(context.Background(), "chanZ"); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("expected ErrPoolClosed after Shutdown, got %v", err)
	}
}

// fakeContainsAny reports whether any spawned fake received a line containing
// substr.
func fakeContainsAny(t *testing.T, ff *fakeFactory, substr string) bool {
	t.Helper()
	for _, f := range ff.all() {
		if f.Contains(substr) {
			return true
		}
	}
	return false
}
