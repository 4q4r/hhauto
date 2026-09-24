package hhapi

import (
	"regexp"
	"strings"
)

// Client hints must be generated from the SAME User-Agent string the client
// sends: a sec-ch-ua brand version that disagrees with the UA's Chrome major
// version is a classic bot marker. Everything here is derived, never guessed.

var chromeMajorRe = regexp.MustCompile(`Chrome/(\d+)`)
var edgeMajorRe = regexp.MustCompile(`Edg/(\d+)`)

// parseClientHints derives sec-ch-ua header values from a Chrome-family
// User-Agent. ok is false for UAs that do not advertise Chromium, in which
// case the client must omit the hints entirely (a real non-Chromium browser
// would not send them).
func parseClientHints(ua string) (brands, mobile, platform string, ok bool) {
	m := chromeMajorRe.FindStringSubmatch(ua)
	if m == nil {
		return "", "", "", false
	}
	major := m[1]

	// Greased brand list: Chromium's default grease token changed over
	// versions; a stable Not;A=Brand is what current stable Chrome sends.
	brands = `"Not;A=Brand";v="8", "Chromium";v="` + major + `"`
	if em := edgeMajorRe.FindStringSubmatch(ua); em != nil {
		brands = `"Not;A=Brand";v="8", "Chromium";v="` + major + `", "Microsoft Edge";v="` + em[1] + `"`
	} else {
		brands += `, "Google Chrome";v="` + major + `"`
	}

	mobile = "?0"
	platform = platformFromUA(ua)
	switch {
	case strings.Contains(ua, "Android"), strings.Contains(ua, "Mobile"):
		mobile = "?1"
	case platform == `"iOS"`:
		mobile = "?1"
	}
	return brands, mobile, platform, platform != ""
}

// platformFromUA maps the OS token of a User-Agent to the platform string
// sec-ch-ua-platform carries. Quote-wrapped for direct header use.
func platformFromUA(ua string) string {
	switch {
	case strings.Contains(ua, "Windows NT"):
		return `"Windows"`
	case strings.Contains(ua, "CrOS"):
		return `"Chrome OS"`
	case strings.Contains(ua, "Android"):
		return `"Android"`
	case strings.Contains(ua, "iPhone"), strings.Contains(ua, "iPad"):
		return `"iOS"`
	case strings.Contains(ua, "Mac OS X"), strings.Contains(ua, "Macintosh"):
		return `"macOS"`
	case strings.Contains(ua, "Linux"), strings.Contains(ua, "X11"):
		return `"Linux"`
	default:
		return ""
	}
}
