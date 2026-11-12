package webclient

import (
	"strings"

	"github.com/tyler-builds/jamb/access"
	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"

	"github.com/tyler-builds/solo/internal/gui"
	"github.com/tyler-builds/solo/internal/remote/device"
)

// state is what the shell's screens read.
type state struct {
	screen   *ui.Signal[screen]
	link     *ui.Signal[link]
	shown    *ui.Signal[string] // the machine whose UI is up, by ID
	machines *ui.Signal[[]device.Machine]
	name     *ui.Signal[string] // what the user calls this device, on the pairing screen
}

func newState() *state {
	return &state{screen: ui.NewSignal(screen{}), link: ui.NewSignal(link{}), shown: ui.NewSignal(""),
		machines: ui.NewSignal([]device.Machine(nil)), name: ui.NewSignal("")}
}

// Root renders the page.
func (s *Shell) Root(ctx *ui.Context) ui.Element {
	s.mu.Lock()
	if s.post == nil {
		s.post = ctx.Post
		s.state.machines.Set(append([]device.Machine(nil), s.machines...))
		if s.state.name.Peek() == "" {
			s.state.name.Set(s.p.DeviceName)
		}
	}
	cur := s.current
	s.mu.Unlock()
	if s.p.Touch {
		ctx.SetZoom(1.2) // as the app's own UI does for fingers
	}
	sc := s.state.screen.Get()
	s.tell(sc, cur)
	switch sc.kind {
	case screenConfirm:
		return s.confirm(ctx)
	case screenPairing:
		return s.digits(ctx, sc)
	case screenMachines:
		return s.machineList(ctx)
	case screenMain:
		if cur == nil {
			return s.welcome(ctx, sc)
		}
		s.state.shown.Get()
		l := s.state.link.Get()
		if !cur.synced && l.state != linkReady {
			return s.waiting(ctx, cur, l)
		}
		return s.main(ctx, cur, l)
	}
	return s.welcome(ctx, sc)
}

// page is a screen outside the app: a column in the middle, narrow enough for a phone.
func page(ctx *ui.Context, children ...ui.Element) ui.Element {
	t := ctx.Theme()
	col := ui.Div().Column().Gap(16).Width(ui.Full).MaxWidth(ui.Px(380)).Padding(24)
	for _, c := range children {
		if c != nil {
			col = col.Child(c)
		}
	}
	return ui.Div().Column().Size(ui.Full, ui.Full).Align(ui.Center).Justify(ui.Center).Background(t.Background).
		ScrollY().Child(col)
}

func heading(ctx *ui.Context, text string) ui.Element {
	return ui.Text(text).FontSize(22).FontWeight(ui.FontSemibold).TextColor(ctx.Theme().Text)
}

func para(ctx *ui.Context, text string) ui.Element {
	return ui.Text(text).FontSize(14).TextColor(ctx.Theme().TextMuted)
}

// problem is what went wrong, said once, above whatever the screen offers next.
func problem(ctx *ui.Context, text string) ui.Element {
	if text == "" {
		return nil
	}
	t := ctx.Theme()
	return ui.Div().Row().Gap(10).Align(ui.Start).Padding(12).Radius(10).Border(1, t.Border).Background(t.Surface).
		Role(access.RoleGroup).Label("Problem").
		Child(ui.Div().PaddingTop(2).Child(ui.Icon(icons.TriangleAlert).Size(15).Color(t.Warning))).
		Child(ui.Text(text).FontSize(13).TextColor(t.Text).Grow(1))
}

// welcome is the page before this device is paired with anything.
func (s *Shell) welcome(ctx *ui.Context, sc screen) ui.Element {
	t := ctx.Theme()
	step := func(n, text string) ui.Element {
		return ui.Div().Row().Gap(12).Align(ui.Start).
			Child(ui.Div().Row().Align(ui.Center).Justify(ui.Center).Width(ui.Px(22)).Height(ui.Px(22)).Radius(11).Shrink(0).
				Background(t.Surface).Border(1, t.Border).
				Child(ui.Text(n).FontSize(12).Medium().TextColor(t.TextMuted))).
			Child(ui.Text(text).FontSize(14).TextColor(t.Text).Grow(1))
	}
	return page(ctx,
		gui.BrandMark(40),
		heading(ctx, "Use Solo from this device"),
		problem(ctx, sc.err),
		para(ctx, "This device isn't paired with a machine yet. Pairing is done at the machine, so only someone who is there can add a device."),
		ui.Div().Column().Gap(12).
			Child(step("1", "On the computer that runs Solo, open Settings, then Remote.")).
			Child(step("2", "Choose Add a device.")).
			Child(step("3", "Scan the code it shows with this device's camera.")))
}

// confirm asks before pairing: a link was opened, and the person should mean it.
func (s *Shell) confirm(ctx *ui.Context) ui.Element {
	go_ := func() { s.pair(s.state.name.Peek()) }
	return page(ctx,
		gui.BrandMark(40),
		heading(ctx, "Pair this device?"),
		para(ctx, "It will be able to use Solo on the machine that showed this code, once you approve it there."),
		ui.Div().Column().Gap(6).
			Child(ui.Text("What the machine will call this device").FontSize(12).TextColor(ctx.Theme().TextMuted)).
			Child(ui.TextInput(s.state.name).Label("This device's name").Placeholder("My phone").Width(ui.Full).
				Key("device-name").OnSubmit(func(string) { go_() })),
		ui.Div().Row().Gap(8).
			Child(ui.Button("Pair").Primary().Key("pair").OnClick(go_)).
			Child(ui.Button("Not now").Ghost().Key("pair-cancel").OnClick(s.cancelPairing)))
}

// digits shows the six digits to type at the machine, while the person there decides.
func (s *Shell) digits(ctx *ui.Context, sc screen) ui.Element {
	t := ctx.Theme()
	if sc.digits == "" {
		return page(ctx, gui.BrandMark(40), heading(ctx, "Reaching the machine…"),
			ui.Div().Row().Child(ui.ProgressRing().Size(16).Label("Working")),
			ui.Div().Row().Child(ui.Button("Cancel").Ghost().Key("pair-cancel").OnClick(s.cancelPairing)))
	}
	shown := sc.digits
	if len(shown) == 6 {
		shown = shown[:3] + " " + shown[3:]
	}
	return page(ctx,
		gui.BrandMark(40),
		heading(ctx, "Type these digits at the machine"),
		ui.Div().Row().Justify(ui.Center).PaddingY(18).Radius(12).Border(1, t.Border).Background(t.Surface).
			Role(access.RoleStaticText).Label("Digits: "+strings.Join(strings.Split(sc.digits, ""), " ")).
			Child(ui.Text(shown).FontSize(38).FontWeight(ui.FontSemibold).TextColor(t.Text).Key("digits")),
		para(ctx, "Solo on the machine is waiting for them under Settings, Remote. They show it that this device, and not another, is the one asking."),
		ui.Div().Row().Gap(10).Align(ui.Center).
			Child(ui.ProgressRing().Size(16).Label("Working")).
			Child(ui.Text("Waiting for approval…").FontSize(13).TextColor(t.TextMuted)),
		ui.Div().Row().Child(ui.Button("Cancel").Ghost().Key("pair-cancel").OnClick(s.cancelPairing)))
}

// waiting is the page while a machine is being reached for the first time since the page opened.
func (s *Shell) waiting(ctx *ui.Context, cur *session, l link) ui.Element {
	t := ctx.Theme()
	title, busy := "Connecting to "+cur.m.Name+"…", true
	switch l.state {
	case linkOffline:
		title = cur.m.Name + " is offline"
	case linkLost:
		title = "Can't reach " + cur.m.Name
	case linkEnded:
		title, busy = "Not paired any more", false
	}
	var foot ui.Element
	if busy {
		note := "Trying again…"
		if l.state == linkConnecting {
			note = "This takes a moment."
		}
		foot = ui.Div().Row().Gap(10).Align(ui.Center).Child(ui.ProgressRing().Size(16).Label("Working")).
			Child(ui.Text(note).FontSize(13).TextColor(t.TextMuted))
	}
	buttons := ui.Div().Row().Gap(8).Wrap()
	if l.state == linkEnded {
		buttons = buttons.Child(ui.Button("Forget this machine").Secondary().Key("forget").OnClick(func() { s.forget(cur.m.ID) }))
	} else if l.state != linkConnecting {
		buttons = buttons.Child(ui.Button("Try now").Secondary().Key("retry").OnClick(s.Resume))
	}
	buttons = buttons.Child(ui.Button("Machines").Ghost().Key("machines").
		OnClick(func() { s.state.screen.Set(screen{kind: screenMachines}) }))
	return page(ctx, gui.BrandMark(40), heading(ctx, title), problem(ctx, l.problem), foot, buttons)
}

// main is a machine's UI, under a line that says so when the connection is down.
func (s *Shell) main(ctx *ui.Context, cur *session, l link) ui.Element {
	t := ctx.Theme()
	app := ui.Div().Column().Grow(1).MinHeight(ui.Px(0)).Width(ui.Full).Child(cur.app.Root(ctx))
	if l.state == linkReady {
		return ui.Div().Column().Size(ui.Full, ui.Full).Child(app)
	}
	text := "Reconnecting to " + cur.m.Name + "…"
	switch l.state {
	case linkOffline:
		text = cur.m.Name + " is offline. What you see may be out of date."
	case linkLost:
		text = "Connection lost. Trying again…"
	case linkEnded:
		text = l.problem
	}
	bar := ui.Div().Row().Align(ui.Center).Gap(8).PaddingX(12).MinHeight(ui.Px(34)).PaddingY(6).Shrink(0).
		Background(t.Surface).BorderEdges(ui.Edges{Bottom: 1}, t.Border).Role(access.RoleStatusBar).Label("Connection").Key("link-bar").
		Child(ui.Icon(icons.TriangleAlert).Size(14).Color(t.Warning)).
		Child(ui.Text(text).FontSize(12).TextColor(t.Text).Grow(1))
	if l.state == linkEnded {
		bar = bar.Child(ui.Button("Machines").Secondary().Small().OnClick(func() { s.state.screen.Set(screen{kind: screenMachines}) }))
	} else {
		bar = bar.Child(ui.Button("Try now").Secondary().Small().Key("retry").OnClick(s.Resume))
	}
	return ui.Div().Column().Size(ui.Full, ui.Full).Child(bar).Child(app)
}

// machineList is the machines this device is paired with: open one, or forget it.
func (s *Shell) machineList(ctx *ui.Context) ui.Element {
	t := ctx.Theme()
	s.mu.Lock()
	cur := s.current
	s.mu.Unlock()
	l := s.state.link.Get()
	list := ui.Div().Column().Radius(10).Border(1, t.Border).Background(t.Surface)
	for i, m := range s.state.machines.Get() {
		m := m
		note := "Paired"
		if m.ReadOnly {
			note = "Paired · can only look"
		}
		if cur != nil && cur.m.ID == m.ID {
			switch l.state {
			case linkReady:
				note = "Connected"
			case linkOffline:
				note = "Offline"
			case linkEnded:
				note = "Not paired any more"
			default:
				note = "Connecting…"
			}
		}
		row := ui.Div().Row().Align(ui.Center).Gap(12).PaddingX(14).PaddingY(12).Key("machine-" + m.ID).
			Child(ui.Icon(icons.Laptop).Size(18).Color(t.TextMuted)).
			Child(ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
				Child(ui.Text(m.Name).FontSize(14).Medium().TextColor(t.Text).Truncate()).
				Child(ui.Text(note).FontSize(12).TextColor(t.TextMuted))).
			Child(ui.Button("Open").Secondary().Small().Key("open-" + m.ID).OnClick(func() { s.open(m) })).
			Child(ui.IconButton(icons.Trash).Ghost().Small().Label("Forget " + m.Name).Tooltip("Forget this machine on this device").
				Key("forget-" + m.ID).OnClick(func() { s.forget(m.ID) }))
		if i > 0 {
			row = row.BorderEdges(ui.Edges{Top: 1}, t.Border)
		}
		list = list.Child(row)
	}
	return page(ctx,
		gui.BrandMark(40),
		heading(ctx, "Machines"),
		list,
		para(ctx, "To add another, open Settings, then Remote, on that machine and scan its code with this device. Forgetting a machine deletes its keys from this device; remove this device at the machine too."))
}

// tell reports what is on screen to Platform.Shown, when it changed.
func (s *Shell) tell(sc screen, cur *session) {
	if s.p.Shown == nil {
		return
	}
	name := [...]string{screenWelcome: "welcome", screenConfirm: "confirm", screenPairing: "pairing",
		screenMachines: "machines", screenMain: "main"}[sc.kind]
	l := ""
	if cur != nil {
		l = [...]string{linkConnecting: "connecting", linkReady: "ready", linkOffline: "offline", linkLost: "lost",
			linkEnded: "ended"}[s.state.link.Get().state]
	}
	now := [3]string{name, sc.digits, l}
	if now != s.told {
		s.told = now
		s.p.Shown(name, sc.digits, l)
	}
}
