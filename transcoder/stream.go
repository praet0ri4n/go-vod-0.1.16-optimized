package transcoder

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	ENCODER_COPY  = "copy"
	ENCODER_X264  = "libx264"
	ENCODER_VAAPI = "h264_vaapi"
	ENCODER_NVENC = "h264_nvenc"

	QUALITY_MAX = "max"
	CODEC_H264  = "h264"
)

type Stream struct {
	c       *Config
	m       *Manager
	quality string
	order   int
	height  int
	width   int
	bitrate int

	goal int

	mutex      sync.Mutex
	chunks     map[int]*Chunk
	seenChunks map[int]bool // only for stdout reader

	coder *exec.Cmd

	inactive int
	stop     chan bool
}

func (s *Stream) Run() {
	// run every 5s
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()

	s.stop = make(chan bool)

	for {
		select {
		case <-t.C:
			s.mutex.Lock()
			// Prune chunks
			for id := range s.chunks {
				if id < s.goal-s.c.GoalBufferMax {
					s.pruneChunk(id)
				}
			}

			s.inactive++

			// Nothing done for 2 minutes
			if s.inactive >= s.c.StreamIdleTime/5 && s.coder != nil {
				t.Stop()
				s.clear()
			}
			s.mutex.Unlock()

		case <-s.stop:
			t.Stop()
			s.mutex.Lock()
			s.clear()
			s.mutex.Unlock()
			return
		}
	}
}

func (s *Stream) clear() {
	log.Printf("%s-%s: stopping stream", s.m.id, s.quality)

	for _, chunk := range s.chunks {
		// Delete files
		s.pruneChunk(chunk.id)
	}

	s.chunks = make(map[int]*Chunk)
	s.seenChunks = make(map[int]bool)
	s.goal = 0

	if s.coder != nil {
		s.coder.Process.Kill()
		s.coder.Wait()
		s.coder = nil
	}
}

func (s *Stream) Stop() {
	select {
	case s.stop <- true:
	default:
	}
}

func (s *Stream) ServeList(w http.ResponseWriter, r *http.Request) error {
	WriteM3U8ContentType(w)
	w.Write([]byte("#EXTM3U\n"))
	
	// Adaptive HLS version based on client capabilities
	hlsVersion := s.c.HLSVersion
	segmentExt := "ts"
	
	// Check for client-specific requirements
	userAgent := r.Header.Get("User-Agent")
	ua := strings.ToLower(userAgent)
	
	// Determine optimal format based on client
	useMP4 := s.c.EnableFMP4 && !s.c.ForceCompatibility
	
	// Force TS for problematic browsers/devices
	if strings.Contains(ua, "firefox") || 
	   strings.Contains(ua, "tv") || 
	   strings.Contains(ua, "webview") ||
	   s.c.LowBandwidthMode {
		useMP4 = false
		hlsVersion = 3 // More compatible version
	}
	
	// Chrome/Safari can use fMP4 for better efficiency
	if useMP4 && (strings.Contains(ua, "chrome") || strings.Contains(ua, "safari") || strings.Contains(ua, "edge")) {
		segmentExt = "mp4"
		hlsVersion = 6
	}
	
	w.Write([]byte(fmt.Sprintf("#EXT-X-VERSION:%d\n", hlsVersion)))
	
	// Add compatibility headers
	if hlsVersion >= 6 && useMP4 {
		w.Write([]byte("#EXT-X-INDEPENDENT-SEGMENTS\n"))
	}
	
	w.Write([]byte("#EXT-X-MEDIA-SEQUENCE:0\n"))
	w.Write([]byte("#EXT-X-PLAYLIST-TYPE:VOD\n"))
	w.Write([]byte(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", s.c.ChunkSize)))

	query := GetQueryString(r)

	duration := s.m.probe.Duration.Seconds()
	i := 0
	for duration > 0 {
		size := float64(s.c.ChunkSize)
		if duration < size {
			size = duration
		}

		w.Write([]byte(fmt.Sprintf("#EXTINF:%.3f,\n", size)))
		w.Write([]byte(fmt.Sprintf("%s-%06d.%s%s\n", s.quality, i, segmentExt, query)))

		duration -= float64(s.c.ChunkSize)
		i++
	}

	w.Write([]byte("#EXT-X-ENDLIST\n"))

	return nil
}

func (s *Stream) ServeChunk(w http.ResponseWriter, id int) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.inactive = 0
	s.checkGoal(id)

	// Already have this chunk
	if chunk, ok := s.chunks[id]; ok {
		// Chunk is finished, just return it
		if chunk.done {
			s.returnChunk(w, chunk)
			return nil
		}

		// Still waiting on transcoder
		s.waitForChunk(w, chunk)
		return nil
	}

	// Will have this soon enough
	foundBehind := false
	for i := id - 1; i > id-s.c.LookBehind && i >= 0; i-- {
		if _, ok := s.chunks[i]; ok {
			foundBehind = true
		}
	}
	if foundBehind {
		// Make sure the chunk exists
		chunk := s.createChunk(id)

		// Wait for it
		s.waitForChunk(w, chunk)
		return nil
	}

	// Let's start over
	s.restartAtChunk(w, id)
	return nil
}

func (s *Stream) ServeFullVideo(w http.ResponseWriter, r *http.Request) error {
	args := s.transcodeArgs(0, false)

	if s.m.probe.CodecName == CODEC_H264 && s.quality == QUALITY_MAX {
		// try to just send the original file
		http.ServeFile(w, r, s.m.path)
		return nil
	}

	// Output mov
	args = append(args, []string{
		"-movflags", "frag_keyframe+empty_moov+faststart",
		"-f", "mp4", "pipe:1",
	}...)

	coder := exec.Command(s.c.FFmpeg, args...)
	log.Printf("%s-%s: %s", s.m.id, s.quality, strings.Join(coder.Args[:], " "))

	cmdStdOut, err := coder.StdoutPipe()
	if err != nil {
		log.Printf("FATAL: ffmpeg command stdout failed with %s\n", err)
	}

	cmdStdErr, err := coder.StderrPipe()
	if err != nil {
		log.Printf("FATAL: ffmpeg command stdout failed with %s\n", err)
	}

	err = coder.Start()
	if err != nil {
		log.Printf("FATAL: ffmpeg command failed with %s\n", err)
	}
	go s.monitorStderr(cmdStdErr)

	// Write to response
	defer cmdStdOut.Close()
	stdoutReader := bufio.NewReader(cmdStdOut)

	// Write mov headers
	w.Header().Set("Content-Type", "video/mp4")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Server does not support Flusher!",
			http.StatusInternalServerError)
		return nil
	}

	// Write data, flusing every 1MB
	buf := make([]byte, 1024*1024)
	for {
		n, err := stdoutReader.Read(buf)
		if err != nil {
			if err == io.EOF {
				break
			}
			log.Printf("FATAL: ffmpeg command failed with %s\n", err)
			break
		}

		_, err = w.Write(buf[:n])
		if err != nil {
			log.Printf("%s-%s: client closed connection", s.m.id, s.quality)
			log.Println(err)
			break
		}
		flusher.Flush()
	}

	// Terminate ffmpeg process
	coder.Process.Kill()
	coder.Wait()

	return nil
}

func (s *Stream) createChunk(id int) *Chunk {
	if c, ok := s.chunks[id]; ok {
		return c
	} else {
		s.chunks[id] = NewChunk(id)
		return s.chunks[id]
	}
}

func (s *Stream) pruneChunk(id int) {
	delete(s.chunks, id)

	// Remove file
	filename := s.getTsPath(id)
	os.Remove(filename)
}

func (s *Stream) returnChunk(w http.ResponseWriter, chunk *Chunk) {
	// This function is called with lock, but we don't need it
	s.mutex.Unlock()
	defer s.mutex.Lock()

	// Read file and write to response (support both TS and MP4)
	filename := s.getChunkPath(chunk.id)
	
	// Use memory mapping for large files if enabled
	if s.c.EnableMemoryMapping {
		if info, err := os.Stat(filename); err == nil && info.Size() > 1024*1024 { // >1MB
			// TODO: Implement memory mapping for very large chunks
		}
	}
	
	f, err := os.Open(filename)
	if err != nil {
		log.Println(err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	defer f.Close()
	
	// Set appropriate content type based on file extension
	if strings.HasSuffix(filename, ".mp4") {
		w.Header().Set("Content-Type", "video/mp4")
	} else {
		w.Header().Set("Content-Type", "video/MP2T")
	}
	
	// Dynamic buffer size based on configuration and content
	bufferSize := s.c.ChunkBufferSize * 1024 // Convert KB to bytes
	if bufferSize < 32*1024 {
		bufferSize = 32 * 1024 // Minimum 32KB
	}
	if bufferSize > 512*1024 {
		bufferSize = 512 * 1024 // Maximum 512KB
	}
	
	buf := make([]byte, bufferSize)
	_, err = io.CopyBuffer(w, f, buf)
	if err != nil {
		log.Printf("%s-%s: error serving chunk %d: %v", s.m.id, s.quality, chunk.id, err)
	}
}

func (s *Stream) waitForChunk(w http.ResponseWriter, chunk *Chunk) {
	if chunk.done {
		s.returnChunk(w, chunk)
		return
	}

	// Add our channel
	notif := make(chan bool)
	chunk.notifs = append(chunk.notifs, notif)
	t := time.NewTimer(30 * time.Second)  // Increased for high bitrate content
	coder := s.coder

	s.mutex.Unlock()

	select {
	case <-notif:
		t.Stop()
	case <-t.C:
	}

	s.mutex.Lock()

	// remove channel
	for i, c := range chunk.notifs {
		if c == notif {
			chunk.notifs = append(chunk.notifs[:i], chunk.notifs[i+1:]...)
			break
		}
	}

	// check for success
	if chunk.done {
		s.returnChunk(w, chunk)
		return
	}

	// Check if coder was changed
	if coder != s.coder {
		w.WriteHeader(http.StatusConflict)
		return
	}

	// Return timeout error
	w.WriteHeader(http.StatusRequestTimeout)
}

func (s *Stream) restartAtChunk(w http.ResponseWriter, id int) {
	// Stop current transcoder
	s.clear()

	chunk := s.createChunk(id) // create first chunk

	// Start the transcoder
	s.goal = id + s.c.GoalBufferMax
	s.transcode(id)

	s.waitForChunk(w, chunk) // this is also a request
}

// Get arguments to ffmpeg
func (s *Stream) transcodeArgs(startAt float64, isHls bool) []string {
	args := []string{
		"-loglevel", "warning",
	}

	if startAt > 0 {
		args = append(args, []string{
			"-ss", fmt.Sprintf("%.6f", startAt),
		}...)
	}

	// encoder selection with hardware optimization
	CV := ENCODER_X264

	// Check whether hwaccel should be used
	if s.c.VAAPI {
		CV = ENCODER_VAAPI
		extra := "-hwaccel vaapi -hwaccel_device /dev/dri/renderD128 -hwaccel_output_format vaapi"
		args = append(args, strings.Split(extra, " ")...)
	} else if s.c.NVENC {
		CV = ENCODER_NVENC
		// Enhanced CUDA acceleration with device selection
		extra := fmt.Sprintf("-hwaccel cuda -hwaccel_device %d", s.c.CUDADevice)
		
		// Don't use hwaccel_output_format cuda - it causes filter chain issues
		// Let frames stay in system memory and use hwupload_cuda in the filter
		
		args = append(args, strings.Split(extra, " ")...)
	}

	// Disable autorotation (see transpose comments below)
	if s.c.UseTranspose {
		args = append(args, []string{"-noautorotate"}...)
	}

	// Input specs
	args = append(args, []string{
		"-i", s.m.path, // Input file
		"-copyts", // So the "-to" refers to the original TS
		"-fflags", "+genpts",
	}...)

	// Filters
	format := "format=nv12"
	scaler := "scale"
	scalerArgs := make([]string, 0)
	scalerArgs = append(scalerArgs, "force_original_aspect_ratio=decrease")

	if CV == ENCODER_VAAPI {
		format = "format=nv12|vaapi,hwupload"
		scaler = "scale_vaapi"
		scalerArgs = append(scalerArgs, "format=nv12")
	} else if CV == ENCODER_NVENC {
		// Use NPP for better performance (your FFmpeg supports it)
		format = "format=nv12,hwupload_cuda"
		
		// Use appropriate scaler based on NVENCScale setting
		if s.c.NVENCScale == "npp" {
			scaler = "scale_npp"
		} else if s.c.NVENCScale == "cuda" {
			scaler = "scale_cuda"
			// workaround to force scale_cuda to examine all input frames
			scalerArgs = append(scalerArgs, "passthrough=0")
		} else {
			// fallback to basic scale
			scaler = "scale"
		}
	}

	// Scale height and width if not max quality
	if s.quality != QUALITY_MAX {
		// Proper aspect ratio scaling - avoid creating squares!
		scalerArgs = append(scalerArgs, fmt.Sprintf("w=%d", s.width))
		scalerArgs = append(scalerArgs, fmt.Sprintf("h=%d", s.height))
	}

	// Apply filter
	if CV != ENCODER_COPY {
		filter := fmt.Sprintf("%s,%s=%s", format, scaler, strings.Join(scalerArgs, ":"))

		// Rotation is a mess: https://trac.ffmpeg.org/ticket/8329
		//   1/ -noautorotate copies the sidecar metadata to the output
		//   2/ autorotation doesn't seem to work with some types of HW (at least not with VAAPI)
		//   3/ autorotation doesn't work with HLS streams
		//   4/ VAAPI cannot transport on AMD GPUs
		// So: give the user to disable autorotation for HLS and use a manual transpose
		if isHls && s.c.UseTranspose {
			transposer := "transpose"
			if CV == ENCODER_VAAPI {
				transposer = "transpose_vaapi"
			} else if CV == ENCODER_NVENC {
				transposer = fmt.Sprintf("transpose_%s", s.c.NVENCScale)
			}

			if transposer != "transpose_cuda" { // does not exist
				if s.m.probe.Rotation == -90 {
					filter = fmt.Sprintf("%s,%s=1", filter, transposer)
				} else if s.m.probe.Rotation == 90 {
					filter = fmt.Sprintf("%s,%s=2", filter, transposer)
				} else if s.m.probe.Rotation == 180 || s.m.probe.Rotation == -180 {
					filter = fmt.Sprintf("%s,%s=1,%s=1", filter, transposer, transposer)
				}
			}
		}

		args = append(args, []string{"-vf", filter}...)
	}

	// Output specs for video
	args = append(args, []string{
		"-map", "0:v:0",
		"-c:v", CV,
	}...)

	// Device specific output args
	if CV == ENCODER_VAAPI {
		args = append(args, []string{"-global_quality", fmt.Sprintf("%d", s.c.QF)}...)

		if s.c.VAAPILowPower {
			args = append(args, []string{"-low_power", "1"}...)
		}
	} else if CV == ENCODER_NVENC {
		// Adaptive encoding complexity based on content and hardware
		preset := "p4"  // Default balanced preset
		tune := "hq"    // Default high quality
		lookahead := 60 // Default lookahead
		
		// Adjust based on content complexity and adaptive settings
		if s.c.AdaptiveComplexity {
			// Special handling for very demanding content
			if s.m.probe.BitRate > 100000000 { // >100Mbps: extremely high complexity
				preset = "p2" // Slowest preset for demanding content
				lookahead = 250 // Maximum lookahead for best prediction
				tune = "hq" // High quality mode
			} else if s.m.probe.BitRate > 50000000 { // >50Mbps: very high complexity
				preset = "p3" // Slower preset for high bitrate
				lookahead = 120 // Extended lookahead
			} else if s.m.probe.BitRate > 20000000 { // >20Mbps: high complexity
				preset = "p4" // Balanced for moderate high bitrate
				lookahead = 80
			}
			
			// Special handling for high framerate content (>30fps)
			if s.m.probe.FrameRate > 30 {
				lookahead = int(float64(lookahead) * 1.5) // Increase lookahead for HFR
				if s.m.probe.FrameRate >= 50 { // 50fps+ (like deinterlaced content)
					preset = "p3" // Use slower preset for very high framerate
					if lookahead > 250 {
						lookahead = 250 // Cap at maximum
					}
				}
			}
			
			// For lower qualities, prioritize speed but not at expense of stability
			if s.quality == "480p" {
				if s.m.probe.BitRate > 50000000 {
					preset = "p5" // Don't go too fast for complex content
					lookahead = 40
				} else {
					preset = "p7" // Fastest for simple low quality
					tune = "ll"   // Low latency
					lookahead = 20
				}
			} else if s.quality == "720p" {
				if s.m.probe.BitRate > 50000000 {
					preset = "p4" // Slower for complex 720p
					lookahead = 60
				} else {
					preset = "p6"
					lookahead = 30
				}
			}
		}
		
		// GPU-specific optimizations
		args = append(args, []string{
			"-gpu", fmt.Sprintf("%d", s.c.CUDADevice),
			"-preset", preset,
			"-tune", tune,
			"-rc", "vbr",
			"-rc-lookahead", fmt.Sprintf("%d", lookahead),
			"-multipass", "fullres",   // Better quality for demanding content
			"-cq", fmt.Sprintf("%d", s.c.QF),
		}...)

		// Advanced NVENC features
		if s.c.NVENCTemporalAQ {
			args = append(args, []string{"-temporal-aq", "1"}...)
		}
		
		// GPU memory management (removed incompatible option)
		// Note: -gpu_memory_limit not supported in all FFmpeg versions

		// Add rate control aligned with advertised bandwidth
		// This prevents encoder overshoot that causes rebuffering
		if s.quality != QUALITY_MAX {
			target := s.bitrate
			maxrate := int(float64(target) * 1.25)  // Allow 25% burst
			bufsize := maxrate * 2                   // 2 second buffer
			args = append(args, []string{
				"-maxrate", fmt.Sprintf("%d", maxrate),
				"-bufsize", fmt.Sprintf("%d", bufsize),
			}...)
		}
	} else if CV == ENCODER_X264 {
		args = append(args, []string{
			"-preset", "faster",
			"-crf", fmt.Sprintf("%d", s.c.QF),
		}...)
	}

	// Audio output specs
	args = append(args, []string{
		"-map", "0:a:0?",
		"-c:a", "aac",
		"-ac", "1",
	}...)

	return args
}

func (s *Stream) transcode(startId int) {
	if startId > 0 {
		// Start one frame before
		// This ensures that the keyframes are aligned
		startId--
	}
	startAt := float64(startId * s.c.ChunkSize)

	args := s.transcodeArgs(startAt, true)

	// Adaptive segmenting specs based on configuration and client support
	segmentType := "mpegts"
	segmentExt := "ts"
	hlsFlags := "split_by_time"  // Use split_by_time only for compatibility
	
	// Use fMP4 for modern browsers if enabled
	if s.c.EnableFMP4 && !s.c.ForceCompatibility {
		segmentType = "fmp4"
		segmentExt = "mp4"
		hlsFlags += "+single_file" // Enable single file mode for fMP4
	}
	
	// Force TS for compatibility mode or low bandwidth
	if s.c.ForceCompatibility || s.c.LowBandwidthMode {
		segmentType = "mpegts"
		segmentExt = "ts"
		hlsFlags = "split_by_time"  // Remove conflicting independent_segments flag
	}
	
	args = append(args, []string{
		"-start_number", fmt.Sprintf("%d", startId),
		"-avoid_negative_ts", "disabled",
		"-f", "hls",
		"-hls_flags", hlsFlags,
		"-hls_time", fmt.Sprintf("%d", s.c.ChunkSize),
		"-hls_segment_type", segmentType,
		"-hls_segment_filename", s.getSegmentPath(-1, segmentExt),
	}...)

	// Keyframe specs - enhanced for complex content
	if s.c.UseGopSize && s.m.probe.FrameRate > 0 {
		// Fix GOP size
		args = append(args, []string{
			"-g", fmt.Sprintf("%d", s.c.ChunkSize*s.m.probe.FrameRate),
			"-keyint_min", fmt.Sprintf("%d", s.c.ChunkSize*s.m.probe.FrameRate),
		}...)
	} else {
		// Force keyframes every chunk - enhanced for edited content
		args = append(args, []string{
			"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", s.c.ChunkSize),
		}...)
		
		// For NVENC, add extra options to handle complex edited content
		if s.c.NVENC {
			args = append(args, []string{
				"-forced-idr", "1",  // Force IDR frames for better seeking
			}...)
		}
	}

	// Output to stdout
	args = append(args, "-")

	// Start the process
	s.coder = exec.Command(s.c.FFmpeg, args...)

	// Log command, quoting the args as needed
	quotedArgs := make([]string, len(s.coder.Args))
	invalidChars := strings.Join([]string{" ", "=", ":", "\"", "\\", "\n", "\t"}, "")
	for i, arg := range s.coder.Args {
		if strings.ContainsAny(arg, invalidChars) {
			quotedArgs[i] = fmt.Sprintf("\"%s\"", arg)
		} else {
			quotedArgs[i] = arg
		}
	}
	log.Printf("%s-%s: %s", s.m.id, s.quality, strings.Join(quotedArgs[:], " "))

	cmdStdOut, err := s.coder.StdoutPipe()
	if err != nil {
		log.Printf("FATAL: ffmpeg command stdout failed with %s\n", err)
	}

	cmdStdErr, err := s.coder.StderrPipe()
	if err != nil {
		log.Printf("FATAL: ffmpeg command stdout failed with %s\n", err)
	}

	err = s.coder.Start()
	if err != nil {
		log.Printf("FATAL: ffmpeg command failed with %s\n", err)
	}

	go s.monitorTranscodeOutput(cmdStdOut, startAt)
	go s.monitorStderr(cmdStdErr)
	go s.monitorExit()
}

func (s *Stream) checkGoal(id int) {
	// Adaptive buffering based on content complexity
	goalBufferMin := s.c.GoalBufferMin
	goalBufferMax := s.c.GoalBufferMax
	
	// Increase buffer requirements for demanding content
	if s.m.probe.BitRate > 50000000 { // >50Mbps
		goalBufferMin = int(float64(goalBufferMin) * 1.5)
		goalBufferMax = int(float64(goalBufferMax) * 1.8)
	}
	if s.m.probe.FrameRate >= 50 { // High framerate content (like deinterlaced)
		goalBufferMin = int(float64(goalBufferMin) * 1.4)
		goalBufferMax = int(float64(goalBufferMax) * 1.6)
	}
	
	// Cap at reasonable limits to avoid excessive memory usage
	if goalBufferMax > 25 {
		goalBufferMax = 25
	}
	if goalBufferMin > goalBufferMax/2 {
		goalBufferMin = goalBufferMax / 2
	}

	goal := id + goalBufferMin
	if goal > s.goal {
		s.goal = id + goalBufferMax

		// resume encoding
		if s.coder != nil {
			log.Printf("%s-%s: resuming transcoding (adaptive buffer: %d-%d)", s.m.id, s.quality, goalBufferMin, goalBufferMax)
			s.coder.Process.Signal(syscall.SIGCONT)
		}
	}
	
	// For demanding content, be much more aggressive about staying ahead
	chunksAhead := 0
	for i := id; i <= id+goalBufferMax; i++ {
		if chunk, ok := s.chunks[i]; ok && chunk.done {
			chunksAhead++
		}
	}
	
	// Dynamic restart threshold based on content complexity
	restartThreshold := goalBufferMax / 2
	if s.m.probe.BitRate > 100000000 { // Very high bitrate
		restartThreshold = int(float64(goalBufferMax) * 0.75) // Keep 75% ahead
	} else if s.m.probe.BitRate > 50000000 || s.m.probe.FrameRate >= 50 {
		restartThreshold = int(float64(goalBufferMax) * 0.6) // Keep 60% ahead
	}
	
	if chunksAhead < restartThreshold && s.coder == nil {
		log.Printf("%s-%s: proactively restarting for chunk %d (%d/%d chunks ahead, bitrate: %dMbps, fps: %d)", 
			s.m.id, s.quality, id, chunksAhead, goalBufferMax, s.m.probe.BitRate/1000000, s.m.probe.FrameRate)
		
		// Start transcoding immediately in this thread for demanding content
		if s.m.probe.BitRate > 50000000 || s.m.probe.FrameRate >= 50 {
			s.goal = id + goalBufferMax
			s.transcode(id)
		} else {
			// Use goroutine for less demanding content
			go func() {
				s.mutex.Lock()
				defer s.mutex.Unlock()
				if s.coder == nil { // Double-check we still need to start
					s.goal = id + goalBufferMax
					s.transcode(id)
				}
			}()
		}
	}
}

func (s *Stream) getTsPath(id int) string {
	if id == -1 {
		return fmt.Sprintf("%s/%s-%%06d.ts", s.m.tempDir, s.quality)
	}
	return fmt.Sprintf("%s/%s-%06d.ts", s.m.tempDir, s.quality, id)
}

func (s *Stream) getSegmentPath(id int, ext string) string {
	if id == -1 {
		return fmt.Sprintf("%s/%s-%%06d.%s", s.m.tempDir, s.quality, ext)
	}
	return fmt.Sprintf("%s/%s-%06d.%s", s.m.tempDir, s.quality, id, ext)
}

func (s *Stream) getChunkPath(id int) string {
	// Try both extensions for compatibility
	tsPath := s.getTsPath(id)
	mp4Path := s.getSegmentPath(id, "mp4")
	
	// Check which file exists
	if _, err := os.Stat(tsPath); err == nil {
		return tsPath
	}
	if _, err := os.Stat(mp4Path); err == nil {
		return mp4Path
	}
	
	// Default to TS path
	return tsPath
}

// Separate goroutine
func (s *Stream) monitorTranscodeOutput(cmdStdOut io.ReadCloser, startAt float64) {
	s.mutex.Lock()
	coder := s.coder
	s.mutex.Unlock()

	defer cmdStdOut.Close()
	stdoutReader := bufio.NewReader(cmdStdOut)

	for {
		if s.coder != coder {
			break
		}

		line, err := stdoutReader.ReadBytes('\n')
		if err == io.EOF {
			if len(line) == 0 {
				break
			}
		} else if err != nil {
			log.Println(err)
			break
		} else {
			line = line[:(len(line) - 1)]
		}

		l := string(line)

		if strings.Contains(l, ".ts") {
			// 1080p-000003.ts
			idx := strings.Split(strings.Split(l, "-")[1], ".")[0]
			id, err := strconv.Atoi(idx)
			if err != nil {
				log.Println("Error parsing chunk id")
			}

			if s.seenChunks[id] {
				continue
			}
			s.seenChunks[id] = true

			// Debug
			log.Printf("%s-%s: recv %s", s.m.id, s.quality, l)

			func() {
				s.mutex.Lock()
				defer s.mutex.Unlock()

				// The coder has changed; do nothing
				if s.coder != coder {
					return
				}

				// Notify everyone
				chunk := s.createChunk(id)
				if chunk.done {
					return
				}
				chunk.done = true
				for _, n := range chunk.notifs {
					n <- true
				}

				// Check goal satisfied
				if id >= s.goal {
					log.Printf("%s-%s: goal satisfied: %d", s.m.id, s.quality, s.goal)
					s.coder.Process.Signal(syscall.SIGSTOP)
				}
			}()
		}
	}
}

func (s *Stream) monitorStderr(cmdStdErr io.ReadCloser) {
	stderrReader := bufio.NewReader(cmdStdErr)

	for {
		line, err := stderrReader.ReadBytes('\n')
		if err == io.EOF {
			if len(line) == 0 {
				break
			}
		} else if err != nil {
			log.Println(err)
			break
		} else {
			line = line[:(len(line) - 1)]
		}
		log.Println("ffmpeg-error:", string(line))
	}
}

func (s *Stream) monitorExit() {
	// Join the process
	coder := s.coder
	err := coder.Wait()

	// Try to get exit status
	if exitError, ok := err.(*exec.ExitError); ok {
		exitcode := exitError.ExitCode()
		log.Printf("%s-%s: ffmpeg exited with status: %d", s.m.id, s.quality, exitcode)

		s.mutex.Lock()
		defer s.mutex.Unlock()

		// If error code is >0, there was an error in transcoding
		if exitcode > 0 && s.coder == coder {
			// Notify all outstanding chunks
			for _, chunk := range s.chunks {
				for _, n := range chunk.notifs {
					n <- true
				}
			}
		}
	}
}
