//go:build js

package gui

import "github.com/tyler-builds/jamb/ui"

// remotePage is the desktop's: a page is a device, and has no devices of its own to manage.
func (a *App) remotePage(*ui.Context) ui.Element { return nil }
