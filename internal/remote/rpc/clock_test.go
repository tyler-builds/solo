package rpc

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/tyler-builds/solo/internal/core"
)

// The machine's time by a device's clock, which is behind the machine's or ahead of it, or loses time
// while the device sleeps.
func TestMachineClock(t *testing.T) {
	local := time.Unix(1_800_000_000, 0) // this device's clock at the welcome
	ms := func(t time.Time) int64 { return t.UnixMilli() }
	for _, skew := range []time.Duration{10 * time.Minute, -10 * time.Minute} {
		machine := local.Add(skew) // the machine's clock at the welcome
		var k machineClock
		if got := k.now(local); !got.Equal(local) {
			t.Fatalf("skew %v: before any reading, now is %v, want this device's %v", skew, got, local)
		}
		k.read(ms(machine), local, true)
		if got := k.now(local.Add(5 * time.Second)); !got.Equal(machine.Add(5 * time.Second)) {
			t.Fatalf("skew %v: 5s after the welcome, now is %v, want %v", skew, got, machine.Add(5*time.Second))
		}

		// A sync that took a while to arrive reads earlier than the estimate: it stays put.
		k.read(ms(machine.Add(4*time.Second)), local.Add(6*time.Second), false)
		if got := k.now(local.Add(6 * time.Second)); !got.Equal(machine.Add(6 * time.Second)) {
			t.Fatalf("skew %v: a slow sync moved the clock back to %v", skew, got)
		}
		// A machine too old to stamp its syncs changes nothing.
		k.read(0, local.Add(7*time.Second), false)
		if got := k.now(local.Add(7 * time.Second)); !got.Equal(machine.Add(7 * time.Second)) {
			t.Fatalf("skew %v: an unstamped sync moved the clock to %v", skew, got)
		}

		// The device slept 30 minutes and its clock didn't count them: the next sync catches it up.
		asleep := 30 * time.Minute
		woke := local.Add(8 * time.Second) // what this device's clock says on waking
		if got := k.now(woke); !got.Equal(machine.Add(8 * time.Second)) {
			t.Fatalf("skew %v: on waking, now is %v", skew, got)
		}
		k.read(ms(machine.Add(asleep+8*time.Second)), woke, false)
		if got, want := k.now(woke.Add(time.Second)), machine.Add(asleep+9*time.Second); !got.Equal(want) {
			t.Fatalf("skew %v: after a sync on waking, now is %v, want %v", skew, got, want)
		}

		// A new session starts over from its welcome, even behind the estimate (the machine's clock was
		// set back).
		k.read(ms(machine), woke.Add(10*time.Second), true)
		if got := k.now(woke.Add(10 * time.Second)); !got.Equal(machine) {
			t.Fatalf("skew %v: a new session's welcome left the clock at %v, want %v", skew, got, machine)
		}
	}
}

// Over a connection, a turn's running time is right whatever this device's clock says: minutes behind
// the machine's or ahead of it, after it loses time asleep with the session still up, and after it
// jumps ahead and the device reconnects.
func TestTurnTimeOverSkewedClocks(t *testing.T) {
	for _, skew := range []time.Duration{10 * time.Minute, -10 * time.Minute} {
		var off atomic.Int64 // this device's clock minus the machine's
		off.Store(int64(skew))
		deviceNow := func() time.Time { return time.Now().Add(time.Duration(off.Load())) }

		m := newMem()
		m.addThread("w1", core.StatusRunning)
		const ran = 90 * time.Second
		m.update("w1", func(v *core.ThreadView) { v.InTurn, v.TurnStarted = true, time.Now().Add(-ran) })
		r := newRigWith(t, m, Device{ID: "d1"}, ClientOptions{Now: deviceNow})

		// running is how long the device says the turn has run, the way the UI measures it.
		running := func() time.Duration {
			v, ok := r.c.Thread("w1")
			if !ok || !v.InTurn {
				t.Fatalf("skew %v: the device doesn't have the running thread", skew)
			}
			return r.c.Now().Sub(v.TurnStarted)
		}
		started := m.threads["w1"].TurnStarted // with the machine's monotonic clock
		check := func(when string) {
			t.Helper()
			// The estimate is behind by at most how long a message took to arrive.
			got := running()
			if want := time.Since(started); got > want || got < want-time.Second {
				t.Errorf("skew %v, %s: the turn has run %v, want %v", skew, when, got, want)
			}
		}
		check("connected")
		if naive := deviceNow().Sub(started); naive > ran+skew+time.Second || naive < ran+skew-time.Second {
			t.Fatalf("skew %v: the device's own clock says %v; the test's skew doesn't hold", skew, naive)
		}

		// The device sleeps 30 minutes with its session up, and its clock doesn't count them: the next
		// sync puts the time right.
		off.Add(int64(-30 * time.Minute))
		m.update("w1", func(v *core.ThreadView) { v.Title = "after the sleep" })
		r.wait("the sync after the sleep", func() bool { v, _ := r.c.Thread("w1"); return v.Title == "after the sleep" })
		check("after sleeping")

		// The device's clock jumps 45 minutes ahead (set by hand, or corrected), and it reconnects.
		off.Add(int64(45 * time.Minute))
		r.disconnect()
		r.connect()
		check("reconnected")
	}
}
