package transcoder

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Manager struct {
	c *Config

	path     string
	tempDir  string
	id       string
	close    chan string
	inactive int

	probe     *ProbeVideoData
	numChunks int

	streams map[string]*Stream
}

type ProbeVideoData struct {
	Width     int
	Height    int
	Duration  time.Duration
	FrameRate int
	CodecName string
	BitRate   int
	Rotation  int
}

func NewManager(c *Config, path string, id string, close chan string) (*Manager, error) {
	m := &Manager{c: c, path: path, id: id, close: close}
	m.streams = make(map[string]*Stream)

	h := fnv.New32a()
	h.Write([]byte(path))
	ph := fmt.Sprint(h.Sum32())
	m.tempDir = fmt.Sprintf("%s/%s-%s", m.c.TempDir, id, ph)

	// Delete temp dir if exists
	os.RemoveAll(m.tempDir)
	os.MkdirAll(m.tempDir, 0755)

	if err := m.ffprobe(); err != nil {
		return nil, err
	}

	m.numChunks = int(math.Ceil(m.probe.Duration.Seconds() / float64(c.ChunkSize)))

	// Possible streams (bitrates in bps for proper HLS BANDWIDTH reporting)
	// Add extra low-bandwidth options for TV browsers and limited devices
	if m.c.LowBandwidthMode {
		m.streams["360p"] = &Stream{c: c, m: m, quality: "360p", height: 360, width: 640, bitrate: 500000}  // Ultra-low for TV
	}
	m.streams["480p"] = &Stream{c: c, m: m, quality: "480p", height: 480, width: 854, bitrate: 800000}
	m.streams["720p"] = &Stream{c: c, m: m, quality: "720p", height: 720, width: 1280, bitrate: 1500000}
	m.streams["1080p"] = &Stream{c: c, m: m, quality: "1080p", height: 1080, width: 1920, bitrate: 3000000}
	
	// Skip high res for low bandwidth mode
	if !m.c.LowBandwidthMode {
		m.streams["1440p"] = &Stream{c: c, m: m, quality: "1440p", height: 1440, width: 2560, bitrate: 6000000}
	}

	// height is our primary dimension for scaling
	// using the probed size, we adjust the width of the stream
	// the smaller dimemension of the output should match the height here
	smDim, lgDim := m.probe.Height, m.probe.Width
	if m.probe.Height > m.probe.Width {
		smDim, lgDim = lgDim, smDim
	}

	// Get the reference bitrate. This is the same as the current bitrate
	// if the video is H.264, otherwise use double the current bitrate.
	refBitrate := int(float64(m.probe.BitRate) / 2.0)
	if m.probe.CodecName != CODEC_H264 {
		refBitrate *= 2
	}

	// If bitrate could not be read, use 10Mbps
	if refBitrate == 0 {
		refBitrate = 10000000
	}
	
	// For very high bitrate content (>50Mbps), adjust quality targets
	isHighBitrate := m.probe.BitRate > 50000000
	if isHighBitrate {
		log.Printf("%s: detected high bitrate content (%d bps), optimizing settings", m.id, m.probe.BitRate)
		// Increase reference bitrate for high bitrate sources to maintain quality
		refBitrate = int(float64(refBitrate) * 1.2)
	}

	// Get the multiplier for the reference bitrate.
	// For this get the nearest stream size to the original.
	origPixels := float64(m.probe.Height * m.probe.Width)
	nearestPixels := float64(0)
	nearestStream := ""
	for key, stream := range m.streams {
		streamPixels := float64(stream.height * stream.width)
		if nearestPixels == 0 || math.Abs(origPixels-streamPixels) < math.Abs(origPixels-nearestPixels) {
			nearestPixels = streamPixels
			nearestStream = key
		}
	}

	// Get the bitrate multiplier. This is the ratio of the reference
	// bitrate to the nearest stream bitrate, so we can scale all streams.
	bitrateMultiplier := 1.0
	if nearestStream != "" {
		bitrateMultiplier = float64(refBitrate) / float64(m.streams[nearestStream].bitrate)
	}

	// Only keep streams that are smaller than the video
	for k, stream := range m.streams {
		stream.order = 0

		// scale bitrate using the multiplier
		stream.bitrate = int(math.Ceil(float64(stream.bitrate) * bitrateMultiplier))

		// Calculate proper width maintaining aspect ratio
		// Account for rotation metadata
		sourceWidth := m.probe.Width
		sourceHeight := m.probe.Height
		
		// Handle rotation metadata (90/-90 degree rotations swap dimensions)
		if m.probe.Rotation == 90 || m.probe.Rotation == -90 || m.probe.Rotation == 270 {
			sourceWidth, sourceHeight = sourceHeight, sourceWidth
		}
		
		aspectRatio := float64(sourceWidth) / float64(sourceHeight)
		stream.width = int(math.Ceil(float64(stream.height) * aspectRatio))
		
		log.Printf("%s: stream %s - source: %dx%d (rotation: %d), target: %dx%d, aspect: %.2f", 
			m.id, k, m.probe.Width, m.probe.Height, m.probe.Rotation, stream.width, stream.height, aspectRatio)
		
		// Ensure even dimensions for encoder compatibility
		if stream.width%2 != 0 {
			stream.width++
		}
		if stream.height%2 != 0 {
			stream.height++
		}

		// Special handling for square or unusual aspect ratios
		isSquareish := math.Abs(aspectRatio-1.0) < 0.1 // within 10% of square
		if isSquareish {
			log.Printf("%s: detected square/unusual aspect ratio %.2f for %s", m.id, aspectRatio, k)
		}

		// remove invalid streams
		if (stream.height >= smDim || stream.width >= lgDim) || // no upscaling; we're not AI
			(float64(stream.bitrate) > float64(m.probe.BitRate)*0.8) || // no more than 80% of original bitrate
			(stream.width < 64 || stream.height < 64) { // minimum sane dimensions

			// remove stream
			delete(m.streams, k)
			continue
		}
	}

	// Original stream
	m.streams[QUALITY_MAX] = &Stream{
		c: c, m: m,
		quality: QUALITY_MAX,
		height:  m.probe.Height,
		width:   m.probe.Width,
		bitrate: refBitrate,
		order:   1,
	}

	// Start all streams with concurrent management
	streamCount := len(m.streams)
	log.Printf("%s: starting %d streams with max %d concurrent transcodes", m.id, streamCount, m.c.MaxConcurrentTranscodes)
	
	for _, stream := range m.streams {
		go stream.Run()
	}

	log.Printf("%s: new manager for %s", m.id, m.path)

	// Check for inactivity
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			<-t.C

			if m.inactive == -1 {
				t.Stop()
				return
			}

			m.inactive++

			// Check if any stream is active
			for _, stream := range m.streams {
				if stream.coder != nil {
					m.inactive = 0
					break
				}
			}

			// Nothing done for 5 minutes
			if m.inactive >= m.c.ManagerIdleTime/5 {
				t.Stop()
				m.Destroy()
				m.close <- m.id
				return
			}
		}
	}()

	return m, nil
}

// Destroys streams. DOES NOT emit on the close channel.
func (m *Manager) Destroy() {
	log.Printf("%s: destroying manager", m.id)
	m.inactive = -1

	for _, stream := range m.streams {
		stream.Stop()
	}

	// Delete temp dir
	os.RemoveAll(m.tempDir)

	// Delete file if temp
	freeIfTemp(m.path)
}

func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request, chunk string) error {
	// Master list
	if chunk == "index.m3u8" {
		return m.ServeIndex(w, r)
	}

	// Stream list
	m3u8Sfx := ".m3u8"
	if strings.HasSuffix(chunk, m3u8Sfx) {
		quality := strings.TrimSuffix(chunk, m3u8Sfx)
		if stream, ok := m.streams[quality]; ok {
			return stream.ServeList(w, r)
		}
	}

	// Stream chunk (support both TS and MP4)
	tsSfx := ".ts"
	mp4Sfx := ".mp4"
	isChunk := strings.HasSuffix(chunk, tsSfx) || strings.HasSuffix(chunk, mp4Sfx)
	
	if isChunk {
		parts := strings.Split(chunk, "-")
		if len(parts) != 2 {
			w.WriteHeader(http.StatusBadRequest)
			return nil
		}

		quality := parts[0]
		var chunkIdStr string
		if strings.HasSuffix(chunk, tsSfx) {
			chunkIdStr = strings.TrimSuffix(parts[1], tsSfx)
		} else {
			chunkIdStr = strings.TrimSuffix(parts[1], mp4Sfx)
		}
		
		chunkId, err := strconv.Atoi(chunkIdStr)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return nil
		}

		if stream, ok := m.streams[quality]; ok {
			return stream.ServeChunk(w, chunkId)
		}
	}

	// Stream full video
	if strings.HasSuffix(chunk, ".mp4") {
		quality := strings.TrimSuffix(chunk, ".mp4")
		if stream, ok := m.streams[quality]; ok {
			return stream.ServeFullVideo(w, r)
		}

		// Fall back to original
		return m.streams[QUALITY_MAX].ServeFullVideo(w, r)
	}

	w.WriteHeader(http.StatusNotFound)
	return nil
}

func (m *Manager) ServeIndex(w http.ResponseWriter, r *http.Request) error {
	WriteM3U8ContentType(w)
	w.Write([]byte("#EXTM3U\n"))

	// get sorted streams by bitrate
	streams := make([]*Stream, 0)
	for _, stream := range m.streams {
		streams = append(streams, stream)
	}
	sort.Slice(streams, func(i, j int) bool {
		return streams[i].order < streams[j].order ||
			(streams[i].order == streams[j].order && streams[i].bitrate < streams[j].bitrate)
	})

	// Write all streams with enhanced ABR information
	query := GetQueryString(r)
	
	// Client capability detection
	clientHints := ""
	if m.c.EnableClientHints {
		userAgent := r.Header.Get("User-Agent")
		clientHints = m.analyzeClientCapabilities(userAgent)
	}
	
	for _, stream := range streams {
		// Calculate average bandwidth (slightly lower than peak for better ABR decisions)
		avgBandwidth := int(float64(stream.bitrate) * 0.85)
		
		// Enhanced HLS stream info for better client decision making
		streamInfo := fmt.Sprintf("#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d,RESOLUTION=%dx%d,FRAME-RATE=%d,CODECS=\"avc1.42E01E,mp4a.40.2\"", 
			stream.bitrate, avgBandwidth, stream.width, stream.height, m.probe.FrameRate)
		
		// Add client-specific hints if available
		if clientHints != "" {
			streamInfo += "," + clientHints
		}
		
		streamInfo += fmt.Sprintf("\n%s.m3u8%s\n", stream.quality, query)
		w.Write([]byte(streamInfo))
	}
	return nil
}

// Analyze client capabilities based on User-Agent and other headers
func (m *Manager) analyzeClientCapabilities(userAgent string) string {
	hints := make([]string, 0)
	ua := strings.ToLower(userAgent)
	
	// Detailed browser detection for format selection
	browserInfo := m.detectBrowserCapabilities(ua)
	
	// Device type detection with more specificity
	if strings.Contains(ua, "tv") || strings.Contains(ua, "smarttv") || strings.Contains(ua, "roku") || strings.Contains(ua, "chromecast") {
		hints = append(hints, "DEVICE-TYPE=\"tv\"")
		hints = append(hints, "PREFERRED-MAX-BANDWIDTH=4000000") // Conservative for TV browsers
		m.setLowBandwidthMode(true) // Enable low bandwidth mode for TV
	} else if strings.Contains(ua, "mobile") || strings.Contains(ua, "android") || strings.Contains(ua, "iphone") {
		hints = append(hints, "DEVICE-TYPE=\"mobile\"")
		hints = append(hints, "PREFERRED-MAX-BANDWIDTH=2000000")
	} else if strings.Contains(ua, "tablet") || strings.Contains(ua, "ipad") {
		hints = append(hints, "DEVICE-TYPE=\"tablet\"")
		hints = append(hints, "PREFERRED-MAX-BANDWIDTH=4000000")
	} else {
		hints = append(hints, "DEVICE-TYPE=\"desktop\"")
		hints = append(hints, "PREFERRED-MAX-BANDWIDTH=8000000")
	}
	
	// Add browser capabilities
	hints = append(hints, browserInfo...)
	
	// Hardware acceleration hints
	if strings.Contains(ua, "arm64") || strings.Contains(ua, "aarch64") {
		hints = append(hints, "ARCH=\"arm64\"")
	} else if strings.Contains(ua, "x86_64") || strings.Contains(ua, "amd64") {
		hints = append(hints, "ARCH=\"x86_64\"")
	}
	
	return strings.Join(hints, ",")
}

// Detect browser capabilities for format selection
func (m *Manager) detectBrowserCapabilities(ua string) []string {
	caps := make([]string, 0)
	
	// Chrome family (supports fMP4, latest HLS)
	if strings.Contains(ua, "chrome") && !strings.Contains(ua, "edg") {
		caps = append(caps, "BROWSER=\"chrome\"")
		caps = append(caps, "SUPPORTS-FMP4=true")
		caps = append(caps, "HLS-VERSION=6")
	} else if strings.Contains(ua, "edg") {
		// Edge (Chromium-based)
		caps = append(caps, "BROWSER=\"edge\"")
		caps = append(caps, "SUPPORTS-FMP4=true")
		caps = append(caps, "HLS-VERSION=6")
	} else if strings.Contains(ua, "firefox") {
		// Firefox (good HLS support, prefers TS)
		caps = append(caps, "BROWSER=\"firefox\"")
		caps = append(caps, "SUPPORTS-FMP4=false") // Prefer TS for Firefox
		caps = append(caps, "HLS-VERSION=4")
	} else if strings.Contains(ua, "safari") {
		// Safari (native HLS support, all formats)
		caps = append(caps, "BROWSER=\"safari\"")
		caps = append(caps, "SUPPORTS-FMP4=true")
		caps = append(caps, "HLS-VERSION=6")
		caps = append(caps, "NATIVE-HLS=true")
	} else if strings.Contains(ua, "brave") {
		// Brave (Chromium-based)
		caps = append(caps, "BROWSER=\"brave\"")
		caps = append(caps, "SUPPORTS-FMP4=true")
		caps = append(caps, "HLS-VERSION=6")
	} else if strings.Contains(ua, "opera") {
		// Opera (Chromium-based)
		caps = append(caps, "BROWSER=\"opera\"")
		caps = append(caps, "SUPPORTS-FMP4=true")
		caps = append(caps, "HLS-VERSION=5")
	} else {
		// Unknown browser - use conservative settings
		caps = append(caps, "BROWSER=\"unknown\"")
		caps = append(caps, "SUPPORTS-FMP4=false")
		caps = append(caps, "HLS-VERSION=3")
		m.setForceCompatibility(true)
	}
	
	// Android WebView detection (often problematic)
	if strings.Contains(ua, "android") && strings.Contains(ua, "wv") {
		caps = append(caps, "WEBVIEW=true")
		caps = append(caps, "SUPPORTS-FMP4=false") // WebView often has issues with fMP4
		caps = append(caps, "HLS-VERSION=3")
	}
	
	return caps
}

// Helper functions for dynamic configuration
func (m *Manager) setLowBandwidthMode(enabled bool) {
	if m.c.LowBandwidthMode != enabled {
		log.Printf("%s: setting low bandwidth mode: %v", m.id, enabled)
		m.c.LowBandwidthMode = enabled
	}
}

func (m *Manager) setForceCompatibility(enabled bool) {
	if m.c.ForceCompatibility != enabled {
		log.Printf("%s: setting force compatibility mode: %v", m.id, enabled)
		m.c.ForceCompatibility = enabled
	}
}

func (m *Manager) ffprobe() error {
	args := []string{
		// Hide debug information
		"-v", "error",

		// Show everything
		"-show_entries", "format:stream",
		"-select_streams", "v", // Video stream only, we're not interested in audio

		"-of", "json",
		m.path,
	}

	ctx, cancel := context.WithDeadline(context.TODO(), time.Now().Add(5*time.Second))
	defer cancel()
	cmd := exec.CommandContext(ctx, m.c.FFprobe, args...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		log.Println(stderr.String())
		return err
	}

	out := struct {
		Streams []struct {
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			Duration     string `json:"duration"`
			FrameRate    string `json:"avg_frame_rate"`
			CodecName    string `json:"codec_name"`
			BitRate      string `json:"bit_rate"`
			SideDataList []struct {
				SideDataType string `json:"side_data_type"`
				Rotation     int    `json:"rotation"`
			} `json:"side_data_list"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}{}

	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return err
	}

	if len(out.Streams) == 0 {
		return errors.New("no video streams found")
	}

	var duration time.Duration
	if out.Streams[0].Duration != "" {
		duration, _ = time.ParseDuration(out.Streams[0].Duration + "s")
	} else if out.Format.Duration != "" {
		duration, _ = time.ParseDuration(out.Format.Duration + "s")
	}

	// FrameRate is a fraction string
	frac := strings.Split(out.Streams[0].FrameRate, "/")
	if len(frac) != 2 {
		frac = []string{"30", "1"}
	}
	num, e1 := strconv.Atoi(frac[0])
	den, e2 := strconv.Atoi(frac[1])
	if e1 != nil || e2 != nil {
		num = 30
		den = 1
	}
	frameRate := float64(num) / float64(den)

	// BitRate is a string
	bitRate, err := strconv.Atoi(out.Streams[0].BitRate)
	if err != nil {
		bitRate = 5000000
	}

	// Get rotation from side data
	rotation := 0
	for _, sideData := range out.Streams[0].SideDataList {
		if sideData.SideDataType == "Display Matrix" {
			rotation = sideData.Rotation
		}
	}

	m.probe = &ProbeVideoData{
		Width:     out.Streams[0].Width,
		Height:    out.Streams[0].Height,
		Duration:  duration,
		FrameRate: int(frameRate),
		CodecName: out.Streams[0].CodecName,
		BitRate:   bitRate,
		Rotation:  rotation,
	}

	return nil
}
