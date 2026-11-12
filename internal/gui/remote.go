//go:build !js

package gui

import (
	"context"
	"image"
	"image/color"
	"strconv"
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
	token := ui.Local(ctx, "remote-token", "")
	manual := ui.Local(ctx, "remote-manual", false)
	status := "Off"
	switch st.Connection {
	case api.RemoteOnline:
		status = "Connected"
	case api.RemoteConnecting:
		status = "Connecting…"
		if st.Problem != "" {
			status = st.Problem + " Trying again."
		}
	case api.RemoteRefused:
		status = st.Problem
	}
	if st.Cloud.Paused {
		status = "Paused"
	}
	long := func(what string, f func(context.Context) error) {
		go func() {
			c, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			fail(what, f(c))
		}()
	}
	saveAddress := func() {
		v := strings.TrimSpace(address.Peek())
		go func() { fail("Couldn't set the relay", rm.SetRelay(v)) }()
	}
	saveToken := func() {
		v := token.Peek()
		long("Couldn't use that token", func(c context.Context) error {
			err := rm.SetCloudToken(c, v)
			if err == nil {
				a.onUI(func() { token.Set("") })
			}
			return err
		})
	}
	deploy := func() { long("Couldn't set up the relay", rm.DeployRelay) }
	working := st.Cloud.Busy != ""
	note := func(icon *icons.Icon, color ui.Color, text string) ui.Element {
		return ui.Div().Row().Gap(space2).Align(ui.Start).PaddingX(space4).PaddingY(space3).
			Child(ui.Div().PaddingTop(1).Child(ui.Icon(icon).Size(14).Color(color))).
			Child(caption(ctx, text).Grow(1))
	}
	var keyring ui.Element
	if st.SecretsInFiles {
		keyring = note(icons.TriangleAlert, t.Warning, "This system has no keyring, so this machine's remote access key and the Cloudflare token are kept in files only you can read.")
	}
	addressRow := settingBlock(ctx, "Relay address", "Where a relay and its web app are already served from.",
		ui.Div().Row().Gap(space2).Align(ui.Center).
			Child(ui.TextInput(address).Placeholder("https://solo-relay-….your-account.workers.dev").Label("Relay address").
				Small().Grow(1).Key("remote-relay").OnSubmit(func(string) { saveAddress() })).
			Child(ui.Button("Save").Secondary().Small().Disabled(strings.TrimSpace(address.Get()) == st.Relay).OnClick(saveAddress)))
	// The Cloudflare token: asked for once, kept in the keyring, never shown again.
	var tokenRow ui.Element
	if st.Cloud.HasToken {
		where := "Kept on this machine."
		if st.Cloud.Account != "" {
			where = "For the account “" + st.Cloud.Account + "”. Kept on this machine."
		}
		tokenRow = settingRow(ctx, "Cloudflare API token", where,
			ui.Button("Remove").Secondary().Small().Key("remote-token-remove").Disabled(working).
				Tooltip("The relay keeps working; changing or removing it needs a token again").
				OnClick(func() { go func() { fail("Couldn't remove the token", rm.ForgetCloudToken()) }() }))
	} else {
		tokenRow = settingBlock(ctx, "Cloudflare API token",
			"Solo puts a small relay and its web app in your own Cloudflare account; the free plan is enough. Make a token there that can edit Workers scripts and read account settings, and paste it here. It is kept in this machine's keyring and not shown again.",
			ui.Div().Column().Gap(space2).
				Child(ui.Div().Row().Gap(space2).Align(ui.Center).
					Child(ui.TextInput(token).Placeholder("Paste the token").Label("Cloudflare API token").Masked().
						Small().Grow(1).Key("remote-token").OnSubmit(func(string) { saveToken() })).
					Child(ui.Button("Save").Secondary().Small().Key("remote-token-save").
						Disabled(strings.TrimSpace(token.Get()) == "").OnClick(saveToken))).
				Child(ui.Div().Row().Child(ui.Button("Create a token at Cloudflare").Icon(icons.ExternalLink).Ghost().Small().
					OnClick(func() { _ = ctx.OpenURL("https://dash.cloudflare.com/profile/api-tokens") }))))
	}

	// The alert: the relay is not as this machine left it. It is shown whatever else the page shows.
	removeRelay := func() {
		a.confirm.Set(&confirmation{title: "Remove the relay?",
			body:   "It is deleted from your Cloudflare account, and every device is removed here. Every other machine that uses it is cut off too, until remote access is set up again.",
			action: "Remove", run: func() { long("Couldn't remove the relay", rm.RemoveRelay) }})
	}
	var alert ui.Element
	if st.Cloud.Alert != "" {
		box := ui.Div().Column().Gap(space3).PaddingX(space4).PaddingY(space3).Key("remote-alert").
			Child(ui.Div().Row().Gap(space2).Align(ui.Start).
				Child(ui.Div().PaddingTop(1).Child(ui.Icon(icons.TriangleAlert).Size(15).Color(t.Danger))).
				Child(ui.Text(st.Cloud.Alert).FontSize(13).TextColor(t.Text).Grow(1)))
		if len(st.Cloud.Changes) > 0 {
			found := ui.Div().Column().Gap(2).PaddingLeft(space5 + 3)
			for _, c := range st.Cloud.Changes {
				found = found.Child(caption(ctx, "• "+c))
			}
			box = box.Child(found)
		}
		if st.Cloud.Paused {
			box = box.Child(ui.Div().Row().Wrap().Gap(space2).
				Child(ui.Button("It was mine: resume").Secondary().Small().Key("remote-accept").
					Tooltip("Keeps the devices. Choose this only if you made the change, from another of your machines").
					OnClick(func() {
						a.confirm.Set(&confirmation{title: "Resume remote access?",
							body:   "Only if you made this change yourself, by setting up or updating Solo on another of your machines. If you aren't sure, resume with the devices removed instead, and pair them again.",
							action: "Resume", safe: true,
							run: func() {
								long("Couldn't check the relay", func(c context.Context) error { return rm.AcceptRelay(c, false) })
							}})
					})).
				Child(ui.Button("Resume, and remove every device").Secondary().Small().Key("remote-accept-clean").
					Tooltip("For when you aren't sure: devices are paired again afterwards").
					OnClick(func() {
						a.confirm.Set(&confirmation{title: "Remove every device and resume?",
							body:   "Each device has to be paired again at this machine.",
							action: "Remove and resume",
							run: func() {
								long("Couldn't check the relay", func(c context.Context) error { return rm.AcceptRelay(c, true) })
							}})
					})).
				Child(ui.Button("Remove the relay").Danger().Small().Key("remote-remove-paused").Disabled(!st.Cloud.HasToken).OnClick(removeRelay)))
		}
		alert = box
	}
	var unchecked ui.Element
	if st.Cloud.CheckProblem != "" {
		unchecked = note(icons.TriangleAlert, t.Warning, st.Cloud.CheckProblem)
	}
	var local ui.Element
	if strings.HasPrefix(st.Relay, "http://") {
		local = note(icons.TriangleAlert, t.Warning, "This relay runs on this machine without encryption to it, for trying things out. Any program here that takes its port could stand in for it, so don't leave remote access on with it.")
	}

	var relay ui.Element
	if st.Relay == "" {
		// Not set up yet: the token, then one button.
		var action ui.Element
		switch {
		case working:
			action = ui.Div().Row().Gap(space2).Align(ui.Center).PaddingX(space4).PaddingY(space3).
				Child(ui.ProgressRing().Size(16).Label("Working")).
				Child(ui.Text(st.Cloud.Busy).FontSize(13).TextColor(t.Text))
		case !st.Cloud.CanDeploy:
			action = note(icons.TriangleAlert, t.Warning, "This build of Solo doesn't include the relay and the web app, so it can't set one up. It can use a relay that already runs: enter its address below.")
		default:
			action = settingRow(ctx, "Set up remote access", "Puts the relay and the web app in your Cloudflare account and connects this machine to it. About a minute.",
				ui.Button("Set up").Primary().Small().Key("remote-deploy").Disabled(!st.Cloud.HasToken).OnClick(deploy))
		}
		other := ui.Collapsible("Use a relay that already runs", manual, addressRow)
		if st.Cloud.Paused {
			action = nil // the alert's choices come first
		}
		relay = settingsSection(ctx, "Set up", "Devices reach this machine through a relay in your own Cloudflare account. It carries only encrypted traffic: it can't read or change what passes.", nil,
			alert, tokenRow, action, keyring, ui.Div().PaddingX(space2).PaddingY(space1).Child(other))
	} else {
		var manage ui.Element
		if working {
			manage = ui.Div().Row().Gap(space2).Align(ui.Center).PaddingX(space4).PaddingY(space3).
				Child(ui.ProgressRing().Size(16).Label("Working")).
				Child(ui.Text(st.Cloud.Busy).FontSize(13).TextColor(t.Text))
		} else if st.Cloud.Deployed && !st.Cloud.Paused {
			manage = settingRow(ctx, "The relay in Cloudflare", "Update it after Solo is updated, or remove it to cut every device off.",
				ui.Div().Row().Gap(space2).
					Child(ui.Button("Update").Secondary().Small().Key("remote-update").Disabled(!st.Cloud.HasToken || !st.Cloud.CanDeploy).
						Tooltip("Uploads this version's relay and web app if they are newer").OnClick(deploy)).
					Child(ui.Button("Remove").Secondary().Small().Key("remote-remove").Disabled(!st.Cloud.HasToken).OnClick(removeRelay)))
		}
		buttons := ui.Div().Row().Gap(space2).
			Child(ui.Button("Copy").Icon(icons.Copy).Secondary().Small().OnClick(func() { a.copyText("Relay address", st.Relay) }))
		if !st.Cloud.Deployed {
			// A relay entered by its address is left by forgetting it; one this machine set up is
			// removed, or kept.
			buttons = buttons.Child(ui.Button("Forget").Ghost().Small().Tooltip("Stop using this relay on this machine").
				OnClick(func() { go func() { fail("Couldn't forget the relay", rm.SetRelay("")) }() }))
		}
		where := settingRow(ctx, "Address", st.Relay, buttons)
		relay = settingsSection(ctx, "Relay", "Devices reach this machine through a relay in your own Cloudflare account. It carries only encrypted traffic: it can't read or change what passes.", nil,
			alert,
			settingRow(ctx, "Remote access", status,
				ui.Switch("Remote access", ui.NewSignal(st.Enabled)).Key("remote-on").
					OnChange(func(on bool) { go func() { fail("Couldn't change remote access", rm.SetEnabled(on)) }() })),
			where, local, tokenRow, manage, unchecked, keyring)
	}

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
	// A device that asks is not named until it is approved: its name is its own choice of words, and
	// would stand right above the box for the digits.
	var waiting ui.Element = caption(ctx, "Waiting for a device to scan the code…")
	if n := len(st.Requests); n > 0 {
		text := "A device is asking to pair. Type the digits it shows."
		if n > 1 {
			text = strconv.Itoa(n) + " devices are asking to pair. Type the digits yours shows."
		}
		waiting = ui.Div().Row().Gap(space2).Align(ui.Center).Key("remote-asking").
			Child(ui.Icon(icons.Smartphone).Size(14).Color(t.Text)).
			Child(ui.Text(text).FontSize(13).TextColor(t.Text))
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
