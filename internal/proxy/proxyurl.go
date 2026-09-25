package proxy

// T24: the tls-client transport (hhapi [api].transport = "tls-client")
// consumes full proxy URLs (tls-client SetProxy), so the [proxy.*] config
// table needs a URL projection with properly escaped credentials.

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"

	"hhauto/internal/config"
)

// URL projects one [proxy.*] entry onto the URL form the tls-client
// transport accepts (tls-client SetProxy): socks5://, http:// or https://
// with credentials in the userinfo. Credential resolution is shared with the
// dialers (PassEnv wins over the discouraged inline Pass); url.UserPassword
// percent-escapes the userinfo so passwords with reserved characters
// survive.
func URL(p config.Proxy) (string, error) {
	if p.Host == "" {
		return "", errors.New("proxy: empty host")
	}
	if p.Port <= 0 || p.Port > 65535 {
		return "", fmt.Errorf("proxy: port %d out of range", p.Port)
	}
	switch p.Type {
	case "socks5", "http", "https":
	default:
		return "", fmt.Errorf("proxy: unknown type %q (want socks5, http or https)", p.Type)
	}
	user, pass := credentials(p)
	u := &url.URL{
		Scheme: p.Type,
		Host:   net.JoinHostPort(p.Host, strconv.Itoa(p.Port)),
	}
	switch {
	case user != "" && pass != "":
		u.User = url.UserPassword(user, pass)
	case user != "":
		u.User = url.User(user)
	}
	return u.String(), nil
}
