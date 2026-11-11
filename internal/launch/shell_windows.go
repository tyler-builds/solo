package launch

import (
	"errors"
	"runtime"
	"strings"
	"syscall"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// shellOpen opens path in its default app as File Explorer does, without starting a program of
// Solo's own. The shell may use COM, so it runs on a thread of its own with COM started.
func shellOpen(path string) error {
	verb, err := windows.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED|windows.COINIT_DISABLE_OLE1DDE); err == nil || errors.Is(err, syscall.Errno(1)) { // S_OK or S_FALSE
			defer windows.CoUninitialize()
		}
		done <- windows.ShellExecute(0, verb, file, nil, nil, windows.SW_SHOWNORMAL)
	}()
	return <-done
}

const explorerPolicies = `Software\Microsoft\Windows\CurrentVersion\Policies\Explorer`

// policyBlocks reports whether the group policy "Don't run specified Windows applications" lists exe
// (a file name, such as "powershell.exe"), or "Run only specified Windows applications" leaves it out,
// for this user or this machine. It reads the registry; it starts nothing.
func policyBlocks(exe string) bool {
	for _, root := range []registry.Key{registry.CURRENT_USER, registry.LOCAL_MACHINE} {
		k, err := registry.OpenKey(root, explorerPolicies, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		disallow, _, _ := k.GetIntegerValue("DisallowRun")
		restrict, _, _ := k.GetIntegerValue("RestrictRun")
		k.Close()
		if disallow == 1 && policyLists(root, `\DisallowRun`, exe) {
			return true
		}
		if restrict == 1 && !policyLists(root, `\RestrictRun`, exe) {
			return true
		}
	}
	return false
}

// policyLists reports whether the policy list under explorerPolicies+sub names exe.
func policyLists(root registry.Key, sub, exe string) bool {
	k, err := registry.OpenKey(root, explorerPolicies+sub, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	names, _ := k.ReadValueNames(0)
	for _, n := range names {
		if v, _, err := k.GetStringValue(n); err == nil && strings.EqualFold(strings.TrimSpace(v), exe) {
			return true
		}
	}
	return false
}
