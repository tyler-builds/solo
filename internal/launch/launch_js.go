package launch

import "errors"

// A web page runs none of the user's apps: the editor, the terminal and the file manager are on the
// machine that Solo runs on, not on the one that shows the page. The calls answer with ErrNoApps, and
// Detect finds nothing.

// Available reports whether this build can open the user's apps.
const Available = false

// ErrNoApps is what every call answers in a web page.
var ErrNoApps = errors.New("a web page cannot open apps on this device")

// ErrNoEditor means no code editor was found.
var ErrNoEditor = errors.New("no code editor found: set one in Settings › General")

// Editor opens dir in the editor command.
func Editor(command, dir string) error { return ErrNoApps }

// EditFile opens path in the editor command, at line.
func EditFile(command, path string, line int) error { return ErrNoApps }

// Open opens path, a file or a folder, in the system's default app for it.
func Open(path string) error { return ErrNoApps }

// Reveal shows path in the file manager.
func Reveal(path string) error { return ErrNoApps }

// Terminal opens a terminal in dir.
func Terminal(command, dir string) error { return ErrNoApps }

// Detect returns the editor or terminal command Solo would use: none in a web page.
func Detect(kind string) string { return "" }
