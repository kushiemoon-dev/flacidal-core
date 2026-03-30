<div align="center">

### flacidal-core

**Shared Go module powering FLACidal desktop & mobile apps**

[![Go](https://img.shields.io/badge/Go-1.23+-00ADD8?style=flat-square&logo=go&logoColor=white)](https://go.dev)
[![License](https://img.shields.io/badge/License-MIT-gray?style=flat-square)](LICENSE)

</div>

---

## Overview

**flacidal-core** is the backend engine shared between [FLACidal Desktop](https://github.com/kushiemoon-dev/FLACidal) and [FLACidal Mobile](https://github.com/kushiemoon-dev/flacidal-mobile). It handles all download logic, metadata tagging, search, lyrics, format conversion, and the extension system.

Used as a Go module via `import` (desktop) or compiled as a C-shared library via FFI (mobile).

---

## Features

- **Multi-source downloads** — Tidal (Hi-Res 24-bit/192kHz) and Qobuz with automatic fallback
- **Concurrent download manager** — Worker pool with pause/resume/cancel/retry
- **FLAC metadata tagging** — Vorbis comments, embedded cover art, custom filename templates
- **Lyrics** — LRCLIB integration with source tracking and instrumental detection
- **Quality analysis** — Spectrum analysis to verify true lossless
- **Format conversion** — FLAC to MP3/AAC/Opus via FFmpeg
- **Extension system** — Declarative JSON manifest for adding music sources
- **SQLite storage** — Download history, ISRC matching cache, extension data
- **JSON-RPC bridge** — 50+ methods exposed via C-shared library for FFI consumers

---

## Architecture

```
flacidal-core/
├── core.go              # Core struct, init, event system
├── rpc.go               # JSON-RPC dispatcher (50+ methods)
├── tidal.go             # Tidal API client
├── downloader.go        # TidalHifiService (proxy-based FLAC downloads)
├── download_manager.go  # Concurrent worker pool with progress events
├── source.go            # MusicSource interface, SourceManager
├── spotify.go           # Spotify ISRC matching
├── matcher.go           # Cross-source track matching
├── tagger.go            # FLAC Vorbis comment writer
├── lyrics.go            # LRCLIB lyrics client
├── analyzer.go          # Spectrum analysis
├── converter.go         # FFmpeg format conversion
├── database.go          # SQLite (history, cache)
├── config.go            # JSON config with injectable DataDir
├── extension.go         # Extension manager (install/uninstall/auth)
├── cmd/bridge/          # C-shared library exports (FFI)
└── Makefile             # Cross-compilation (Android, iOS, Linux)
```

---

## Usage

### As a Go module (desktop)

```go
import core "github.com/kushiemoon-dev/flacidal-core"

c, err := core.NewCore("/path/to/data")
defer c.Shutdown()

result := c.HandleRPC(`{"method": "fetchContent", "params": {"url": "https://tidal.com/album/123"}}`)
```

### As a C-shared library (mobile FFI)

```bash
# Android
make android-arm64

# iOS (requires macOS + Xcode)
make ios

# Linux
make linux
```

Exports: `FlacInit`, `FlacCall`, `FlacCallAsync`, `FlacSetEventCallback`, `FlacFree`, `FlacShutdown`

---

## Build

**Requirements:** Go 1.23+, GCC (for CGo/SQLite)

```bash
# Build and test
go build ./...
go test ./...

# Cross-compile for Android (requires NDK)
make android-arm64
make android-arm
make android-x86_64

# Cross-compile for iOS (requires Xcode)
make ios
```

---

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=kushiemoon-dev/flacidal-core&type=Date)](https://star-history.com/#kushiemoon-dev/flacidal-core&Date)

### FLACidal Ecosystem

[![Star History Chart](https://api.star-history.com/svg?repos=kushiemoon-dev/FLACidal,kushiemoon-dev/flacidal-core,kushiemoon-dev/FLACidal-Mobile&type=Date)](https://star-history.com/#kushiemoon-dev/FLACidal&kushiemoon-dev/flacidal-core&kushiemoon-dev/FLACidal-Mobile&Date)

---

## Disclaimer

This module is intended for **educational and personal use only**. It is not affiliated with Tidal, Qobuz, or any streaming service. Use responsibly and in accordance with local laws.

---

<div align="center">

**MIT License** · Made with ♥ by [KushieMoon](https://github.com/kushiemoon-dev)

</div>
