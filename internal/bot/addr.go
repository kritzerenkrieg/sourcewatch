package bot

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/kritzerenkrieg/sourcewatch/internal/poller"
)

const defaultPort = "27015"

// lookupIP resolves a host. It is a variable so tests do not need DNS.
var lookupIP = net.LookupIP

// normalizeAddr turns user input into "host:port". Anyone with Manage Server can name
// an address the bot will send UDP packets to, so a host that resolves to a private,
// loopback, link-local or other non-public address is refused, as is a host with no IPv4
// address. That keeps the bot from probing the network it runs in. The poller checks the
// address again on every poll (poller.PublicDialer), in case the host name changes.
func normalizeAddr(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("give an address like `1.2.3.4:27015`")
	}
	if !strings.Contains(raw, ":") {
		raw += ":" + defaultPort
	}
	host, portStr, err := net.SplitHostPort(raw)
	if err != nil || host == "" {
		return "", fmt.Errorf("`%s` is not a valid address. Use `host:port`", clip(raw))
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("`%s` is not a valid port", clip(portStr))
	}
	if len(host) > 253 || !validHost(host) {
		return "", fmt.Errorf("`%s` is not a valid host name or IPv4 address", clip(host))
	}
	ips, err := lookupIP(host)
	if err != nil || len(ips) == 0 {
		return "", fmt.Errorf("I could not find `%s`", clip(host))
	}
	switch _, err := poller.PublicIPv4(ips); {
	case errors.Is(err, poller.ErrNoIPv4):
		return "", fmt.Errorf("`%s` has no IPv4 address, and only IPv4 game servers can be watched", clip(host))
	case err != nil:
		return "", fmt.Errorf("`%s` is not a public address", clip(host))
	}
	// The port is written as a number, so 027015 and 27015 are the same game server.
	return strings.ToLower(host) + ":" + strconv.Itoa(port), nil
}

// validHost allows letters, digits, dots and hyphens only.
func validHost(h string) bool {
	for _, r := range h {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
		default:
			return false
		}
	}
	return true
}

// clip shortens user input before it is shown back.
func clip(s string) string {
	if r := []rune(s); len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return s
}
