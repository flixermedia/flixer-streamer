package httpserver

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/flixermedia/flixer-streamer/internal/token"
)

func TestDASHManifestRewritesBasesAndSegmentTemplates(t *testing.T) {
	now := time.Now()
	claims := token.Claims{TokenID: "dash-token", ExpiresAt: now.Add(time.Hour).Unix()}
	body := []byte(`<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="dynamic">
  <BaseURL>https://8.8.8.8:8080/live/</BaseURL>
  <Period>
    <AdaptationSet>
      <Representation id="video_0">
        <SegmentTemplate initialization="init-$RepresentationID$.mp4" media="seg-$Number%05d$-$RepresentationID$.m4s"/>
      </Representation>
    </AdaptationSet>
  </Period>
</MPD>`)

	proxy := newLiveProxy()
	rewritten, err := proxy.rewriteDASHManifest(context.Background(), "signed.token.value", claims,
		"https://8.8.8.8:8080/live/manifest.mpd", body)
	if err != nil {
		t.Fatal(err)
	}

	decoder := xml.NewDecoder(strings.NewReader(string(rewritten)))
	var basePath string
	var mediaTemplate string
	var initialization string
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Local == "SegmentTemplate" {
				for _, attribute := range value.Attr {
					switch attribute.Name.Local {
					case "media":
						mediaTemplate = attribute.Value
					case "initialization":
						initialization = attribute.Value
					}
				}
			}
		case xml.CharData:
			if strings.Contains(string(value), "/resource/") && strings.Contains(string(value), "signed.token.value") {
				basePath = strings.TrimSpace(string(value))
			}
		}
	}
	if !strings.HasPrefix(basePath, "/streamer/live/signed.token.value/resource/") {
		t.Fatalf("BaseURL was not proxied: %q", basePath)
	}
	if !strings.Contains(mediaTemplate, "?d0=$Number%05d$&d1=$RepresentationID$") {
		t.Fatalf("segment template substitutions were not retained: %q", mediaTemplate)
	}
	if !strings.Contains(initialization, "?d0=$RepresentationID$") {
		t.Fatalf("initialization template substitution was not retained: %q", initialization)
	}

	mediaID := strings.Split(resourceIDFromPath(mediaTemplate), "?")[0]
	resource, ok := proxy.resourceEntryFor(claims, mediaID, now)
	if !ok {
		t.Fatal("DASH media template was not registered")
	}
	target, err := resolveLiveResource(resource, "", url.Values{
		"d0": {"00042"},
		"d1": {"video_0"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if target != "https://8.8.8.8:8080/live/seg-00042-video_0.m4s" {
		t.Fatalf("resolved media target = %q", target)
	}
	baseID := strings.TrimSuffix(resourceIDFromPath(basePath), "/")
	baseResource, ok := proxy.resourceEntryFor(claims, baseID, now)
	if !ok || !baseResource.base {
		t.Fatal("DASH BaseURL resource was not registered as a base")
	}
	baseTarget, err := resolveLiveResource(baseResource, "segment-00042.m4s", nil)
	if err != nil {
		t.Fatal(err)
	}
	if baseTarget != "https://8.8.8.8:8080/live/segment-00042.m4s" {
		t.Fatalf("resolved BaseURL resource = %q", baseTarget)
	}
}

func TestDASHManifestDetectionAndResourceTraversalGuard(t *testing.T) {
	body := []byte(`<?xml version="1.0"?><MPD xmlns="urn:mpeg:dash:schema:mpd:2011"/>`)
	if !isDASHManifest(body, "application/dash+xml", "https://cdn.example/manifest") {
		t.Fatal("DASH manifest with a non-standard URL was not detected")
	}
	if isDASHManifest([]byte("<html>not a manifest</html>"), "application/dash+xml", "https://cdn.example/manifest") {
		t.Fatal("non-MPD XML was detected as a DASH manifest")
	}

	resource := liveResource{url: "https://8.8.8.8:8080/dash/", base: true}
	for _, suffix := range []string{"../secret", "%2e%2e/secret", "a/../secret", `a\secret`} {
		if _, err := resolveLiveResource(resource, suffix, nil); err == nil {
			t.Errorf("unsafe DASH resource suffix %q was accepted", suffix)
		}
	}
}

func TestDASHBaseURLPreservesQueryAndRejectsTraversalSubstitutions(t *testing.T) {
	resource := liveResource{url: "https://8.8.8.8:8080/dash/?token=abc123", base: true}
	target, err := resolveLiveResource(resource, "segment.m4s", url.Values{"quality": {"high"}})
	if err != nil {
		t.Fatal(err)
	}
	if target != "https://8.8.8.8:8080/dash/segment.m4s?quality=high&token=abc123" {
		t.Fatalf("resolved BaseURL resource = %q", target)
	}

	templateResource := liveResource{
		url: "https://8.8.8.8:8080/dash/segment-$RepresentationID$.m4s",
		placeholders: []dashURLPlaceholder{
			{marker: "$RepresentationID$", name: "RepresentationID"},
		},
	}
	for _, value := range []string{".", ".."} {
		if _, err := resolveLiveResource(templateResource, "", url.Values{"d0": {value}}); err == nil {
			t.Errorf("unsafe RepresentationID %q was accepted", value)
		}
	}
}

func TestDASHManifestRewritePreservesNamespaceMarkup(t *testing.T) {
	now := time.Now()
	claims := token.Claims{TokenID: "dash-namespace-test", ExpiresAt: now.Add(time.Hour).Unix()}
	body := []byte(`<?xml version="1.0"?>
<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:schemaLocation="urn:mpeg:dash:schema:mpd:2011 schema.xsd">
  <BaseURL>https://8.8.8.8:8080/live/</BaseURL>
  <Period><SegmentTemplate media="segment-$Number$.m4s"/></Period>
</MPD>`)
	proxy := newLiveProxy()
	rewritten, err := proxy.rewriteDASHManifest(
		context.Background(),
		"signed.token.value",
		claims,
		"https://8.8.8.8:8080/live/manifest.mpd",
		body,
	)
	if err != nil {
		t.Fatal(err)
	}

	decoder := xml.NewDecoder(strings.NewReader(string(rewritten)))
	var sawRoot, sawBaseURL, sawTemplate bool
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("rewritten MPD is not valid XML: %v", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Local == "MPD" {
				sawRoot = true
				if value.Name.Space != "urn:mpeg:dash:schema:mpd:2011" {
					t.Fatalf("MPD namespace = %q", value.Name.Space)
				}
				var schemaLocation bool
				for _, attribute := range value.Attr {
					if attribute.Name.Local == "schemaLocation" &&
						attribute.Name.Space == "http://www.w3.org/2001/XMLSchema-instance" {
						schemaLocation = true
					}
				}
				if !schemaLocation {
					t.Fatal("xsi:schemaLocation was not preserved")
				}
			}
			if value.Name.Local == "BaseURL" {
				sawBaseURL = true
			}
			if value.Name.Local == "SegmentTemplate" {
				sawTemplate = true
				for _, attribute := range value.Attr {
					if attribute.Name.Local == "media" &&
						!strings.Contains(attribute.Value, "/streamer/live/signed.token.value/resource/") {
						t.Fatalf("segment template was not proxied: %q", attribute.Value)
					}
				}
			}
		}
	}
	if !sawRoot || !sawBaseURL || !sawTemplate {
		t.Fatalf("incomplete rewritten MPD: root=%t baseURL=%t template=%t", sawRoot, sawBaseURL, sawTemplate)
	}
}
