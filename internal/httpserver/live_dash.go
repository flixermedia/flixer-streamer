package httpserver

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/flixermedia/flixer-streamer/internal/token"
)

var dashTemplatePattern = regexp.MustCompile(`\$(Number|Time|Bandwidth|RepresentationID)(%0[1-9][0-9]*d)?\$`)
var dashMarkerPattern = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
var dashNumberPattern = regexp.MustCompile(`^[0-9]+$`)

type dashURLPlaceholder struct {
	marker   string
	template string
	name     string
}

type dashManifestNode struct {
	start        xml.StartElement
	startTagFrom int
	startTagTo   int
	endTagFrom   int
	content      []dashManifestContent
}

type dashManifestContent struct {
	node     *dashManifestNode
	token    xml.Token
	rawFrom  int
	rawUntil int
}

type dashXMLReplacement struct {
	from  int
	until int
	value string
}

func isDASHURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && strings.HasSuffix(strings.ToLower(parsed.Path), ".mpd")
}

func isDASHManifestType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && strings.EqualFold(mediaType, "application/dash+xml")
}

func isDASHManifest(body []byte, contentType, rawURL string) bool {
	if isDASHManifestType(contentType) || isDASHURL(rawURL) {
		return isDASHXMLPrefix(body)
	}
	return isDASHXMLPrefix(body)
}

func isDASHXMLPrefix(body []byte) bool {
	probe := body
	if len(probe) > 1024 {
		probe = probe[:1024]
	}
	decoder := xml.NewDecoder(bytes.NewReader(probe))
	for {
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		if start, ok := token.(xml.StartElement); ok {
			return start.Name.Local == "MPD"
		}
	}
}

func (p *liveProxy) rewriteDASHManifest(
	ctx context.Context,
	rawToken string,
	claims token.Claims,
	manifestURL string,
	body []byte,
) ([]byte, error) {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	var root *dashManifestNode
	for {
		tokenOffset := int(decoder.InputOffset())
		token, err := decoder.Token()
		if err != nil {
			return nil, errors.New("invalid DASH XML manifest")
		}
		if element, ok := token.(xml.StartElement); ok {
			if element.Name.Local != "MPD" {
				return nil, errors.New("DASH XML root element is not MPD")
			}
			root, err = readDASHElement(decoder, element, tokenOffset)
			if err != nil {
				return nil, err
			}
			break
		}
	}

	base, err := url.Parse(manifestURL)
	if err != nil {
		return nil, errors.New("invalid DASH manifest URL")
	}
	var replacements []dashXMLReplacement
	if _, err := p.rewriteDASHElement(ctx, rawToken, claims, root, base, make(map[string]struct{}), body, &replacements); err != nil {
		return nil, err
	}

	for {
		_, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.New("invalid trailing DASH XML")
		}
	}

	return applyDASHReplacements(body, replacements)
}

func readDASHElement(decoder *xml.Decoder, start xml.StartElement, startOffset int) (*dashManifestNode, error) {
	startEnd := int(decoder.InputOffset())
	node := &dashManifestNode{
		start:        start,
		startTagFrom: startOffset,
		startTagTo:   startEnd,
	}
	for {
		tokenOffset := int(decoder.InputOffset())
		token, err := decoder.Token()
		if err != nil {
			return nil, errors.New("invalid DASH XML element")
		}
		tokenEnd := int(decoder.InputOffset())
		switch value := token.(type) {
		case xml.StartElement:
			child, err := readDASHElement(decoder, value, tokenOffset)
			if err != nil {
				return nil, err
			}
			node.content = append(node.content, dashManifestContent{node: child})
		case xml.EndElement:
			if value.Name != start.Name {
				return nil, errors.New("mismatched DASH XML element")
			}
			node.endTagFrom = tokenOffset
			return node, nil
		default:
			node.content = append(node.content, dashManifestContent{
				token:    token,
				rawFrom:  tokenOffset,
				rawUntil: tokenEnd,
			})
		}
	}
}

func (p *liveProxy) rewriteDASHElement(
	ctx context.Context,
	rawToken string,
	claims token.Claims,
	node *dashManifestNode,
	base *url.URL,
	validatedHosts map[string]struct{},
	body []byte,
	replacements *[]dashXMLReplacement,
) (*url.URL, error) {
	if node.start.Name.Local == "BaseURL" {
		return p.rewriteDASHBaseElement(ctx, rawToken, claims, node, base, validatedHosts, body, replacements)
	}
	if node.start.Name.Local == "Location" || node.start.Name.Local == "PatchLocation" {
		rewritten, err := p.rewriteDASHTextReference(ctx, rawToken, claims, node, base, validatedHosts, body)
		if err != nil {
			return nil, err
		}
		if rewritten.value != "" {
			*replacements = append(*replacements, rewritten)
		}
		return base, nil
	}

	for _, attribute := range node.start.Attr {
		if attribute.Name.Local != "media" &&
			attribute.Name.Local != "initialization" &&
			attribute.Name.Local != "sourceURL" &&
			attribute.Name.Local != "index" {
			continue
		}
		if strings.TrimSpace(attribute.Value) == "" {
			continue
		}
		valueFrom, valueUntil, err := findDASHAttributeValue(
			body[node.startTagFrom:node.startTagTo],
			attribute.Name.Local,
		)
		if err != nil {
			return nil, fmt.Errorf("find DASH attribute %q: %w", attribute.Name.Local, err)
		}
		rewritten, err := p.registerDASHReference(ctx, rawToken, claims, base, attribute.Value, validatedHosts)
		if err != nil {
			return nil, err
		}
		*replacements = append(*replacements, dashXMLReplacement{
			from:  node.startTagFrom + valueFrom,
			until: node.startTagFrom + valueUntil,
			value: escapeDASHXMLAttribute(rewritten, body[node.startTagFrom+valueFrom-1]),
		})
	}

	currentBase := base
	for index := range node.content {
		content := &node.content[index]
		if content.node == nil {
			continue
		}
		nextBase, err := p.rewriteDASHElement(ctx, rawToken, claims, content.node, currentBase, validatedHosts, body, replacements)
		if err != nil {
			return nil, err
		}
		if content.node.start.Name.Local == "BaseURL" {
			currentBase = nextBase
		}
	}
	return currentBase, nil
}

func (p *liveProxy) rewriteDASHBaseElement(
	ctx context.Context,
	rawToken string,
	claims token.Claims,
	node *dashManifestNode,
	base *url.URL,
	validatedHosts map[string]struct{},
	body []byte,
	replacements *[]dashXMLReplacement,
) (*url.URL, error) {
	value, rawFrom, rawUntil, err := dashTextContent(node, body)
	if err != nil {
		return nil, err
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return base, nil
	}
	reference, err := url.Parse(value)
	if err != nil {
		return nil, errors.New("invalid DASH BaseURL")
	}
	resolved := base.ResolveReference(reference)
	resolved.Fragment = ""
	if err := validateDASHReference(ctx, resolved, validatedHosts); err != nil {
		return nil, err
	}
	key := "dash-base:" + resolved.String()
	id, ok := p.registerResourceEntry(claims, key, liveResource{url: resolved.String(), base: true}, time.Now())
	if !ok {
		return nil, errors.New("too many live DASH resources")
	}
	escaped := url.PathEscape(rawToken)
	proxyURL := "/streamer/live/" + escaped + "/resource/" + id + "/"
	leading, trailing := dashTextWhitespace(body[rawFrom:rawUntil])
	*replacements = append(*replacements, dashXMLReplacement{
		from:  rawFrom,
		until: rawUntil,
		value: leading + escapeDASHXMLText(proxyURL) + trailing,
	})
	return resolved, nil
}

func (p *liveProxy) rewriteDASHTextReference(
	ctx context.Context,
	rawToken string,
	claims token.Claims,
	node *dashManifestNode,
	base *url.URL,
	validatedHosts map[string]struct{},
	body []byte,
) (dashXMLReplacement, error) {
	value, rawFrom, rawUntil, err := dashTextContent(node, body)
	if err != nil {
		return dashXMLReplacement{}, err
	}
	if strings.TrimSpace(value) == "" {
		return dashXMLReplacement{}, nil
	}
	rewritten, err := p.registerDASHReference(ctx, rawToken, claims, base, value, validatedHosts)
	if err != nil {
		return dashXMLReplacement{}, err
	}
	leading, trailing := dashTextWhitespace(body[rawFrom:rawUntil])
	return dashXMLReplacement{
		from:  rawFrom,
		until: rawUntil,
		value: leading + escapeDASHXMLText(rewritten) + trailing,
	}, nil
}

func dashTextContent(node *dashManifestNode, body []byte) (string, int, int, error) {
	var text strings.Builder
	rawFrom, rawUntil := -1, -1
	for _, content := range node.content {
		if content.node != nil {
			return "", 0, 0, errors.New("DASH URL element cannot contain child elements")
		}
		if data, ok := content.token.(xml.CharData); ok {
			text.Write(data)
			if rawFrom < 0 {
				rawFrom = content.rawFrom
			}
			rawUntil = content.rawUntil
		} else if _, ok := content.token.(xml.Comment); !ok {
			return "", 0, 0, errors.New("invalid DASH URL element content")
		}
	}
	if rawFrom < 0 {
		rawFrom = node.startTagTo
		rawUntil = node.endTagFrom
	}
	if rawFrom < 0 || rawUntil < rawFrom || rawUntil > len(body) {
		return "", 0, 0, errors.New("invalid DASH URL element range")
	}
	return text.String(), rawFrom, rawUntil, nil
}

func dashTextWhitespace(raw []byte) (string, string) {
	leadingLength := len(raw) - len(bytes.TrimLeft(raw, " \t\r\n"))
	trailingLength := len(raw) - len(bytes.TrimRight(raw, " \t\r\n"))
	if leadingLength+trailingLength > len(raw) {
		return string(raw), ""
	}
	return string(raw[:leadingLength]), string(raw[len(raw)-trailingLength:])
}

func findDASHAttributeValue(tag []byte, localName string) (int, int, error) {
	tagStart := bytes.IndexByte(tag, '<')
	if tagStart < 0 {
		return 0, 0, errors.New("invalid DASH XML start tag")
	}
	index := tagStart + 1
	for index < len(tag) && !isDASHXMLSpace(tag[index]) && tag[index] != '>' && tag[index] != '/' {
		index++
	}
	for index < len(tag) {
		for index < len(tag) && isDASHXMLSpace(tag[index]) {
			index++
		}
		if index >= len(tag) || tag[index] == '>' || tag[index] == '/' {
			break
		}
		nameStart := index
		for index < len(tag) && !isDASHXMLSpace(tag[index]) && tag[index] != '=' && tag[index] != '>' && tag[index] != '/' {
			index++
		}
		name := string(tag[nameStart:index])
		for index < len(tag) && isDASHXMLSpace(tag[index]) {
			index++
		}
		if index >= len(tag) || tag[index] != '=' {
			return 0, 0, fmt.Errorf("invalid DASH XML attribute in %q at %d", tag, index)
		}
		index++
		for index < len(tag) && isDASHXMLSpace(tag[index]) {
			index++
		}
		if index >= len(tag) || (tag[index] != '"' && tag[index] != '\'') {
			return 0, 0, errors.New("invalid DASH XML attribute value")
		}
		quote := tag[index]
		index++
		valueStart := index
		for index < len(tag) && tag[index] != quote {
			index++
		}
		if index >= len(tag) {
			return 0, 0, errors.New("unterminated DASH XML attribute")
		}
		if colon := strings.LastIndexByte(name, ':'); colon >= 0 {
			name = name[colon+1:]
		}
		if name == localName {
			return valueStart, index, nil
		}
		index++
	}
	return 0, 0, errors.New("DASH XML attribute value was not found")
}

func isDASHXMLSpace(value byte) bool {
	return value == ' ' || value == '\t' || value == '\r' || value == '\n'
}

func escapeDASHXMLText(value string) string {
	var escaped bytes.Buffer
	_ = xml.EscapeText(&escaped, []byte(value))
	return escaped.String()
}

func escapeDASHXMLAttribute(value string, quote byte) string {
	escaped := escapeDASHXMLText(value)
	if quote == '"' {
		return strings.ReplaceAll(escaped, `"`, "&#34;")
	}
	return strings.ReplaceAll(escaped, "'", "&#39;")
}

func applyDASHReplacements(body []byte, replacements []dashXMLReplacement) ([]byte, error) {
	sort.Slice(replacements, func(left, right int) bool {
		return replacements[left].from > replacements[right].from
	})
	result := append([]byte(nil), body...)
	previousFrom := len(body)
	for _, replacement := range replacements {
		if replacement.from < 0 || replacement.until < replacement.from ||
			replacement.until > previousFrom || replacement.until > len(body) {
			return nil, errors.New("invalid DASH XML replacement range")
		}
		result = append(result[:replacement.from], append([]byte(replacement.value), result[replacement.until:]...)...)
		previousFrom = replacement.from
	}
	return result, nil
}

func (p *liveProxy) registerDASHReference(
	ctx context.Context,
	rawToken string,
	claims token.Claims,
	base *url.URL,
	reference string,
	validatedHosts map[string]struct{},
) (string, error) {
	matches := dashTemplatePattern.FindAllStringSubmatch(reference, -1)
	if len(matches) == 0 {
		if strings.Contains(reference, "$$") {
			reference = strings.ReplaceAll(reference, "$$", "$")
		}
		return p.registerReference(ctx, rawToken, claims, base, reference, validatedHosts)
	}

	markerIndexes := make(map[string]int)
	var placeholders []dashURLPlaceholder
	resolvedReference := dashTemplatePattern.ReplaceAllStringFunc(reference, func(template string) string {
		if index, ok := markerIndexes[template]; ok {
			return placeholders[index].marker
		}
		match := dashTemplatePattern.FindStringSubmatch(template)
		index := len(placeholders)
		marker := fmt.Sprintf("flixerdashmarker%d", index)
		markerIndexes[template] = index
		placeholders = append(placeholders, dashURLPlaceholder{marker: marker, template: template, name: match[1]})
		return marker
	})
	ref, err := url.Parse(strings.TrimSpace(resolvedReference))
	if err != nil || (ref.Scheme == "" && ref.Host == "" && ref.Path == "" && ref.RawQuery == "") {
		return "", errors.New("invalid DASH URI template")
	}
	resolved := base.ResolveReference(ref)
	resolved.Fragment = ""
	for _, placeholder := range placeholders {
		if strings.Contains(resolved.Host, placeholder.marker) {
			return "", errors.New("DASH URI templates cannot change the upstream host")
		}
	}
	if err := validateDASHReference(ctx, resolved, validatedHosts); err != nil {
		return "", err
	}
	target := resolved.String()
	key := "dash-template:" + target
	id, ok := p.registerResourceEntry(claims, key, liveResource{url: target, placeholders: placeholders}, time.Now())
	if !ok {
		return "", errors.New("too many live DASH resources")
	}
	rewritten := "/streamer/live/" + url.PathEscape(rawToken) + "/resource/" + id
	for index, placeholder := range placeholders {
		separator := "?"
		if strings.Contains(rewritten, "?") {
			separator = "&"
		}
		rewritten += separator + "d" + fmt.Sprint(index) + "=" + placeholder.template
	}
	return rewritten, nil
}

func validateDASHReference(ctx context.Context, target *url.URL, validatedHosts map[string]struct{}) error {
	safeURL, err := parseLiveURL(target.String())
	if err != nil {
		return errors.New("DASH URI is not a permitted public URL")
	}
	host := strings.ToLower(safeURL.Hostname())
	if _, ok := validatedHosts[host]; !ok {
		ips, err := resolvePublicHost(ctx, safeURL)
		if err != nil || len(ips) == 0 {
			return errors.New("DASH URI is not a permitted public URL")
		}
		validatedHosts[host] = struct{}{}
	}
	return nil
}

func resolveLiveResource(resource liveResource, suffix string, query url.Values) (string, error) {
	if resource.base {
		if suffix == "" || strings.ContainsAny(suffix, "\\:") {
			return "", errors.New("invalid DASH resource path")
		}
		decoded, err := url.PathUnescape(suffix)
		if err != nil {
			return "", errors.New("invalid DASH resource path")
		}
		suffix = decoded
		for _, segment := range strings.Split(suffix, "/") {
			if segment == "" || segment == "." || segment == ".." {
				return "", errors.New("invalid DASH resource path")
			}
		}
		base, err := url.Parse(resource.url)
		if err != nil {
			return "", err
		}
		resolved := base.ResolveReference(&url.URL{Path: suffix})
		baseQuery := base.Query()
		for key, values := range query {
			if _, exists := baseQuery[key]; !exists {
				baseQuery[key] = values
			}
		}
		resolved.RawQuery = baseQuery.Encode()
		return resolved.String(), nil
	}
	if suffix != "" {
		return "", errors.New("unexpected resource path")
	}
	if len(resource.placeholders) == 0 {
		return resource.url, nil
	}
	target := resource.url
	for index, placeholder := range resource.placeholders {
		value := query.Get("d" + fmt.Sprint(index))
		pattern := dashMarkerPattern
		if placeholder.name == "Number" || placeholder.name == "Time" || placeholder.name == "Bandwidth" {
			pattern = dashNumberPattern
		}
		if !pattern.MatchString(value) {
			return "", errors.New("invalid DASH segment substitution")
		}
		if placeholder.name == "RepresentationID" && (value == "." || value == "..") {
			return "", errors.New("invalid DASH segment substitution")
		}
		target = strings.ReplaceAll(target, placeholder.marker, url.PathEscape(value))
	}
	return target, nil
}
