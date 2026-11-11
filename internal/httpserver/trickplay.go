package httpserver

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/flixermedia/flixer-streamer/internal/token"
)

var trickplaySheetName = regexp.MustCompile(`^[0-9]+\.jpg$`)

type trickplayIndex struct {
	IntervalMs  int `json:"intervalMs"`
	ThumbWidth  int `json:"thumbWidth"`
	ThumbHeight int `json:"thumbHeight"`
	Columns     int `json:"columns"`
	Rows        int `json:"rows"`
	Count       int `json:"count"`
	Sheets      int `json:"sheets"`
}

type jellyscrubManifest struct {
	WidthResolutions []int `json:"widthResolutions"`
}

type bifFrame struct {
	timestamp uint32
	offset    int64
	size      int64
}

type mediaBIFTrickplay struct {
	width        int
	frames       []bifFrame
	intervalMs   int
	thumbWidth   int
	thumbHeight  int
	modTime      time.Time
	relativePath string
}

const (
	bifHeaderSize     = 64
	bifIndexEntrySize = 8
	maxBIFFrameCount  = 1_000_000
	maxManifestSize   = 64 << 10
)

var bifMagic = [8]byte{0x89, 'B', 'I', 'F', '\r', '\n', 0x1a, '\n'}

func trickplayKey(locator token.Locator) string {
	sum := sha256.Sum256([]byte(locator.RootID + "\n" + locator.RelativePath))
	return hex.EncodeToString(sum[:])[:32]
}

func (h *Handler) handleTrickplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method not allowed")
		return
	}
	remaining := strings.TrimPrefix(r.URL.Path, "/trickplay/")
	rawToken, name, hasName := strings.Cut(remaining, "/")
	locator, _, ok := h.authorizeToken(w, r, rawToken)
	if !ok {
		return
	}
	if !hasName || name == "" {
		h.writeError(w, http.StatusNotFound, "trickplay_not_available", "Trickplay is not available")
		return
	}
	keyDir := filepath.Join(h.trickplayDir, trickplayKey(locator))
	if name == "tiles.m3u8" {
		if _, err := readTrickplayIndex(keyDir); err == nil {
			h.serveTrickplayPlaylist(w, r, keyDir, "")
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			h.logger.Warn("read generated trickplay index failed; checking media-side trickplay", "error", err)
		}
		h.serveMediaTrickplayPlaylist(w, r, locator, 0)
		return
	}
	first, rest, nested := strings.Cut(name, "/")
	if nested && rest == "tiles.m3u8" {
		width, err := strconv.Atoi(first)
		if err != nil || width <= 0 {
			h.writeError(w, http.StatusNotFound, "trickplay_not_available", "Trickplay is not available")
			return
		}
		if _, err := readTrickplayIndex(keyDir); err == nil {
			h.serveTrickplayPlaylist(w, r, keyDir, first)
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			h.logger.Warn("read generated trickplay index failed; checking media-side trickplay", "error", err)
		}
		h.serveMediaTrickplayPlaylist(w, r, locator, width)
		return
	}
	if nested {
		width, err := strconv.Atoi(first)
		if err != nil || width <= 0 || !trickplaySheetName.MatchString(rest) {
			h.writeError(w, http.StatusNotFound, "trickplay_not_available", "Trickplay is not available")
			return
		}
		index, indexErr := readTrickplayIndex(keyDir)
		if indexErr == nil && index.ThumbWidth == width {
			name = rest
		} else {
			if indexErr != nil && !errors.Is(indexErr, os.ErrNotExist) {
				h.logger.Warn("read generated trickplay index failed; checking media-side trickplay", "error", indexErr)
			}
			h.serveMediaTrickplayTile(w, r, locator, width, rest)
			return
		}
	}
	if strings.Contains(name, "/") || (name != "index.json" && !trickplaySheetName.MatchString(name)) {
		h.writeError(w, http.StatusNotFound, "trickplay_not_available", "Trickplay is not available")
		return
	}
	filePath := filepath.Join(keyDir, name)
	file, err := os.Open(filePath)
	if errors.Is(err, os.ErrNotExist) {
		h.writeError(w, http.StatusNotFound, "trickplay_not_available", "Trickplay is not available")
		return
	}
	if err != nil {
		h.logger.Error("open trickplay file failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "trickplay_failed", "Unable to read trickplay")
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		h.writeError(w, http.StatusNotFound, "trickplay_not_available", "Trickplay is not available")
		return
	}
	if name == "index.json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "private, max-age=300")
	} else {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=86400")
	}
	http.ServeContent(w, r, name, info.ModTime(), file)
}

func (h *Handler) loadMediaBIF(locator token.Locator, requestedWidth int) (*mediaBIFTrickplay, error) {
	mediaName := path.Base(locator.RelativePath)
	baseName := strings.TrimSuffix(mediaName, path.Ext(mediaName))
	trickplayDir := path.Join(path.Dir(locator.RelativePath), "trickplay")
	manifestPath := path.Join(trickplayDir, baseName+"-manifest.json")
	manifestFile, err := h.resolver.Open(locator.RootID, manifestPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open Jellyscrub manifest: %w", err)
	}
	manifestBytes, readErr := io.ReadAll(io.LimitReader(manifestFile, maxManifestSize+1))
	closeErr := manifestFile.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read Jellyscrub manifest: %w", readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close Jellyscrub manifest: %w", closeErr)
	}
	if len(manifestBytes) > maxManifestSize {
		return nil, errors.New("Jellyscrub manifest is too large")
	}
	var manifest jellyscrubManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, fmt.Errorf("decode Jellyscrub manifest: %w", err)
	}
	widths := make([]int, 0, len(manifest.WidthResolutions))
	seen := make(map[int]struct{}, len(manifest.WidthResolutions))
	for _, width := range manifest.WidthResolutions {
		if width <= 0 || width > 4096 {
			continue
		}
		if _, exists := seen[width]; exists {
			continue
		}
		seen[width] = struct{}{}
		widths = append(widths, width)
	}
	if requestedWidth > 0 {
		widths = []int{requestedWidth}
	} else {
		sort.Slice(widths, func(i, j int) bool {
			distanceI, distanceJ := absInt(widths[i]-320), absInt(widths[j]-320)
			if distanceI == distanceJ {
				return widths[i] > widths[j]
			}
			return distanceI < distanceJ
		})
	}

	for _, width := range widths {
		if _, listed := seen[width]; !listed {
			continue
		}
		bifPath := path.Join(trickplayDir, fmt.Sprintf("%s-%d.bif", baseName, width))
		file, err := h.resolver.Open(locator.RootID, bifPath)
		if errors.Is(err, os.ErrNotExist) {
			if requestedWidth > 0 {
				return nil, nil
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("open Jellyscrub BIF file: %w", err)
		}
		info, statErr := file.Stat()
		if statErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("inspect Jellyscrub BIF file: %w", statErr)
		}
		index, parseErr := readBIFIndex(file, info.Size())
		if parseErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("parse Jellyscrub BIF file: %w", parseErr)
		}
		frame := index.frames[0]
		config, decodeErr := jpeg.DecodeConfig(io.NewSectionReader(file, frame.offset, frame.size))
		if decodeErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("decode first Jellyscrub BIF image: %w", decodeErr)
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("close Jellyscrub BIF file: %w", err)
		}
		index.width = width
		index.thumbWidth = config.Width
		index.thumbHeight = config.Height
		index.modTime = info.ModTime()
		index.relativePath = bifPath
		return &index, nil
	}
	return nil, nil
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func readBIFIndex(file *os.File, fileSize int64) (mediaBIFTrickplay, error) {
	var result mediaBIFTrickplay
	if fileSize < bifHeaderSize {
		return result, errors.New("BIF file is shorter than its header")
	}
	header := make([]byte, bifHeaderSize)
	if _, err := file.ReadAt(header, 0); err != nil {
		return result, fmt.Errorf("read BIF header: %w", err)
	}
	if string(header[:8]) != string(bifMagic[:]) {
		return result, errors.New("invalid BIF magic number")
	}
	if header[8] != 0 {
		return result, fmt.Errorf("unsupported BIF version %d", header[8])
	}
	frameCount := binary.LittleEndian.Uint32(header[12:16])
	if frameCount == 0 || frameCount > maxBIFFrameCount {
		return result, fmt.Errorf("invalid BIF frame count %d", frameCount)
	}
	multiplier := binary.LittleEndian.Uint32(header[16:20])
	if multiplier == 0 {
		multiplier = 1000
	}
	tableEnd := uint64(bifHeaderSize) + (uint64(frameCount)+1)*bifIndexEntrySize
	if tableEnd > uint64(fileSize) {
		return result, errors.New("BIF index exceeds file size")
	}
	table := make([]byte, int((uint64(frameCount)+1)*bifIndexEntrySize))
	if _, err := file.ReadAt(table, bifHeaderSize); err != nil {
		return result, fmt.Errorf("read BIF index: %w", err)
	}
	frames := make([]bifFrame, frameCount)
	// The final index entry is a sentinel at EOF, which supplies the last frame's size.
	for i := uint32(0); i <= frameCount; i++ {
		offset := int(i) * bifIndexEntrySize
		timestamp := binary.LittleEndian.Uint32(table[offset : offset+4])
		imageOffset := binary.LittleEndian.Uint32(table[offset+4 : offset+8])
		if uint64(imageOffset) < tableEnd || int64(imageOffset) > fileSize {
			return result, fmt.Errorf("invalid BIF image offset at entry %d", i)
		}
		if i == frameCount {
			if timestamp != ^uint32(0) || int64(imageOffset) != fileSize {
				return result, errors.New("invalid BIF end-of-data index entry")
			}
			frames[i-1].size = int64(imageOffset) - frames[i-1].offset
			break
		}
		if timestamp == ^uint32(0) {
			return result, fmt.Errorf("unexpected BIF end marker at frame %d", i)
		}
		if i > 0 && timestamp <= frames[i-1].timestamp {
			return result, fmt.Errorf("non-increasing BIF timestamp at frame %d", i)
		}
		if i > 0 && int64(imageOffset) <= frames[i-1].offset {
			return result, fmt.Errorf("non-increasing BIF image offset at frame %d", i)
		}
		frames[i] = bifFrame{timestamp: timestamp, offset: int64(imageOffset)}
		if i > 0 {
			frames[i-1].size = int64(imageOffset) - frames[i-1].offset
		}
	}
	intervalMs := uint64(multiplier)
	if len(frames) > 1 {
		intervalMs *= uint64(frames[1].timestamp - frames[0].timestamp)
	}
	if intervalMs == 0 || intervalMs > uint64(math.MaxInt) {
		return result, errors.New("invalid BIF timestamp interval")
	}
	result.frames = frames
	result.intervalMs = int(intervalMs)
	return result, nil
}

func (h *Handler) serveMediaTrickplayPlaylist(w http.ResponseWriter, r *http.Request, locator token.Locator, requestedWidth int) {
	trickplay, err := h.loadMediaBIF(locator, requestedWidth)
	if err != nil {
		h.logger.Error("load Jellyscrub trickplay failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "trickplay_failed", "Unable to read trickplay")
		return
	}
	if trickplay == nil {
		h.writeError(w, http.StatusNotFound, "trickplay_not_available", "Trickplay is not available")
		return
	}
	interval := float64(trickplay.intervalMs) / 1000
	targetDuration := int(math.Ceil(interval))
	playlist := strings.Builder{}
	fmt.Fprintf(&playlist, "#EXTM3U\n#EXT-X-TARGETDURATION:%d\n#EXT-X-VERSION:7\n#EXT-X-MEDIA-SEQUENCE:1\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-IMAGES-ONLY\n", targetDuration)
	for index := range trickplay.frames {
		fmt.Fprintf(&playlist, "#EXTINF:%s,\n", strconv.FormatFloat(interval, 'f', 3, 64))
		fmt.Fprintf(&playlist, "#EXT-X-TILES:RESOLUTION=%dx%d,LAYOUT=1x1,DURATION=%s\n",
			trickplay.thumbWidth, trickplay.thumbHeight, strconv.FormatFloat(interval, 'f', 3, 64))
		fmt.Fprintf(&playlist, "%d/%d.jpg\n", trickplay.width, index)
	}
	playlist.WriteString("#EXT-X-ENDLIST\n")

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "private, max-age=300")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write([]byte(playlist.String()))
}

func (h *Handler) serveMediaTrickplayTile(w http.ResponseWriter, r *http.Request, locator token.Locator, width int, fileName string) {
	index, parseErr := strconv.Atoi(strings.TrimSuffix(fileName, ".jpg"))
	if parseErr != nil || index < 0 {
		h.writeError(w, http.StatusNotFound, "trickplay_not_available", "Trickplay is not available")
		return
	}
	trickplay, err := h.loadMediaBIF(locator, width)
	if err != nil {
		h.logger.Error("load Jellyscrub trickplay tile failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "trickplay_failed", "Unable to read trickplay")
		return
	}
	if trickplay == nil || index >= len(trickplay.frames) {
		h.writeError(w, http.StatusNotFound, "trickplay_not_available", "Trickplay is not available")
		return
	}
	file, err := h.resolver.Open(locator.RootID, trickplay.relativePath)
	if errors.Is(err, os.ErrNotExist) {
		h.writeError(w, http.StatusNotFound, "trickplay_not_available", "Trickplay is not available")
		return
	}
	if err != nil {
		h.logger.Error("open Jellyscrub trickplay tile failed", "error", err)
		h.writeError(w, http.StatusInternalServerError, "trickplay_failed", "Unable to read trickplay")
		return
	}
	defer file.Close()
	frame := trickplay.frames[index]
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeContent(w, r, fileName, trickplay.modTime, io.NewSectionReader(file, frame.offset, frame.size))
}

func readTrickplayIndex(directory string) (trickplayIndex, error) {
	var index trickplayIndex
	data, err := os.ReadFile(filepath.Join(directory, "index.json"))
	if err != nil {
		return index, err
	}
	if err := json.Unmarshal(data, &index); err != nil {
		return index, err
	}
	if index.IntervalMs <= 0 || index.ThumbWidth <= 0 || index.ThumbHeight <= 0 ||
		index.Columns <= 0 || index.Rows <= 0 || index.Count <= 0 || index.Sheets <= 0 ||
		index.Count > index.Columns*index.Rows*index.Sheets {
		return trickplayIndex{}, errors.New("invalid trickplay index")
	}
	return index, nil
}

func (h *Handler) serveTrickplayPlaylist(w http.ResponseWriter, r *http.Request, directory, requestedWidth string) {
	index, err := readTrickplayIndex(directory)
	if err != nil || (requestedWidth != "" && requestedWidth != strconv.Itoa(index.ThumbWidth)) {
		h.writeError(w, http.StatusNotFound, "trickplay_not_available", "Trickplay is not available")
		return
	}
	interval := float64(index.IntervalMs) / 1000
	perTile := index.Columns * index.Rows
	playlist := strings.Builder{}
	tileCount := (index.Count + perTile - 1) / perTile
	targetDuration := int(math.Ceil(interval * float64(perTile)))
	fmt.Fprintf(&playlist, "#EXTM3U\n#EXT-X-TARGETDURATION:%d\n#EXT-X-VERSION:7\n#EXT-X-MEDIA-SEQUENCE:1\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXT-X-IMAGES-ONLY\n", targetDuration)
	imagePrefix := ""
	if requestedWidth == "" {
		imagePrefix = strconv.Itoa(index.ThumbWidth) + "/"
	}
	for tile := 0; tile < tileCount; tile++ {
		thumbnails := perTile
		if tile == tileCount-1 {
			thumbnails = index.Count - tile*perTile
		}
		duration := interval * float64(thumbnails)
		fmt.Fprintf(&playlist, "#EXTINF:%s,\n", strconv.FormatFloat(duration, 'f', 3, 64))
		fmt.Fprintf(&playlist, "#EXT-X-TILES:RESOLUTION=%dx%d,LAYOUT=%dx%d,DURATION=%s\n",
			index.ThumbWidth, index.ThumbHeight, index.Columns, index.Rows, strconv.FormatFloat(interval, 'f', 3, 64))
		fmt.Fprintf(&playlist, "%s%d.jpg\n", imagePrefix, tile)
	}
	playlist.WriteString("#EXT-X-ENDLIST\n")

	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "private, max-age=300")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write([]byte(playlist.String()))
}
