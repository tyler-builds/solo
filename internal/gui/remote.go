//go:build !js

package gui

import (
	"image"
	"image/color"
	"strings"
	"time"

	"github.com/tyler-builds/jamb/icons"
	"github.com/tyler-builds/jamb/ui"
	"rsc.io/qr"

	"github.com/tyler-builds/solo/internal/api"
)

// qrImage draws text as a QR code, each module scale pixels wide, with the quiet margin scanners need.
func qrImage(text string, scale int) (*ui.ImageSource, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return nil, err
	}
	const margin = 4
	n := code.Size + 2*margin
	img := image.NewGray(image.Rect(0, 0, n*scale, n*scale))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if !code.Black(x, y) {
				continue
			}
			for dy := 0; dy < scale; dy++ {
				for dx := 0; dx < scale; dx++ {
					img.SetGray((x+margin)*scale+dx, (y+margin)*scale+dy, color.Gray{})
				}
			}
		}
	}
	return ui.ImageFromImage(img), nil
}

// remotePage is Settings' page for using Solo from other devices.
func (a *App) remotePage(ctx *ui.Context) ui.Element {
	rm := a.Remote
	if rm == nil {
		return nil
	}
	t := ctx.Theme()
	st := a.remoteState.Get()
	fail := func(what string, err error) {
		if err != nil {
			a.notify(ui.ToastError, what, err.Error())
		}
	}

	// The relay: where it is, and whether this machine reaches it.
	address := ui.Local(ctx, "remote-relay", st.Relay)
	status, tone := "Off", t.TextSubtle
	switch st.Connection {
	case api.RemoteOnline:
		status, tone = "Connected", t.Success
	case api.RemoteConnecting:
		status, tone = "Connecting…", t.TextMuted
		if st.Problem != "" {
			status = st.Problem + " Trying again."
		}
	case api.RemoteRefused:
		status, tone = st.Problem, t.Danger
	}
	if st.Relay == "" {
		status = "Not set up"
	}
	save := func() {
		v := strings.TrimSpace(address.Peek())
		go func() { fail("Couldn't set the relay", rm.SetRelay(v)) }()
	}
	relayRows := []ui.Element{
		settingRow(ctx, "Remote access", status,
			ui.Switch("Remote access", ui.NewSignal(st.Enabled)).Disabled(st.Relay == "").Key("remote-on").
				OnChange(func(on bool) { go func() { fail("Couldn't change remote access", rm.SetEnabled(on)) }() })),
		settingBlock(ctx, "Relay address", "Where the relay and the web app are served from. Open it on a phone or another computer to use Solo there.",
			ui.Div().Row().Gap(space2).Align(ui.Center).
				Child(ui.TextInput(address).Placeholder("https://solo-relay.your-account.workers.dev").Label("Relay address").
					Small().Grow(1).Key("remote-relay").OnSubmit(func(string) { save() })).
				Child(ui.Button("Save").Secondary().Small().Disabled(strings.TrimSpace(address.Get()) == st.Relay).OnClick(save))),
	}
	if st.SecretsInFiles {
		relayRows = append(relayRows, ui.Div().Row().Gap(space2).Align(ui.Center).PaddingX(space4).PaddingY(space3).
			Child(ui.Icon(icons.TriangleAlert).Size(14).Color(t.Warning)).
			Child(caption(ctx, "This system has no keyring, so this machine's remote access key is kept in a file only you can read.")))
	}
	_ = tone
	relay := settingsSection(ctx, "Relay", "Devices reach this machine through a relay in your own Cloudflare account. It carries only encrypted traffic: it can't read or change what passes.", nil, relayRows...)

	// Devices.
	var rows []ui.Element
	for _, d := range st.Devices {
		id, name := d.ID, d.Name
		seen := "never connected"
		switch {
		case d.Online:
			seen = "connected now"
		case !d.LastSeen.IsZero():
			seen = "last here " + doneAgo(d.LastSeen)
		}
		icon, shade := icons.Smartphone, t.TextSubtle
		if d.Online {
			shade = t.Success
		}
		access := ui.NewSignal(d.ReadOnly)
		rows = append(rows, ui.Div().Row().Align(ui.Center).Gap(space3).PaddingX(space4).PaddingY(space3).Key("remote-device-"+id).
			Child(ui.Icon(icon).Size(16).Color(shade)).
			Child(ui.Div().Column().Gap(2).Grow(1).MinWidth(ui.Px(0)).
				Child(ui.Text(name).FontSize(13).Medium().TextColor(t.Text).Truncate()).
				Child(caption(ctx, "Added "+doneAgo(d.Added)+" · "+seen).Truncate())).
			Child(ui.Select(access, ui.Option[bool]{Value: false, Label: "Full access"}, ui.Option[bool]{Value: true, Label: "Can only look"}).
				Width(ui.Px(140)).Label("Access for "+name).
				OnChange(func(ro bool) { go func() { fail("Couldn't change its access", rm.SetReadOnly(id, ro)) }() })).
			Child(ui.Button("Remove").Secondary().Small().Tooltip("It can't reach this machine again without pairing").
				OnClick(func() {
					a.confirm.Set(&confirmation{title: "Remove “" + name + "”?",
						body:   "It is cut off now, and has to be paired again to come back.",
						action: "Remove", run: func() { go func() { fail("Couldn't remove it", rm.Revoke(id)) }() }})
				})))
	}
	if len(rows) == 0 {
		rows = append(rows, ui.Div().PaddingX(space4).PaddingY(space3).
			Child(caption(ctx, "No devices yet. Add one to use Solo from a phone or another computer.")))
	}
	canPair := st.Connection == api.RemoteOnline
	add := ui.Button("Add a device").Icon(icons.Plus).Secondary().Small().Disabled(!canPair || st.Pairing != nil).
		OnClick(func() {
			go func() {
				_, err := rm.StartPairing()
				fail("Couldn't start pairing", err)
			}()
		})
	if !canPair {
		add = add.Tooltip("This machine has to be connected to its relay first")
	}
	devices := settingsSection(ctx, "Devices", "Each device is paired here, at this machine, and can be removed at any time.", add, rows...)

	return settingsPage(ctx, "Remote", "Use Solo on this machine from a phone or another computer.",
		relay, a.pairingCard(ctx, st), devices)
}

// pairingCard is the pairing in progress: the code to scan, and the box for the digits the device
// shows.
func (a *App) pairingCard(ctx *ui.Context, st api.RemoteState) ui.Element {
	p := st.Pairing
	if p == nil {
		return nil
	}
	rm := a.Remote
	t := ctx.Theme()
	digits := ui.Local(ctx, "remote-digits", "")
	readOnly := ui.Local(ctx, "remote-readonly", false)
	showLink := ui.Local(ctx, "remote-showlink", false)
	// The code is drawn once for each link.
	shown := ui.Local(ctx, "remote-qr", qrShown{})
	if shown.Peek().link != p.Link {
		src, _ := qrImage(p.Link, 4)
		shown.Set(qrShown{link: p.Link, src: src})
	}
	left := time.Until(p.Expires)
	// The time left is counted down a minute at a time: one timer, started again when it fires.
	if !a.remoteTicking {
		a.remoteTicking = true
		ctx.After(20*time.Second, func() {
			a.remoteTicking = false
			a.remoteTick.Update(func(n int) int { return n + 1 })
		})
	}
	a.remoteTick.Get()
	mins := "less than a minute"
	if left > time.Minute {
		mins = strings.TrimSuffix(left.Round(time.Minute).String(), "0s")
	}
	approve := func() {
		d, ro := digits.Peek(), readOnly.Peek()
		go func() {
			dev, err := rm.Approve(d, ro)
			if err != nil {
				a.notify(ui.ToastError, "Not paired", err.Error())
				return
			}
			a.onUI(func() { digits.Set("") })
			a.notify(ui.ToastSuccess, "Paired", "“"+dev.Name+"” can now use Solo on this machine.")
		}()
	}
	var waiting ui.Element = caption(ctx, "Waiting for a device to scan the code…")
	if n := len(st.Requests); n > 0 {
		names := make([]string, 0, n)
		for _, q := range st.Requests {
			names = append(names, "“"+q.Name+"”")
		}
		verb := " is asking to pair."
		if n > 1 {
			verb = " are asking to pair. Type the digits shown on yours."
		}
		waiting = ui.Div().Row().Gap(space2).Align(ui.Center).
			Child(ui.Icon(icons.Smartphone).Size(14).Color(t.Text)).
			Child(ui.Text(strings.Join(names, ", ") + verb).FontSize(13).TextColor(t.Text))
	}
	var code ui.Element = caption(ctx, "The code couldn't be drawn. Use the link instead.")
	if src := shown.Get().src; src != nil {
		code = ui.Div().Padding(space2).Radius(radiusM).Background(ui.RGB(255, 255, 255)).Shrink(0).
			Child(ui.Image(src).Size(ui.Px(200), ui.Px(200)).Fit(ui.FitContain).Alt("Pairing code"))
	}
	steps := ui.Div().Column().Gap(space3).Grow(1).MinWidth(ui.Px(0)).
		Child(ui.Div().Column().Gap(space1).
			Child(subhead("1. Scan this with the device's camera")).
			Child(caption(ctx, "It opens Solo's web app there. The code works once, for "+mins+"."))).
		Child(ui.Div().Column().Gap(space1).
			Child(subhead("2. Type the six digits the device shows")).
			Child(caption(ctx, "They prove the device in your hand is the one asking."))).
		Child(waiting).
		Child(ui.Div().Row().Gap(space2).Align(ui.Center).
			Child(ui.TextInput(digits).Placeholder("000 000").Label("Digits shown on the device").Width(ui.Px(120)).
				Key("remote-digits").OnSubmit(func(string) { approve() })).
			Child(ui.Button("Pair").Primary().Disabled(len(st.Requests) == 0 || countDigits(digits.Get()) != 6).OnClick(approve)).
			Child(ui.Button("Cancel").Ghost().OnClick(func() { go rm.CancelPairing() }))).
		Child(ui.Checkbox("It can only look, not change anything", readOnly))
	link := ui.Collapsible("Can't scan? Use a link instead", showLink,
		ui.Div().Column().Gap(space2).PaddingTop(space2).
			Child(caption(ctx, "Open this on the device. Anyone who has the link can ask to pair until it expires, so don't send it where others can read it.")).
			Child(ui.Div().Row().Gap(space2).Align(ui.Center).
				Child(ui.Text(p.Link).FontSize(11).TextColor(t.TextMuted).Truncate().Grow(1)).
				Child(ui.Button("Copy").Icon(icons.Copy).Secondary().Small().OnClick(func() { a.copyText("Pairing link", p.Link) }))))
	body := ui.Div().Column().Gap(space4).Padding(space4).
		Child(ui.Div().Row().Wrap().Gap(space5).Align(ui.Start).Child(code).Child(steps)).
		Child(link)
	return settingsSection(ctx, "Add a device", "", nil, body)
}

type qrShown struct {
	link string
	src  *ui.ImageSource
}

func countDigits(s string) int {
	n := 0
	for _, c := range s {
		if c >= '0' && c <= '9' {
			n++
		}
	}
	return n
}
