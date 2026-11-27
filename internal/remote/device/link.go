package device

import (
	"errors"
	"net/url"
	"strings"

	"github.com/tyler-builds/solo/internal/remote/secure"
)

// ParseLink reads a pairing link, as a machine shows it ("https://<relay>/#p=<code>"): the relay's
// address and the code. It does not check the code's expiry; the machine decides that.
func ParseLink(link string) (address string, code secure.PairingCode, err error) {
	bad := errors.New("That isn't a pairing link. Copy the link Settings › Remote shows under Add a device on the other machine.")
	u, perr := url.Parse(strings.TrimSpace(link))
	if perr != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return "", code, bad
	}
	frag, ok := strings.CutPrefix(u.Fragment, "p=")
	if !ok {
		return "", code, bad
	}
	if u.Scheme == "http" && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return "", code, errors.New("A relay that isn't on this machine must use https.")
	}
	if code, err = secure.ParsePairingCode(frag); err != nil {
		return "", code, errors.New("That pairing link isn't complete. Copy it again from the other machine.")
	}
	return u.Scheme + "://" + u.Host, code, nil
}
