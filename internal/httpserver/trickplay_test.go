package httpserver

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flixermedia/flixer-streamer/internal/media"
	"github.com/flixermedia/flixer-streamer/internal/token"
)

func TestTrickplayServesIndexAndSheetWithValidToken(t *testing.T) {
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	locator := token.Locator{RootID: "movies", RelativePath: "folder/movie.mp4"}
	if got := trickplayKey(locator); got != "3c47b4b1a186d47b47783e012c0df2ab" {
		t.Fatalf("trickplay key = %q", got)
	}
	dir := t.TempDir()
	keyDir := filepath.Join(dir, trickplayKey(locator))
	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	index := []byte(`{"version":1,"count":1}`)
	sheet := []byte("jpeg-data")
	if err := os.WriteFile(filepath.Join(keyDir, "index.json"), index, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keyDir, "0.jpg"), sheet, 0o644); err != nil {
		t.Fatal(err)
	}
	resolver, err := media.NewResolver([]media.Root{{ID: "movies", Path: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Options{Secret: secret, Resolver: resolver, TrickplayDir: dir})
	streamToken := createStreamToken(t, secret, locator.RootID, locator.RelativePath)
	for _, test := range []struct {
		method, name, contentType, cache string
		body                             []byte
	}{{http.MethodGet, "index.json", "application/json", "private, max-age=300", index}, {http.MethodGet, "0.jpg", "image/jpeg", "private, max-age=86400", sheet}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(test.method, "/streamer/trickplay/"+streamToken+"/"+test.name, nil))
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != test.contentType || response.Header().Get("Cache-Control") != test.cache || !bytes.Equal(response.Body.Bytes(), test.body) {
			t.Errorf("%s response = %d, headers %v, body %q", test.name, response.Code, response.Header(), response.Body.Bytes())
		}
	}
	head := httptest.NewRecorder()
	handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/streamer/trickplay/"+streamToken+"/0.jpg", nil))
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD response = %d, body %q", head.Code, head.Body.String())
	}
	rangeRequest := httptest.NewRequest(http.MethodGet, "/streamer/trickplay/"+streamToken+"/0.jpg", nil)
	rangeRequest.Header.Set("Range", "bytes=0-3")
	ranged := httptest.NewRecorder()
	handler.ServeHTTP(ranged, rangeRequest)
	if ranged.Code != http.StatusPartialContent || ranged.Header().Get("Content-Range") != "bytes 0-3/9" || ranged.Body.String() != "jpeg" {
		t.Errorf("range response = %d, Content-Range %q, body %q", ranged.Code, ranged.Header().Get("Content-Range"), ranged.Body.String())
	}
}

func TestTrickplayServesHLSImagePlaylistAndWidthScopedTiles(t *testing.T) {
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	locator := token.Locator{RootID: "movies", RelativePath: "movie.mp4"}
	dir := t.TempDir()
	keyDir := filepath.Join(dir, trickplayKey(locator))
	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	index := []byte(`{"intervalMs":1000,"thumbWidth":320,"thumbHeight":180,"columns":10,"rows":10,"count":101,"sheets":2}`)
	sheet := []byte("jpeg-data")
	if err := os.WriteFile(filepath.Join(keyDir, "index.json"), index, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"0.jpg", "1.jpg"} {
		if err := os.WriteFile(filepath.Join(keyDir, name), sheet, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	resolver, err := media.NewResolver([]media.Root{{ID: "movies", Path: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Options{Secret: secret, Resolver: resolver, TrickplayDir: dir})
	streamToken := createStreamToken(t, secret, locator.RootID, locator.RelativePath)

	for _, test := range []struct {
		name string
		want string
	}{
		{"tiles.m3u8", "320/0.jpg"},
		{"320/tiles.m3u8", "0.jpg"},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/streamer/trickplay/"+streamToken+"/"+test.name, nil))
		body := response.Body.String()
		if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/vnd.apple.mpegurl" ||
			!strings.Contains(body, "#EXT-X-IMAGES-ONLY") || !strings.Contains(body, "#EXT-X-TILES:RESOLUTION=320x180,LAYOUT=10x10,DURATION=1.000") ||
			!strings.Contains(body, "#EXT-X-TARGETDURATION:100") || !strings.Contains(body, "#EXTINF:1.000,") || !strings.Contains(body, test.want) {
			t.Errorf("%s response = %d, headers %v, body %q", test.name, response.Code, response.Header(), body)
		}
	}

	tile := httptest.NewRecorder()
	handler.ServeHTTP(tile, httptest.NewRequest(http.MethodGet, "/streamer/trickplay/"+streamToken+"/320/1.jpg", nil))
	if tile.Code != http.StatusOK || tile.Header().Get("Content-Type") != "image/jpeg" || !bytes.Equal(tile.Body.Bytes(), sheet) {
		t.Errorf("tile response = %d, headers %v, body %q", tile.Code, tile.Header(), tile.Body.Bytes())
	}
}

func TestTrickplayServesJellyscrubBIFSidecar(t *testing.T) {
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	root := t.TempDir()
	mediaPath := filepath.Join(root, "17 Again (2009) - Bluray-1080p.mkv")
	if err := os.WriteFile(mediaPath, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	trickplayDir := filepath.Join(root, "trickplay")
	if err := os.MkdirAll(trickplayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	baseName := "17 Again (2009) - Bluray-1080p"
	writeBIFFixture(t, trickplayDir, baseName, 320)
	if err := os.WriteFile(filepath.Join(trickplayDir, baseName+"-manifest.json"), []byte(`{"Version":"1.0","WidthResolutions":[320]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	resolver, err := media.NewResolver([]media.Root{{ID: "movies", Path: root}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Options{Secret: secret, Resolver: resolver, TrickplayDir: t.TempDir()})
	streamToken := createStreamToken(t, secret, "movies", "17 Again (2009) - Bluray-1080p.mkv")

	playlist := httptest.NewRecorder()
	handler.ServeHTTP(playlist, httptest.NewRequest(http.MethodGet, "/streamer/trickplay/"+streamToken+"/tiles.m3u8", nil))
	if playlist.Code != http.StatusOK || !strings.Contains(playlist.Body.String(), "#EXT-X-IMAGES-ONLY") ||
		!strings.Contains(playlist.Body.String(), "#EXT-X-TILES:RESOLUTION=320x180,LAYOUT=1x1,DURATION=10.000") ||
		!strings.Contains(playlist.Body.String(), "320/1.jpg") {
		t.Fatalf("Jellyscrub playlist = %d, headers %v, body %q", playlist.Code, playlist.Header(), playlist.Body.String())
	}

	tile := httptest.NewRecorder()
	handler.ServeHTTP(tile, httptest.NewRequest(http.MethodGet, "/streamer/trickplay/"+streamToken+"/320/1.jpg", nil))
	if tile.Code != http.StatusOK || tile.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("Jellyscrub tile = %d, headers %v", tile.Code, tile.Header())
	}
	config, err := jpeg.DecodeConfig(bytes.NewReader(tile.Body.Bytes()))
	if err != nil || config.Width != 320 || config.Height != 180 {
		t.Fatalf("BIF tile JPEG = %dx%d, err %v", config.Width, config.Height, err)
	}
}

func TestLoadJellyscrubBIFFromEpisodeSeasonFolder(t *testing.T) {
	root := t.TempDir()
	relativePath := "TV Shows/9-1-1/Season 1/9-1-1 - S01E01 - Pilot WEBDL-1080p.mkv"
	mediaPath := filepath.Join(root, filepath.FromSlash(relativePath))
	if err := os.MkdirAll(filepath.Dir(mediaPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mediaPath, []byte("media"), 0o644); err != nil {
		t.Fatal(err)
	}
	trickplayDir := filepath.Join(filepath.Dir(mediaPath), "trickplay")
	if err := os.MkdirAll(trickplayDir, 0o755); err != nil {
		t.Fatal(err)
	}
	baseName := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
	writeBIFFixture(t, trickplayDir, baseName, 320)
	if err := os.WriteFile(filepath.Join(trickplayDir, baseName+"-manifest.json"), []byte(`{"WidthResolutions":[320]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	resolver, err := media.NewResolver([]media.Root{{ID: "tv", Path: root}})
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{resolver: resolver}

	trickplay, err := handler.loadMediaBIF(token.Locator{RootID: "tv", RelativePath: relativePath}, 320)
	if err != nil || trickplay == nil || len(trickplay.frames) != 2 {
		t.Fatalf("episode BIF = %+v, err %v", trickplay, err)
	}
}

func writeBIFFixture(t *testing.T, directory, baseName string, width int) {
	t.Helper()
	var images [][]byte
	for i := 0; i < 2; i++ {
		var imageBytes bytes.Buffer
		if err := jpeg.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, width, 180)), nil); err != nil {
			t.Fatal(err)
		}
		images = append(images, imageBytes.Bytes())
	}
	headerSize := bifHeaderSize
	tableSize := (len(images) + 1) * bifIndexEntrySize
	offset := uint32(headerSize + tableSize)
	var file bytes.Buffer
	file.Write(bifMagic[:])
	file.WriteByte(0)
	file.Write(make([]byte, 3))
	_ = binary.Write(&file, binary.LittleEndian, uint32(len(images)))
	_ = binary.Write(&file, binary.LittleEndian, uint32(10_000))
	file.Write(make([]byte, bifHeaderSize-file.Len()))
	for i, imageBytes := range images {
		_ = binary.Write(&file, binary.LittleEndian, uint32(i))
		_ = binary.Write(&file, binary.LittleEndian, offset)
		offset += uint32(len(imageBytes))
	}
	_ = binary.Write(&file, binary.LittleEndian, ^uint32(0))
	_ = binary.Write(&file, binary.LittleEndian, offset)
	for _, imageBytes := range images {
		file.Write(imageBytes)
	}
	if file.Len() != int(offset) {
		t.Fatalf("BIF fixture length = %d, expected %d", file.Len(), offset)
	}
	if err := os.WriteFile(filepath.Join(directory, fmt.Sprintf("%s-%d.bif", baseName, width)), file.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestReadBIFIndexRejectsInvalidHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.bif")
	if err := os.WriteFile(path, []byte("invalid"), 0o644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readBIFIndex(file, 7); err == nil {
		t.Fatal("expected invalid BIF to be rejected")
	}
	_ = file.Close()
}

func TestTrickplayMissingTraversalAndBadToken(t *testing.T) {
	secret := []byte("test-secret-that-is-at-least-thirty-two-bytes")
	locator := token.Locator{RootID: "movies", RelativePath: "movie.mp4"}
	resolver, err := media.NewResolver([]media.Root{{ID: "movies", Path: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(Options{Secret: secret, Resolver: resolver, TrickplayDir: t.TempDir()})
	valid := createStreamToken(t, secret, locator.RootID, locator.RelativePath)
	for _, test := range []struct {
		url  string
		code int
		want string
	}{{"/streamer/trickplay/" + valid + "/9.jpg", http.StatusNotFound, "trickplay_not_available"}, {"/streamer/trickplay/" + valid + "/../index.json", http.StatusNotFound, "trickplay_not_available"}, {"/streamer/trickplay/not-a-token/index.json", http.StatusUnauthorized, "invalid_token"}} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.url, nil))
		var body map[string]any
		_ = json.Unmarshal(response.Body.Bytes(), &body)
		if response.Code != test.code || body["error"] != test.want {
			t.Errorf("GET %s = %d %v, want %d code %q", test.url, response.Code, body, test.code, test.want)
		}
	}
}

func TestTrickplayRejectsUnsupportedMethod(t *testing.T) {
	handler := NewHandler(Options{Secret: []byte("test-secret-that-is-at-least-thirty-two-bytes")})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/streamer/trickplay/no/index.json", nil))
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST response = %d, Allow %q", response.Code, response.Header().Get("Allow"))
	}
}
