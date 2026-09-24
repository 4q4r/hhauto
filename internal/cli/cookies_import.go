package cli

// Cookie import parsers (account add / onboarding). Accepted formats:
//   - header line:  "name=value; name2=value2"
//   - JSON object:  {"name": "value", ...}
//   - JSON array:   [{"name":"n","value":"v","domain":..., "path":...,
//                     "secure":true, "expires": 1735689600|null}, ...]
//   - Netscape:     cookies.txt lines (7 tab-separated fields; the
//     "#HttpOnly_" prefix is a real cookie, other '#' lines are comments)

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// parseCookiesInput detects the format and parses the payload.
func parseCookiesInput(raw string) ([]*http.Cookie, error) {
	trimmed := strings.TrimSpace(raw)
	switch {
	case trimmed == "":
		return nil, fmt.Errorf("cookies: empty input")
	case strings.HasPrefix(trimmed, "{"):
		return parseCookiesJSONMap(trimmed)
	case strings.HasPrefix(trimmed, "["):
		return parseCookiesJSONArray(trimmed)
	case strings.Contains(trimmed, "\t"):
		return parseNetscapeCookies(trimmed)
	default:
		return parseCookieHeaderLine(trimmed)
	}
}

// parseCookieHeaderLine parses "name=value; name2=value2".
func parseCookieHeaderLine(line string) ([]*http.Cookie, error) {
	header := http.Header{}
	header.Add("Cookie", line)
	req := http.Request{Header: header}
	cookies := req.Cookies()
	if len(cookies) == 0 {
		return nil, fmt.Errorf("cookies: no name=value pairs found in the input line")
	}
	for _, c := range cookies {
		if strings.TrimSpace(c.Name) == "" || strings.TrimSpace(c.Value) == "" {
			return nil, fmt.Errorf("cookies: entry %q has no value — want name=value pairs", c.Name)
		}
	}
	return cookies, nil
}

// parseCookiesJSONMap parses {"name": "value", ...}.
func parseCookiesJSONMap(doc string) ([]*http.Cookie, error) {
	var m map[string]string
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		return nil, fmt.Errorf("cookies: JSON object decode: %w", err)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("cookies: JSON object has no cookie entries")
	}
	out := make([]*http.Cookie, 0, len(m))
	for name, value := range m {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("cookies: empty cookie name in JSON object")
		}
		out = append(out, &http.Cookie{Name: name, Value: value})
	}
	return out, nil
}

// jsonCookie is one entry of a browser-style JSON export.
type jsonCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Secure   bool    `json:"secure"`
	HTTPOnly bool    `json:"httpOnly"`
	Expires  *int64  `json:"expires"`
	MaxAge   float64 `json:"maxAge"`
}

// parseCookiesJSONArray parses [{"name": ..., "value": ...}, ...].
func parseCookiesJSONArray(doc string) ([]*http.Cookie, error) {
	var entries []jsonCookie
	if err := json.Unmarshal([]byte(doc), &entries); err != nil {
		return nil, fmt.Errorf("cookies: JSON array decode: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("cookies: JSON array has no cookie entries")
	}
	out := make([]*http.Cookie, 0, len(entries))
	for _, e := range entries {
		if e.Name == "" {
			return nil, fmt.Errorf("cookies: JSON cookie entry without a name")
		}
		c := &http.Cookie{Name: e.Name, Value: e.Value, Domain: e.Domain, Path: e.Path, Secure: e.Secure, HttpOnly: e.HTTPOnly}
		switch {
		case e.Expires != nil && *e.Expires > 0:
			c.Expires = time.Unix(*e.Expires, 0)
		case e.MaxAge > 0:
			c.MaxAge = int(e.MaxAge)
		}
		out = append(out, c)
	}
	return out, nil
}

// parseNetscapeCookies parses Netscape cookies.txt content.
func parseNetscapeCookies(doc string) ([]*http.Cookie, error) {
	var out []*http.Cookie
	lines := strings.Split(doc, "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// "#HttpOnly_" prefix marks an http-only cookie in Firefox exports.
		httpOnly := false
		if strings.HasPrefix(trimmed, "#HttpOnly_") {
			httpOnly = true
			trimmed = strings.TrimPrefix(trimmed, "#HttpOnly_")
		} else if strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Split(trimmed, "\t")
		if len(fields) < 7 {
			return nil, fmt.Errorf("cookies: line %d: Netscape format needs 7 tab-separated fields, got %d", i+1, len(fields))
		}
		domain := fields[0]
		_, err := strconv.ParseInt(fields[4], 10, 64)
		var expires time.Time
		if err == nil {
			sec, _ := strconv.ParseInt(fields[4], 10, 64)
			if sec > 0 {
				expires = time.Unix(sec, 0)
			}
		}
		name, value := fields[5], fields[6]
		if name == "" {
			return nil, fmt.Errorf("cookies: line %d: empty cookie name", i+1)
		}
		c := &http.Cookie{
			Name: name, Value: value, Domain: domain, Path: fields[2],
			Secure: fields[3] == "TRUE", Expires: expires, HttpOnly: httpOnly,
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("cookies: no cookie lines found in Netscape input")
	}
	return out, nil
}
