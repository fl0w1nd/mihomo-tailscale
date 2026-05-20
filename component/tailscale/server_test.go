//go:build with_tailscale

package tailscale

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// resetRegistryForTest clears the package-level registry. It cancels any
// pending close timers so leftover state from one test cannot leak into
// the next. Callers should defer it as well to keep the registry tidy
// for any later test that runs in the same process.
func resetRegistryForTest(t *testing.T) {
	t.Helper()
	regMu.Lock()
	defer regMu.Unlock()
	for _, inst := range reg {
		if inst.closeTimer != nil {
			inst.closeTimer.Stop()
			inst.closeTimer = nil
		}
	}
	reg = map[string]*Instance{}
}

func withShortGrace(t *testing.T, d time.Duration) {
	t.Helper()
	prev := closeGracePeriod
	closeGracePeriod = d
	t.Cleanup(func() { closeGracePeriod = prev })
}

func TestAcquireSameIdentityShares(t *testing.T) {
	resetRegistryForTest(t)
	t.Cleanup(func() { resetRegistryForTest(t) })

	opts := ServerOptions{Name: "shared", StateDir: t.TempDir()}
	a, releaseA := Acquire(opts)
	b, releaseB := Acquire(opts)

	if a != b {
		t.Fatalf("Acquire returned different instances for same identity")
	}

	regMu.Lock()
	got := a.refs
	regMu.Unlock()
	if got != 2 {
		t.Fatalf("refs = %d, want 2", got)
	}

	releaseA()
	regMu.Lock()
	got = a.refs
	timer := a.closeTimer
	regMu.Unlock()
	if got != 1 {
		t.Fatalf("refs after first release = %d, want 1", got)
	}
	if timer != nil {
		t.Fatalf("close timer scheduled with refs > 0")
	}

	releaseB()
	regMu.Lock()
	got = a.refs
	timer = a.closeTimer
	regMu.Unlock()
	if got != 0 {
		t.Fatalf("refs after second release = %d, want 0", got)
	}
	if timer == nil {
		t.Fatalf("close timer not scheduled after final release")
	}
	timer.Stop()
}

func TestReleaseIsIdempotent(t *testing.T) {
	resetRegistryForTest(t)
	t.Cleanup(func() { resetRegistryForTest(t) })

	opts := ServerOptions{Name: "idem", StateDir: t.TempDir()}
	inst, release := Acquire(opts)

	release()
	release() // second call must be a no-op
	release() // and again

	regMu.Lock()
	got := inst.refs
	regMu.Unlock()
	if got != 0 {
		t.Fatalf("refs after repeated release = %d, want 0", got)
	}
}

func TestAcquireCancelsPendingClose(t *testing.T) {
	resetRegistryForTest(t)
	t.Cleanup(func() { resetRegistryForTest(t) })
	withShortGrace(t, 200*time.Millisecond)

	opts := ServerOptions{Name: "grace", StateDir: t.TempDir()}
	inst, release := Acquire(opts)
	release()

	regMu.Lock()
	timer := inst.closeTimer
	regMu.Unlock()
	if timer == nil {
		t.Fatalf("expected close timer after final release")
	}

	revived, release2 := Acquire(opts)
	if revived != inst {
		t.Fatalf("re-Acquire within grace returned different instance")
	}
	regMu.Lock()
	timer = inst.closeTimer
	regMu.Unlock()
	if timer != nil {
		t.Fatalf("re-Acquire did not cancel pending close timer")
	}

	// Wait past the original grace window: the cancelled timer must not
	// have torn the instance down.
	time.Sleep(400 * time.Millisecond)

	regMu.Lock()
	_, present := reg[inst.key]
	regMu.Unlock()
	if !present {
		t.Fatalf("instance evicted from registry despite re-Acquire")
	}

	release2()
}

func TestCloseTimerRemovesEntry(t *testing.T) {
	resetRegistryForTest(t)
	t.Cleanup(func() { resetRegistryForTest(t) })
	withShortGrace(t, 20*time.Millisecond)

	opts := ServerOptions{Name: "evict", StateDir: t.TempDir()}
	inst, release := Acquire(opts)
	release()

	deadline := time.After(2 * time.Second)
	for {
		regMu.Lock()
		_, present := reg[inst.key]
		regMu.Unlock()
		if !present {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("instance still in registry after grace period")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestAcquireConflictFirstWinsForNodeOptions(t *testing.T) {
	resetRegistryForTest(t)
	t.Cleanup(func() { resetRegistryForTest(t) })

	base := ServerOptions{Name: "conflict", StateDir: t.TempDir(), AuthKey: "tskey-first"}
	first, releaseFirst := Acquire(base)
	defer releaseFirst()

	enabled := true
	conflict := base
	conflict.Ephemeral = true
	conflict.AcceptRoutes = &enabled
	conflict.AuthKey = "tskey-second"

	second, releaseSecond := Acquire(conflict)
	defer releaseSecond()

	if first != second {
		t.Fatalf("conflicting Acquire returned different instance pointer")
	}
	first.optionMu.Lock()
	gotEphemeral := first.option.Ephemeral
	gotAcceptRoutes := first.option.AcceptRoutes
	gotAuthKey := first.option.AuthKey
	first.optionMu.Unlock()

	if gotEphemeral != false {
		t.Fatalf("first-wins violated: Ephemeral=%v, want false", gotEphemeral)
	}
	if gotAcceptRoutes != nil {
		t.Fatalf("first-wins violated: AcceptRoutes=%v, want nil", *gotAcceptRoutes)
	}
	if gotAuthKey != "tskey-first" {
		t.Fatalf("first-wins violated for non-empty auth-key: got %q, want tskey-first", gotAuthKey)
	}
}

func TestAcquirePromotesEmptyAuthKey(t *testing.T) {
	resetRegistryForTest(t)
	t.Cleanup(func() { resetRegistryForTest(t) })

	dir := t.TempDir()
	first, releaseFirst := Acquire(ServerOptions{Name: "promote", StateDir: dir})
	defer releaseFirst()

	first.optionMu.Lock()
	got := first.option.AuthKey
	first.optionMu.Unlock()
	if got != "" {
		t.Fatalf("seed auth-key should be empty, got %q", got)
	}

	second, releaseSecond := Acquire(ServerOptions{Name: "promote", StateDir: dir, AuthKey: "tskey-bootstrap"})
	defer releaseSecond()

	if first != second {
		t.Fatalf("Acquire returned different instance for same identity")
	}

	first.optionMu.Lock()
	got = first.option.AuthKey
	first.optionMu.Unlock()
	if got != "tskey-bootstrap" {
		t.Fatalf("auth-key was not promoted onto shared identity: got %q, want tskey-bootstrap", got)
	}
}

func TestAcquireDoesNotPromoteAuthKeyAfterStart(t *testing.T) {
	resetRegistryForTest(t)
	t.Cleanup(func() { resetRegistryForTest(t) })

	dir := t.TempDir()
	first, releaseFirst := Acquire(ServerOptions{Name: "no-promote", StateDir: dir})
	defer releaseFirst()

	// Simulate the shared tsnet having already authenticated. Once initOk
	// is set, any later auth-key has nothing to do: tsnet only consults
	// the key on the first Up() call.
	first.initOk.Store(true)

	_, releaseSecond := Acquire(ServerOptions{Name: "no-promote", StateDir: dir, AuthKey: "tskey-too-late"})
	defer releaseSecond()

	first.optionMu.Lock()
	got := first.option.AuthKey
	first.optionMu.Unlock()
	if got != "" {
		t.Fatalf("auth-key promoted after Start: got %q, want empty", got)
	}
}

func TestAcquireDoesNotPromoteAuthKeyDuringStart(t *testing.T) {
	resetRegistryForTest(t)
	t.Cleanup(func() { resetRegistryForTest(t) })

	dir := t.TempDir()
	first, releaseFirst := Acquire(ServerOptions{Name: "starting", StateDir: dir})
	defer releaseFirst()

	first.optionMu.Lock()
	first.startInProgress = true
	first.optionMu.Unlock()
	defer first.endStartAttempt()

	_, releaseSecond := Acquire(ServerOptions{Name: "starting", StateDir: dir, AuthKey: "tskey-during-start"})
	defer releaseSecond()

	first.optionMu.Lock()
	got := first.option.AuthKey
	first.optionMu.Unlock()
	if got != "" {
		t.Fatalf("auth-key promoted during Start: got %q, want empty", got)
	}
}

func TestStartWaitHonorsContext(t *testing.T) {
	resetRegistryForTest(t)
	t.Cleanup(func() { resetRegistryForTest(t) })

	inst, release := Acquire(ServerOptions{Name: "ctx-lock", StateDir: t.TempDir()})
	defer release()

	inst.initMutex.Lock()
	defer inst.initMutex.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	server, err := inst.Start(ctx)
	if server != nil {
		t.Fatalf("Start returned server while init mutex was held: %v", server)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start error = %v, want context deadline exceeded", err)
	}
}

func TestCloseTimerWaitsForStartInProgress(t *testing.T) {
	resetRegistryForTest(t)
	t.Cleanup(func() { resetRegistryForTest(t) })
	withShortGrace(t, 20*time.Millisecond)

	inst, release := Acquire(ServerOptions{Name: "closing-start", StateDir: t.TempDir()})
	inst.optionMu.Lock()
	inst.startInProgress = true
	inst.optionMu.Unlock()

	release()
	time.Sleep(80 * time.Millisecond)

	regMu.Lock()
	_, present := reg[inst.key]
	regMu.Unlock()
	if !present {
		t.Fatalf("instance evicted while Start was still in progress")
	}

	inst.endStartAttempt()
	deadline := time.After(2 * time.Second)
	for {
		regMu.Lock()
		_, present = reg[inst.key]
		regMu.Unlock()
		if !present {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("instance still in registry after Start completed")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestIdentityKeyResolvesPath(t *testing.T) {
	dir := t.TempDir()
	a := identityKeyFromOpts(ServerOptions{Name: "a", StateDir: dir})
	b := identityKeyFromOpts(ServerOptions{Name: "a", StateDir: dir + "/"})
	if a != b {
		t.Fatalf("identity keys differ for path with/without trailing slash:\n  a=%q\n  b=%q", a, b)
	}
}

func TestIdentityKeyHostnameOverridesName(t *testing.T) {
	a := identityKeyFromOpts(ServerOptions{Name: "alpha", Hostname: "shared"})
	b := identityKeyFromOpts(ServerOptions{Name: "beta", Hostname: "shared"})
	if a != b {
		t.Fatalf("identity keys differ when only Name varies:\n  a=%q\n  b=%q", a, b)
	}
}

func TestIdentityKeyDiffersWhenIdentityDiffers(t *testing.T) {
	a := identityKeyFromOpts(ServerOptions{Name: "one"})
	b := identityKeyFromOpts(ServerOptions{Name: "two"})
	if a == b {
		t.Fatalf("identity keys collide for distinct names")
	}

	a = identityKeyFromOpts(ServerOptions{Name: "same", ControlURL: "https://login.example/"})
	b = identityKeyFromOpts(ServerOptions{Name: "same", ControlURL: "https://other.example/"})
	if a == b {
		t.Fatalf("identity keys collide for distinct control URLs")
	}
}

func TestAcquireConcurrent(t *testing.T) {
	resetRegistryForTest(t)
	t.Cleanup(func() { resetRegistryForTest(t) })

	opts := ServerOptions{Name: "concurrent", StateDir: t.TempDir()}
	const n = 64

	var wg sync.WaitGroup
	instances := make([]*Instance, n)
	releases := make([]func(), n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			instances[idx], releases[idx] = Acquire(opts)
		}(i)
	}
	wg.Wait()

	for i := 1; i < n; i++ {
		if instances[i] != instances[0] {
			t.Fatalf("concurrent Acquire returned different instance at index %d", i)
		}
	}
	regMu.Lock()
	got := instances[0].refs
	regMu.Unlock()
	if got != n {
		t.Fatalf("refs after concurrent Acquire = %d, want %d", got, n)
	}

	for _, release := range releases {
		release()
	}
	regMu.Lock()
	got = instances[0].refs
	regMu.Unlock()
	if got != 0 {
		t.Fatalf("refs after concurrent release = %d, want 0", got)
	}
}
