# go-vod Community Enhanced Version 0.1.16

## 🚀 Major Improvements for High-Bitrate Content

This enhanced version of go-vod specifically targets smooth playback of demanding video content, including:
- **High-bitrate footage** (GoPro, Samsung S25 Ultra, professional cameras >50Mbps)
- **Deinterlaced 50fps content** (converted from m2ts sources)
- **Complex edited videos** (Premiere Pro exports with scene changes)
- **Mobile-shot content** with rotation metadata

## ✨ Key Features

### **NVENC Hardware Acceleration**
- **Fixed filter chain issues** that prevented NVENC from working properly
- **Adaptive presets** based on content complexity:
  - >100Mbps: `p2` preset with 250-frame lookahead
  - >50Mbps: `p3` preset with 120-frame lookahead  
  - 50fps+: Special handling for high framerate content
- **NPP scaler integration** for optimal NVIDIA GPU performance
- **Dynamic GPU utilization** with up to 6 concurrent streams

### **Intelligent Buffering**
- **Adaptive buffer sizing**: Up to 22 chunks for demanding content
- **Proactive transcoding restart** with smart thresholds:
  - Normal content: 50% buffer remaining
  - High-bitrate: 75% buffer remaining
  - Complex content: 60% buffer remaining
- **Content-aware chunk management** with extended lookahead

### **Enhanced HLS Compatibility**
- **Universal browser support**: Chrome, Firefox, Safari, Edge, mobile browsers
- **Dual format support**: fMP4 for modern browsers, TS fallback for compatibility
- **Client capability detection** with User-Agent analysis
- **Optimized ABR ladder** with proper bandwidth reporting
- **Advanced stream metadata** (CODECS, AVERAGE-BANDWIDTH)

### **Video Processing Improvements**
- **Rotation handling**: Proper aspect ratio for mobile-shot videos
- **Keyframe optimization**: Better seeking in edited content
- **Rate control**: Aligned encoder output with advertised bitrates
- **Memory optimization**: Dynamic buffer sizing and memory mapping

## 🔧 Technical Details

### **Fixed Issues**
1. **CUDA format pipeline**: Removed conflicting `hwaccel_output_format cuda`
2. **HLS flags conflict**: Fixed `independent_segments+split_by_time` incompatibility
3. **Aspect ratio bugs**: Corrected dimension calculations with rotation metadata
4. **Filter chain errors**: Proper NVENC filter syntax with NPP scaler

### **Performance Optimizations**
- **Extended lookahead**: Up to 250 frames for motion prediction
- **Multipass encoding**: `fullres` mode for better quality
- **GPU memory management**: Intelligent device selection and memory fraction
- **Concurrent processing**: Optimized for multi-stream transcoding

### **Adaptive Logic**
```go
// Example: Content-aware preset selection
if bitrate > 100000000 { // >100Mbps
    preset = "p2"         // Slowest, highest quality
    lookahead = 250       // Maximum prediction
} else if framerate >= 50 { // High FPS content
    preset = "p3"         // Slower preset for stability
    lookahead *= 1.5      // Extended lookahead
}
```

## 🎯 Performance Results

### **Before vs After**
- **Stuttering**: Significantly reduced on high-bitrate content
- **GPU utilization**: More consistent load vs. 0%-70% spikes  
- **Buffer health**: Larger grey bars that stay ahead of playhead
- **Cross-browser**: Perfect playback on all tested browsers
- **Startup time**: "Blazingly fast" startup (user feedback)

### **Tested Content**
✅ **GoPro Hero footage** (high bitrate, complex motion)  
✅ **Samsung S25 Ultra MP4** (variable bitrate, HDR)  
✅ **Deinterlaced 50fps** (m2ts → mp4 conversions)  
✅ **Premiere Pro exports** (complex edited content)  
✅ **Mobile portrait videos** (rotation metadata)  

## 🌐 Browser Compatibility

| Browser | Desktop | Mobile | Android TV | Status |
|---------|---------|--------|------------|--------|
| Chrome | ✅ | ✅ | ✅ | Perfect |
| Firefox | ✅ | ✅ | ✅ | Perfect |
| Safari | ✅ | ✅ | N/A | Perfect |
| Edge | ✅ | ✅ | ✅ | Perfect |
| Brave | ✅ | ✅ | ✅ | Perfect |
| Via Browser | N/A | ✅ | ✅ | Perfect |
| Samsung Internet | N/A | ✅ | ✅ | Perfect |

## 🚀 Quick Start

### **Requirements**
- NVIDIA GPU with NVENC support
- FFmpeg with CUDA, NVENC, and NPP support
- Go 1.19+ for building

### **Build**
```bash
CGO_ENABLED=0 go build -ldflags="-s -w"
```

### **Configuration**
The enhanced version auto-detects optimal settings, but you can override:
```json
{
  "MaxConcurrentTranscodes": 6,
  "GPUMemoryFraction": 0.75,
  "NVENCScale": "npp",
  "EnableFMP4": true,
  "EnableTSFallback": true,
  "AdaptiveComplexity": true,
  "GoalBufferMax": 12
}
```

## 🔍 Debugging

Enhanced logging shows adaptive behavior:
```
stream-720p: proactively restarting for chunk 15 (8/22 chunks ahead, bitrate: 85Mbps, fps: 50)
stream-1080p: resuming transcoding (adaptive buffer: 5-22)
yegcia9gvmj0: detected high bitrate content (95000000 bps), optimizing settings
```

## 🤝 Contributing

This community-enhanced version addresses real-world performance issues with modern high-bitrate content. Feel free to:
- Test with your own demanding content
- Report issues with specific video types
- Suggest further optimizations
- Contribute additional browser compatibility fixes

## 📄 License

Same as original go-vod project. This enhanced version maintains full compatibility while adding significant performance improvements for demanding content.

---

**Original go-vod by [pulsejet](https://github.com/pulsejet/go-vod)**  
**Community enhancements for high-bitrate content optimization**
