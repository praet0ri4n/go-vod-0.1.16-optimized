package transcoder

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
)

type Config struct {
	// Current version of go-vod
	Version string

	// Is this server configured?
	Configured bool

	// Restart the server if incorrect version detected
	VersionMonitor bool

	// Bind address
	Bind string `json:"bind"`

	// FFmpeg binary
	FFmpeg string `json:"ffmpeg"`
	// FFprobe binary
	FFprobe string `json:"ffprobe"`
	// Temp files directory
	TempDir string `json:"tempdir"`

	// Size of each chunk in seconds
	ChunkSize int `json:"chunkSize"`
	// How many *chunks* to look behind before restarting transcoding
	LookBehind int `json:"lookBehind"`
	// Number of chunks in goal to restart encoding
	GoalBufferMin int `json:"goalBufferMin"`
	// Number of chunks in goal to stop encoding
	GoalBufferMax int `json:"goalBufferMax"`

	// Number of seconds to wait before shutting down encoding
	StreamIdleTime int `json:"streamIdleTime"`
	// Number of seconds to wait before shutting down a client
	ManagerIdleTime int `json:"managerIdleTime"`

	// Quality Factor (e.g. CRF / global_quality)
	QF int `json:"qf"`

	// Hardware acceleration configuration

	// VA-API
	VAAPI         bool `json:"vaapi"`
	VAAPILowPower bool `json:"vaapiLowPower"`

	// NVENC
	NVENC           bool   `json:"nvenc"`
	NVENCTemporalAQ bool   `json:"nvencTemporalAQ"`
	NVENCScale      string `json:"nvencScale"` // cuda, npp

	// Use transpose workaround for streaming (VA-API)
	UseTranspose bool `json:"useTranspose"`

	// Use GOP size workaround for streaming (NVENC)
	UseGopSize bool `json:"useGopSize"`
	
	// Performance optimizations
	
	// Maximum concurrent transcoding processes (0 = auto-detect)
	MaxConcurrentTranscodes int `json:"maxConcurrentTranscodes"`
	
	// GPU memory management
	GPUMemoryFraction float64 `json:"gpuMemoryFraction"` // 0.8 = use 80% of GPU memory
	
	// Advanced CUDA/NPP settings
	CUDADevice          int    `json:"cudaDevice"`          // GPU device index
	NPPStreamCount      int    `json:"nppStreamCount"`      // NPP stream count for parallelism
	CUDADecodeThreads   int    `json:"cudaDecodeThreads"`   // CUDA decoder threads
	
	// Memory optimization
	ChunkBufferSize     int    `json:"chunkBufferSize"`     // Buffer size for chunk I/O (KB)
	EnableMemoryMapping bool   `json:"enableMemoryMapping"` // Use memory mapping for large files
	
	// Client awareness
	EnableClientHints   bool   `json:"enableClientHints"`   // Parse client capability headers
	AdaptiveComplexity  bool   `json:"adaptiveComplexity"`  // Adjust encoding complexity based on content
	
	// HLS compatibility settings
	HLSVersion          int    `json:"hlsVersion"`          // HLS protocol version (3, 4, 6)
	EnableFMP4          bool   `json:"enableFMP4"`          // Enable fMP4 segments for modern browsers
	EnableTSFallback    bool   `json:"enableTSFallback"`    // Fallback to TS for compatibility
	LowBandwidthMode    bool   `json:"lowBandwidthMode"`    // Special mode for limited devices
	ForceCompatibility  bool   `json:"forceCompatibility"`  // Force maximum compatibility mode
}

func (c *Config) FromFile(path string) {
	// load json config
	content, err := ioutil.ReadFile(path)
	if err != nil {
		log.Fatal("Error when opening file: ", err)
	}

	err = json.Unmarshal(content, &c)
	if err != nil {
		log.Fatal("Error loading config file", err)
	}

	// Set config as loaded
	c.Configured = true
	c.Print()
}

func (c *Config) AutoDetect() {
	// Auto-detect ffmpeg and ffprobe paths
	if c.FFmpeg == "" || c.FFprobe == "" {
		ffmpeg, err := exec.LookPath("ffmpeg")
		if err != nil {
			log.Fatal("Could not find ffmpeg")
		}

		ffprobe, err := exec.LookPath("ffprobe")
		if err != nil {
			log.Fatal("Could not find ffprobe")
		}

		c.FFmpeg = ffmpeg
		c.FFprobe = ffprobe
	}

	// Auto-choose tempdir
	if c.TempDir == "" {
		c.TempDir = os.TempDir() + "/go-vod"
	}

	// Print updated config
	c.Print()
}

func (c *Config) Print() {
	log.Printf("%+v\n", c)
}
