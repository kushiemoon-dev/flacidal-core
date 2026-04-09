package core

import (
	"fmt"
	"sync"
)

// Core is the headless application core, usable from any frontend (Wails, FFI, REST).
// It mirrors the desktop App struct but has no UI dependencies.
type Core struct {
	config          *Config
	db              *Database
	tidalClient     *TidalClient
	spotifySearch   *SpotifyClient
	matcher         *Matcher
	downloader      *TidalHifiService
	downloadManager *DownloadManager
	logBuffer       *LogBuffer
	sourceManager    *SourceManager
	tidalSource      *TidalSource
	qobuzSource      *QobuzSource
	extensionManager *ExtensionManager
	trackContentMap sync.Map // maps trackID (int) → contentID (string) for history tracking

	// EventCallback is called when async events occur (download progress, etc.)
	// Set via SetEventCallback before starting downloads.
	eventCallback func(event Event)
}

// Event represents an async event sent from Go to the frontend.
type Event struct {
	Type    string      `json:"type"`
	Payload interface{} `json:"payload,omitempty"`
}

// NewCore creates and initializes the core with the given data directory.
// If dataDir is empty, uses the default (~/.flacidal/).
func NewCore(dataDir string) (*Core, error) {
	if dataDir != "" {
		SetDataDir(dataDir)
	}

	c := &Core{}

	// Initialize log buffer
	c.logBuffer = NewLogBuffer(500)
	c.logBuffer.Info("FLACidal core starting...")

	// Load config
	config, err := LoadConfig()
	if err != nil {
		c.logBuffer.Warn("Could not load config: " + err.Error())
		config = &Config{}
	}
	c.config = config
	c.logBuffer.Success("Configuration loaded")

	// Initialize database
	db, err := NewDatabase()
	if err != nil {
		c.logBuffer.Error("Database initialization failed: " + err.Error())
	} else {
		c.logBuffer.Success("Database initialized")
	}
	c.db = db

	// Initialize Tidal client
	c.tidalClient = NewTidalClientDefault()
	c.tidalClient.SetCountryCode(config.CountryCode)
	if config.ProxyURL != "" {
		if err := c.tidalClient.SetProxy(config.ProxyURL); err != nil {
			c.logBuffer.Warn("Proxy config error (Tidal API): " + err.Error())
		}
	}

	// Initialize Spotify search client
	c.spotifySearch = NewSpotifyClientForSearch()

	// Initialize matcher
	c.matcher = NewMatcher(c.spotifySearch, c.db)

	// Initialize FLAC downloader
	c.downloader = NewTidalHifiService()
	c.downloader.SetLogger(c.logBuffer)
	if config.ProxyURL != "" {
		if err := c.downloader.SetProxy(config.ProxyURL); err != nil {
			c.logBuffer.Warn("Proxy config error (downloader): " + err.Error())
		}
	}
	if len(config.TidalHifiEndpoints) > 0 {
		c.downloader.SetEndpoints(config.TidalHifiEndpoints)
		c.logBuffer.Info(fmt.Sprintf("Tidal HiFi endpoint pool: %d endpoints configured", len(config.TidalHifiEndpoints)))
	}

	// Set download options
	c.downloader.SetOptions(DownloadOptions{
		Quality:              config.DownloadQuality,
		FileNameFormat:       config.FileNameFormat,
		OrganizeFolders:      config.OrganizeFolders,
		FolderTemplate:       config.FolderTemplate,
		EmbedCover:           config.EmbedCover,
		SaveCoverFile:        config.SaveCoverFile,
		SaveFolderCover:      config.SaveFolderCover,
		AutoAnalyze:          config.AutoAnalyze,
		AutoQualityFallback:  config.AutoQualityFallback,
		QualityFallbackOrder: config.QualityOrder,
		FirstArtistOnly:      config.FirstArtistOnly,
		SkipExisting:         config.SkipExisting,
		ArtistSeparator:      config.ArtistSeparator,
		PlaylistSubfolder:    config.PlaylistSubfolder,
		SaveLyricsFile:       config.SaveLyricsFile,
		SeparateSingles:      config.SeparateSingles,
	})

	// Initialize download manager
	workers := config.ConcurrentDownloads
	if workers <= 0 {
		workers = 4
	}
	c.downloadManager = NewDownloadManager(c.downloader, workers)

	// Initialize sources
	c.tidalSource = NewTidalSource()
	c.qobuzSource = NewQobuzSource(config.QobuzAppID, config.QobuzAppSecret)
	if config.QobuzAuthToken != "" {
		c.qobuzSource.SetCredentials(config.QobuzAppID, config.QobuzAppSecret, config.QobuzAuthToken)
	}

	c.downloadManager.SetFallbackQobuzSource(c.qobuzSource)

	c.sourceManager = NewSourceManager()
	c.sourceManager.RegisterSource(c.tidalSource)
	c.sourceManager.RegisterSource(c.qobuzSource)
	c.sourceManager.SetPreferredSource(config.PreferredSource)

	// Initialize extension manager
	c.extensionManager = NewExtensionManager(GetDataDir(), c.logBuffer)

	c.logBuffer.Success("FLACidal core initialized")
	return c, nil
}

// SetEventCallback registers a callback for async events.
// Also wires download manager progress to emit events.
func (c *Core) SetEventCallback(cb func(event Event)) {
	c.eventCallback = cb

	// Wire download progress to event system
	c.downloadManager.SetProgressCallback(func(trackID int, status string, result *DownloadResult) {
		payload := map[string]interface{}{
			"trackId": trackID,
			"status":  status,
			"result":  result,
		}
		if result != nil {
			payload["bytesDownloaded"] = result.BytesDownloaded
			payload["bytesTotal"] = result.BytesTotal
			payload["speed"] = result.Speed
		}
		c.emitEvent("download-progress", payload)
	})

	// Start download manager workers
	c.downloadManager.Start()
}

// emitEvent sends an event to the registered callback.
func (c *Core) emitEvent(eventType string, payload interface{}) {
	if c.eventCallback != nil {
		c.eventCallback(Event{Type: eventType, Payload: payload})
	}
}

// Shutdown cleanly stops all core components.
func (c *Core) Shutdown() {
	if c.downloadManager != nil {
		c.downloadManager.Stop()
	}
	if c.db != nil {
		c.db.Close()
	}
}
