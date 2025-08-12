package main

import (
	"fmt"
	"log"
	"os"
	"runtime"

	"github.com/pulsejet/go-vod/transcoder"
)

const VERSION = "0.1.16"

func main() {
	// Auto-detect optimal settings based on hardware
	cpuCount := runtime.NumCPU()
	maxConcurrent := cpuCount / 2 // Conservative: 2 CPU cores per transcode
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	
	// For NVENC, allow more concurrent streams since GPU can handle multiple
	// Use more conservative settings only if system is likely constrained
	if maxConcurrent > 6 {
		maxConcurrent = 6 // Cap higher for better GPU utilization
	}

	// Build initial configuration with hardware-aware defaults
	c := &transcoder.Config{
		VersionMonitor:  false,
		Version:         VERSION,
		Bind:            ":47788",
		ChunkSize:       3,
		LookBehind:      8,        // Even more for high bitrate content
		GoalBufferMin:   3,        // Start buffering earlier
		GoalBufferMax:   12,       // Much larger buffer for demanding content
		StreamIdleTime:  60,
		ManagerIdleTime: 60,
		
		// Performance optimizations with intelligent defaults
		MaxConcurrentTranscodes: maxConcurrent,
		GPUMemoryFraction:      0.75,    // Use 75% of GPU memory
		CUDADevice:             0,       // Primary GPU
		NPPStreamCount:         2,       // 2 NPP streams for parallelism
		CUDADecodeThreads:      4,       // 4 decode threads
		ChunkBufferSize:        128,     // 128KB I/O buffer
		EnableMemoryMapping:    true,    // Enable for large files
		EnableClientHints:      true,    // Parse client capabilities
		AdaptiveComplexity:     true,    // Adjust encoding based on content
		
		// NVENC settings - use NPP since your system supports it
		NVENCScale:             "npp",   // Use NPP scaler for better performance
		
		// HLS compatibility defaults for maximum browser support
		HLSVersion:         3,        // HLS v3 for maximum compatibility
		EnableFMP4:         true,     // Support modern browsers
		EnableTSFallback:   true,     // Fallback for older browsers
		LowBandwidthMode:   false,    // Auto-detect based on client
		ForceCompatibility: false,    // Let client detection decide
	}

	// Parse arguments
	for _, arg := range os.Args[1:] {
		if arg == "-version-monitor" {
			c.VersionMonitor = true
		} else if arg == "-version" {
			fmt.Print("go-vod " + VERSION)
			return
		} else {
			c.FromFile(arg) // config file
		}
	}

	// Auto detect ffmpeg and ffprobe
	c.AutoDetect()

	// Start server
	code := transcoder.NewHandler(c).Start()

	// Exit
	log.Println("Exiting go-vod with status code", code)
	os.Exit(code)
}
