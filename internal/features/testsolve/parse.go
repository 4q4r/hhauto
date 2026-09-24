package testsolve

import (
	"net/url"
	"regexp"
	"strings"

	"golang.org/x/net/html"
)

// Form is the tolerant extraction result of an external form page.
type Form struct {
	Title  string
	Fields []FormField
}

// FormField is one answerable question on the form.
type FormField struct {
	Label    string   // question text (best effort)
	Type     string   // text | textarea | select | radio | check
	Options  []string // select options / radio group options
	Required bool
}

// DetectExternalLinks extracts candidate external-form URLs from free text:
// explicit http(s) links only, order-preserving, deduplicated.
func DetectExternalLinks(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, u := range extractLinks(text) {
		l := strings.ToLower(u)
		if !strings.HasPrefix(l, "http://") && !strings.HasPrefix(l, "https://") {
			continue
		}
		if seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}

// extractLinks is the raw URL scan (kit11 keeps the shared regex here locally
// to keep testsolve self-contained).
var linkRe = regexp.MustCompile(`https?://[^\s<>"')\]]+`)

func extractLinks(text string) []string {
	var out []string
	for _, m := range linkRe.FindAllString(text, -1) {
		out = append(out, strings.TrimRight(m, ".,;:!?)»\"'"))
	}
	return out
}

var passwordTypeRe = regexp.MustCompile(`(?i)<input[^>]+type\s*=\s*["']?password`)

// hasPasswordField reports whether the raw HTML contains an <input
// type="password">. The anti-phishing gate runs on raw bytes before any
// parsing so a malformed page cannot dodge it.
func hasPasswordField(htmlText string) bool {
	return passwordTypeRe.MatchString(htmlText)
}

// formKindForURL classifies a form URL host into the assessment provider
// label: google_form | yandex_form | other.
func formKindForURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "other"
	}
	h := strings.ToLower(u.Hostname())
	switch {
	case h == "forms.gle" || strings.HasSuffix(h, ".docs.google.com") || h == "docs.google.com":
		return "google_form"
	case h == "forms.yandex.ru" || strings.HasSuffix(h, ".forms.yandex.ru"):
		return "yandex_form"
	default:
		return "other"
	}
}

// ParseFormHTML extracts answerable fields from an external form page with a
// tolerant heuristic parser (label/input/textarea/select/radio groups). ok is
// false when nothing answerable was found — callers treat that as a parse
// failure, never as an empty form.
func ParseFormHTML(doc string) (Form, bool) {
	root, err := html.Parse(strings.NewReader(doc))
	if err != nil {
		return Form{}, false
	}

	f := Form{}
	// Pass 1: label[for=id] -> label text.
	labelByID := map[string]string{}
	var walk1 func(*html.Node)
	walk1 = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "label" {
			forAttr := attr(n, "for")
			if forAttr != "" {
				if t := textContent(n); t != "" {
					labelByID[forAttr] = t
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk1(c)
		}
	}
	walk1(root)
	if t := textContent(findFirst(root, "title")); t != "" {
		f.Title = t
	}

	// Pass 2: fields in document order.
	var lastText string
	seenRadio := map[string]bool{}
	var walk2 func(*html.Node)
	walk2 = func(n *html.Node) {
		if n.Type == html.TextNode {
			t := strings.TrimSpace(n.Data)
			if t != "" && len([]rune(t)) <= 200 && !isInside(n, "label", "option", "textarea", "title", "script", "style") {
				lastText = t
			}
			return
		}
		if n.Type != html.ElementNode && n.Type != html.DocumentNode {
			return
		}
		switch n.Data {
		case "h1", "h2", "legend":
			if t := textContent(n); t != "" && f.Title == "" {
				f.Title = t
			}
		case "input":
			typ := strings.ToLower(attr(n, "type"))
			if typ == "" {
				typ = "text"
			}
			switch typ {
			case "password", "hidden", "submit", "button", "image", "reset", "file", "date", "time":
				return
			case "radio", "checkbox":
				name := attr(n, "name")
				if typ == "radio" {
					if name == "" || seenRadio[name] {
						return
					}
					seenRadio[name] = true
					f.Fields = append(f.Fields, radioGroup(root, n, name, labelByID, &lastText))
					return
				}
				// checkbox: single yes/no question
				label := fieldLabel(n, labelByID, &lastText)
				f.Fields = append(f.Fields, FormField{Label: label, Type: "check"})
				lastText = ""
				return
			default: // text, email, tel, number, url, search...
				label := fieldLabel(n, labelByID, &lastText)
				f.Fields = append(f.Fields, FormField{
					Label:    label,
					Type:     "text",
					Required: attr(n, "required") != "" || boolAttr(n, "required"),
				})
				lastText = ""
				return
			}
		case "textarea":
			label := fieldLabel(n, labelByID, &lastText)
			if label == "" {
				label = attr(n, "placeholder")
			}
			f.Fields = append(f.Fields, FormField{
				Label:    label,
				Type:     "textarea",
				Required: attr(n, "required") != "" || boolAttr(n, "required"),
			})
			lastText = ""
			return
		case "select":
			var opts []string
			for o := range elements(n, "option") {
				opts = append(opts, textContent(o))
			}
			label := fieldLabel(n, labelByID, &lastText)
			f.Fields = append(f.Fields, FormField{
				Label:    label,
				Type:     "select",
				Options:  opts,
				Required: boolAttr(n, "required"),
			})
			lastText = ""
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk2(c)
		}
	}
	walk2(root)

	return f, len(f.Fields) > 0
}

// radioGroup collects all radios sharing name into one option-group field.
func radioGroup(root, first *html.Node, name string, labelByID map[string]string, lastText *string) FormField {
	var opts []string
	for r := range elements(root, "input") {
		if attr(r, "name") != name || strings.ToLower(attr(r, "type")) != "radio" {
			continue
		}
		if id := attr(r, "id"); id != "" {
			if t, ok := labelByID[id]; ok {
				opts = append(opts, t)
				continue
			}
		}
		opts = append(opts, attr(r, "value"))
	}
	// The question is the nearest preceding context text; the first option's
	// own label is only a fallback when no context was captured.
	label := *lastText
	if label == "" {
		if id := attr(first, "id"); id != "" {
			label = labelByID[id]
		}
	}
	*lastText = ""
	return FormField{Label: label, Type: "radio", Options: opts}
}

// fieldLabel resolves the question text for a field in priority order:
// explicit <label for=id>, ancestor <label>, aria-label, placeholder,
// nearest preceding text.
func fieldLabel(n *html.Node, labelByID map[string]string, lastText *string) string {
	if id := attr(n, "id"); id != "" {
		if t, ok := labelByID[id]; ok && t != "" {
			return t
		}
	}
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "label" {
			if t := textContent(p); t != "" {
				return t
			}
		}
	}
	if t := attr(n, "aria-label"); t != "" {
		return t
	}
	if t := attr(n, "placeholder"); t != "" {
		return t
	}
	return *lastText
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			return strings.TrimSpace(a.Val)
		}
	}
	return ""
}

func boolAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, key) {
			switch strings.ToLower(a.Val) {
			case "", "required", "true":
				return true
			}
			return false
		}
	}
	return false
}

// textContent renders the element's visible text (leaf text joined, no scripts).
func textContent(n *html.Node) string {
	if n == nil {
		return ""
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
			return
		}
		if c.Type == html.ElementNode && (c.Data == "script" || c.Data == "style") {
			return
		}
		for ch := c.FirstChild; ch != nil; ch = ch.NextSibling {
			walk(ch)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func findFirst(root *html.Node, name string) *html.Node {
	for n := range elements(root, name) {
		return n
	}
	return nil
}

// elements yields all descendant elements with the given tag name.
func elements(root *html.Node, name string) func(func(*html.Node) bool) {
	return func(yield func(*html.Node) bool) {
		var walk func(*html.Node) bool
		walk = func(n *html.Node) bool {
			if n.Type == html.ElementNode && n.Data == name {
				if !yield(n) {
					return false
				}
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				if !walk(c) {
					return false
				}
			}
			return true
		}
		walk(root)
	}
}

// isInside reports whether n has an ancestor with one of the given tags.
func isInside(n *html.Node, tags ...string) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode {
			for _, t := range tags {
				if p.Data == t {
					return true
				}
			}
		}
	}
	return false
}
