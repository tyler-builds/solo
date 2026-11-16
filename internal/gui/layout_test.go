package gui

import (
	"testing"
)

// The To do list starts folded in a compact window, where the drawer's height goes to the threads, and
// open in a wide one. Once the user opens or folds it, their choice stands, also when the drawer is
// closed and opened again.
func TestTodosFoldedOnPhone(t *testing.T) {
	a, u := newPhoneUI(t, demoBackend())
	u.Click("open-drawer")
	u.Settle()
	if !u.Has("todo-fold") || u.Has("todo-t1") {
		t.Fatalf("a compact window's To do list isn't folded at first: heading %v, a todo %v", u.Has("todo-fold"), u.Has("todo-t1"))
	}
	u.Click("todo-fold")
	u.Settle()
	if !u.Has("todo-t1") {
		t.Fatal("the folded To do list didn't open")
	}
	a.drawer.Set(false)
	u.Settle()
	u.Click("open-drawer")
	u.Settle()
	if !u.Has("todo-t1") {
		t.Error("the To do list the user opened was folded again when the drawer opened again")
	}

	_, wide := newUI(t, demoBackend())
	wide.Settle()
	if !wide.Has("todo-t1") {
		t.Fatal("a wide window's To do list isn't open at first")
	}
	wide.Click("todo-fold")
	wide.Settle()
	if wide.Has("todo-t1") {
		t.Error("the To do list didn't fold")
	}
}

// A compact window shows the usage limits in one short line, each with its bar; a wide one keeps a row
// per limit.
func TestUsageMiniOnPhone(t *testing.T) {
	f := demoBackend()
	f.rate = demoLimits()
	_, u := newPhoneUI(t, f)
	u.Click("open-drawer")
	u.Settle()
	if !u.Has("usage-mini") {
		t.Fatal("no one-line usage in a compact window")
	}
	mini := u.Get("usage-mini").Bounds()
	for _, k := range []string{"usage-five_hour", "usage-seven_day", "usage-seven_day_opus"} {
		if !u.Has(k) {
			t.Errorf("no bar %s", k)
			continue
		}
		if b := u.Get(k).Bounds(); b.Y < mini.Y || b.Y+b.H > mini.Y+mini.H {
			t.Errorf("%s %v is outside the usage line %v", k, b, mini)
		}
	}
	if fh, sd := u.Get("usage-five_hour").Bounds(), u.Get("usage-seven_day_opus").Bounds(); fh.Y != sd.Y {
		t.Errorf("the limits don't share a line: 5h at %v, 7d Opus at %v", fh.Y, sd.Y)
	}

	f = demoBackend()
	f.rate = demoLimits()
	_, wide := newUI(t, f)
	wide.Settle()
	if wide.Has("usage-mini") || !wide.Has("usage-five_hour") {
		t.Errorf("a wide window's usage isn't its row per limit: mini %v, 5h row %v", wide.Has("usage-mini"), wide.Has("usage-five_hour"))
	}
}

// A page that drives another machine names it in Settings only: the UI has no control of its own for
// switching machines, in a wide window or a phone's drawer.
func TestNoMachineSwitch(t *testing.T) {
	_, wide := newUIWith(t, demoBackend(), func(a *App) { a.Machine = "studio" })
	wide.Settle()
	if wide.Has("machine") || wide.HasName("studio") {
		t.Error("a wide window has a machine switch")
	}
	_, u := newPhoneUIWith(t, demoBackend(), func(a *App) { a.Machine = "studio" })
	u.Click("open-drawer")
	u.Settle()
	if u.Has("machine") || u.HasName("Machine: studio") {
		t.Error("the drawer has a machine switch")
	}
}
