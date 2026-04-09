package core

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// RPCRequest represents an incoming JSON-RPC call.
type RPCRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// RPCResponse is the standard response envelope.
type RPCResponse struct {
	Result interface{} `json:"result,omitempty"`
	Error  *RPCError   `json:"error,omitempty"`
}

// RPCError describes an error in the RPC response.
type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// HandleRPC dispatches a JSON-RPC request to the appropriate method.
func (c *Core) HandleRPC(input string) string {
	var req RPCRequest
	if err := json.Unmarshal([]byte(input), &req); err != nil {
		return marshalError("PARSE_ERROR", "invalid JSON: "+err.Error())
	}

	result, err := c.dispatch(req.Method, req.Params)
	if err != nil {
		return marshalError("METHOD_ERROR", err.Error())
	}

	return marshalResult(result)
}

func (c *Core) dispatch(method string, params json.RawMessage) (interface{}, error) {
	switch method {

	// ── Config ───────────────────────────────────────────────
	case "getConfig":
		return c.config, nil

	case "saveConfig":
		var cfg Config
		if err := json.Unmarshal(params, &cfg); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if err := SaveConfig(&cfg); err != nil {
			return nil, err
		}
		c.config = &cfg
		return map[string]string{"status": "ok"}, nil

	case "getDownloadOptions":
		return c.downloader.GetOptions(), nil

	case "setDownloadOptions":
		var opts DownloadOptions
		if err := json.Unmarshal(params, &opts); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		c.downloader.SetOptions(opts)
		// Persist to config file
		c.config.DownloadQuality = opts.Quality
		c.config.FileNameFormat = opts.FileNameFormat
		c.config.OrganizeFolders = opts.OrganizeFolders
		c.config.FolderTemplate = opts.FolderTemplate
		c.config.EmbedCover = opts.EmbedCover
		c.config.SaveCoverFile = opts.SaveCoverFile
		c.config.SaveFolderCover = opts.SaveFolderCover
		c.config.AutoAnalyze = opts.AutoAnalyze
		c.config.AutoQualityFallback = opts.AutoQualityFallback
		c.config.QualityOrder = opts.QualityFallbackOrder
		c.config.FirstArtistOnly = opts.FirstArtistOnly
		c.config.SkipExisting = opts.SkipExisting
		c.config.ArtistSeparator = opts.ArtistSeparator
		c.config.PlaylistSubfolder = opts.PlaylistSubfolder
		c.config.SaveLyricsFile = opts.SaveLyricsFile
		c.config.SeparateSingles = opts.SeparateSingles
		if err := SaveConfig(c.config); err != nil {
			return nil, fmt.Errorf("failed to save config: %w", err)
		}
		return map[string]string{"status": "ok"}, nil

	// ── Content Fetching ────────────────────────────────────
	case "fetchContent":
		var p struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return c.fetchContent(p.URL)

	case "validateURL":
		var p struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		// Try source manager first (supports Tidal + Qobuz + others)
		if c.sourceManager != nil {
			if source, err := c.sourceManager.DetectSource(p.URL); err == nil {
				id, contentType, err := source.ParseURL(p.URL)
				if err != nil {
					return nil, err
				}
				return map[string]string{"id": id, "type": contentType, "source": source.Name()}, nil
			}
		}
		// Fallback to Tidal-only parsing
		id, contentType, err := ParseTidalURL(p.URL)
		if err != nil {
			return nil, err
		}
		return map[string]string{"id": id, "type": contentType, "source": "tidal"}, nil

	case "detectSource":
		var p struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return DetectSourceFromURL(p.URL), nil

	// ── Search ──────────────────────────────────────────────
	case "searchTidal":
		var p struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if p.Limit <= 0 {
			p.Limit = 20
		}
		results, err := c.downloader.SearchTracks(p.Query, p.Limit)
		if err != nil {
			return nil, err
		}
		return convertSearchResults(results), nil

	case "searchTidalAlbums":
		var p struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if p.Limit <= 0 {
			p.Limit = 20
		}
		return c.tidalClient.SearchAlbums(p.Query, p.Limit)

	case "searchTidalArtists":
		var p struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if p.Limit <= 0 {
			p.Limit = 20
		}
		return c.tidalClient.SearchArtists(p.Query, p.Limit)

	// ── Download Queue ──────────────────────────────────────
	case "queueDownloads":
		var p struct {
			Tracks    []TidalTrack `json:"tracks"`
			OutputDir string       `json:"outputDir"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		queued := c.downloadManager.QueueMultiple(p.Tracks, p.OutputDir)
		return map[string]int{"queued": queued}, nil

	case "queueQobuzDownloads":
		var p struct {
			Tracks    []SourceTrack `json:"tracks"`
			OutputDir string        `json:"outputDir"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		queued := c.downloadManager.QueueQobuzTracks(p.Tracks, p.OutputDir)
		return map[string]int{"queued": queued}, nil

	case "queueSingle":
		var p struct {
			TrackID   int    `json:"trackId"`
			OutputDir string `json:"outputDir"`
			Title     string `json:"title"`
			Artist    string `json:"artist"`
			ISRC      string `json:"isrc"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if p.ISRC != "" {
			return nil, c.downloadManager.QueueDownloadWithISRC(p.TrackID, p.OutputDir, p.Title, p.Artist, p.ISRC)
		}
		return nil, c.downloadManager.QueueDownload(p.TrackID, p.OutputDir, p.Title, p.Artist)

	case "queueSingleWithQuality":
		var p struct {
			TrackID   int    `json:"trackId"`
			OutputDir string `json:"outputDir"`
			Title     string `json:"title"`
			Artist    string `json:"artist"`
			ISRC      string `json:"isrc"`
			Quality   string `json:"quality"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return nil, c.downloadManager.queueDownloadFullWithQuality(p.TrackID, p.OutputDir, p.Title, p.Artist, p.ISRC, 0, "", "", p.Quality)

	case "getQueueStatus":
		return map[string]interface{}{
			"active":  c.downloadManager.GetActiveCount(),
			"queued":  c.downloadManager.GetQueueLength(),
			"failed":  c.downloadManager.GetFailedCount(),
			"running": c.downloadManager.IsRunning(),
			"paused":  c.downloadManager.IsPaused(),
		}, nil

	case "pauseDownloads":
		c.downloadManager.PauseQueue()
		c.emitEvent("queue-paused", nil)
		return map[string]string{"status": "paused"}, nil

	case "resumeDownloads":
		c.downloadManager.ResumeQueue()
		c.emitEvent("queue-resumed", nil)
		return map[string]string{"status": "resumed"}, nil

	case "cancelDownload":
		var p struct {
			TrackID int `json:"trackId"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return nil, c.downloadManager.CancelDownload(p.TrackID)

	case "retryAllFailed":
		count := c.downloadManager.RetryAllFailed()
		return map[string]int{"retried": count}, nil

	case "getFailedJobs":
		return c.downloadManager.GetFailedJobs(), nil

	case "clearFailed":
		count := c.downloadManager.ClearFailed()
		return map[string]int{"cleared": count}, nil

	// ── Files & Metadata ────────────────────────────────────
	case "listFiles":
		var p struct {
			Dir string `json:"dir"`
		}
		if params != nil {
			json.Unmarshal(params, &p)
		}
		dir := p.Dir
		if dir == "" {
			dir = c.config.DownloadFolder
		}
		return ListFLACFiles(dir)

	case "deleteFile":
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return nil, DeleteFile(p.Path)

	case "getMetadata":
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return ReadFLACMetadata(p.Path)

	case "editMetadata":
		var p struct {
			Path     string `json:"path"`
			Metadata struct {
				Title       string `json:"title"`
				Artist      string `json:"artist"`
				Album       string `json:"album"`
				AlbumArtist string `json:"albumArtist"`
				TrackNumber int    `json:"trackNumber"`
				DiscNumber  int    `json:"discNumber"`
				Year        string `json:"year"`
				Genre       string `json:"genre"`
				ISRC        string `json:"isrc"`
				Label       string `json:"label"`
				Copyright   string `json:"copyright"`
				Composer    string `json:"composer"`
				Comment     string `json:"comment"`
			} `json:"metadata"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		ext := strings.ToLower(filepath.Ext(p.Path))
		if ext != ".flac" {
			return nil, fmt.Errorf("unsupported file type: %s (only .flac supported)", ext)
		}
		// Read existing file to preserve cover art
		existingData, err := os.ReadFile(p.Path)
		if err != nil {
			return nil, fmt.Errorf("failed to read file: %w", err)
		}
		tagger := NewFLACTagger()
		meta := TrackMetadata{
			Title:       p.Metadata.Title,
			Artist:      p.Metadata.Artist,
			Album:       p.Metadata.Album,
			AlbumArtist: p.Metadata.AlbumArtist,
			TrackNumber: p.Metadata.TrackNumber,
			DiscNumber:  p.Metadata.DiscNumber,
			Year:        p.Metadata.Year,
			Genre:       p.Metadata.Genre,
			ISRC:        p.Metadata.ISRC,
			Label:       p.Metadata.Label,
			Copyright:   p.Metadata.Copyright,
			Composer:    p.Metadata.Composer,
			Comment:     p.Metadata.Comment,
		}
		// Rebuild preserving existing cover art
		newData, err := tagger.RebuildPreservingCover(existingData, meta)
		if err != nil {
			return nil, fmt.Errorf("failed to rebuild FLAC: %w", err)
		}
		if err := os.WriteFile(p.Path, newData, 0644); err != nil {
			return nil, fmt.Errorf("failed to write file: %w", err)
		}
		return map[string]bool{"success": true}, nil

	case "extractCoverArt":
		var p struct {
			Path       string `json:"path"`
			OutputPath string `json:"outputPath"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		imageData, mimeType, err := GetCoverArt(p.Path)
		if err != nil {
			return nil, err
		}
		outputPath := p.OutputPath
		if outputPath == "" {
			outputPath = filepath.Join(filepath.Dir(p.Path), "cover.jpg")
		}
		ext := ".jpg"
		if strings.Contains(mimeType, "png") {
			ext = ".png"
		}
		if !strings.HasSuffix(outputPath, ext) && !strings.HasSuffix(outputPath, ".jpg") && !strings.HasSuffix(outputPath, ".png") {
			outputPath += ext
		}
		if err := os.WriteFile(outputPath, imageData, 0644); err != nil {
			return nil, fmt.Errorf("failed to write cover art: %w", err)
		}
		return map[string]interface{}{"success": true, "path": outputPath}, nil

	case "saveLyricsToFile":
		var p struct {
			Path   string `json:"path"`
			Lyrics string `json:"lyrics"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if p.Lyrics != "" {
			// Save provided lyrics directly
			lrcPath := strings.TrimSuffix(p.Path, filepath.Ext(p.Path)) + ".lrc"
			if err := os.WriteFile(lrcPath, []byte(p.Lyrics), 0644); err != nil {
				return nil, fmt.Errorf("failed to write lyrics: %w", err)
			}
			return map[string]interface{}{"success": true, "path": lrcPath}, nil
		}
		// Auto-fetch from LRCLIB using file metadata
		fileMeta, err := ReadFLACMetadata(p.Path)
		if err != nil {
			return nil, fmt.Errorf("cannot read metadata: %w", err)
		}
		client := NewLyricsClient()
		lyricsResult, err := client.FetchLyricsForFile(fileMeta)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch lyrics: %w", err)
		}
		synced := lyricsResult.Synced
		plain := lyricsResult.Plain
		if synced == "" && plain == "" {
			return nil, fmt.Errorf("no lyrics found for this track")
		}
		if err := SaveLyricsFile(p.Path, synced, plain); err != nil {
			return nil, err
		}
		savedPath := strings.TrimSuffix(p.Path, filepath.Ext(p.Path))
		if synced != "" {
			savedPath += ".lrc"
		} else {
			savedPath += ".txt"
		}
		return map[string]interface{}{"success": true, "path": savedPath}, nil

	case "reEnrichMetadata":
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		fileMeta, err := ReadFLACMetadata(p.Path)
		if err != nil {
			return nil, fmt.Errorf("cannot read metadata: %w", err)
		}
		var updatedFields []string
		// Try to fetch lyrics if not present
		if !fileMeta.HasLyrics {
			client := NewLyricsClient()
			lyricsResult, err := client.FetchLyricsForFile(fileMeta)
			if err == nil {
				if lyricsResult != nil {
					lr := lyricsResult
					tagger := NewFLACTagger()
					if lr.Synced != "" || lr.Plain != "" {
						if err := tagger.EmbedLyrics(p.Path, lr.Plain, lr.Synced); err == nil {
							updatedFields = append(updatedFields, "lyrics")
						}
						// Also save .lrc file
						_ = SaveLyricsFile(p.Path, lr.Synced, lr.Plain)
					}
				}
			}
		}
		return map[string]interface{}{
			"success":       true,
			"updated_fields": updatedFields,
		}, nil

	// ── Analysis ────────────────────────────────────────────
	case "analyzeFile":
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return AnalyzeFLAC(p.Path)

	case "quickAnalyze":
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return QuickAnalyze(p.Path)

	// ── Lyrics ──────────────────────────────────────────────
	case "fetchLyrics":
		var p struct {
			Title    string `json:"title"`
			Artist   string `json:"artist"`
			Duration int    `json:"duration"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		client := NewLyricsClient()
		return client.SearchLyrics(p.Title, p.Artist, p.Duration)

	case "fetchLyricsForFile":
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		meta, err := ReadFLACMetadata(p.Path)
		if err != nil {
			return nil, fmt.Errorf("cannot read metadata: %w", err)
		}
		client := NewLyricsClient()
		return client.FetchLyricsForFile(meta)

	case "embedLyrics":
		var p struct {
			Path   string `json:"path"`
			Plain  string `json:"plain"`
			Synced string `json:"synced"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		tagger := NewFLACTagger()
		return nil, tagger.EmbedLyrics(p.Path, p.Plain, p.Synced)

	// ── Conversion ──────────────────────────────────────────
	case "isConverterAvailable":
		return IsConverterAvailable(), nil

	case "getConversionFormats":
		converter, err := NewConverter()
		if err != nil {
			return nil, err
		}
		return converter.GetFormats(), nil

	case "convertFiles":
		var p struct {
			Files   []string          `json:"files"`
			Options ConversionOptions `json:"options"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		converter, err := NewConverter()
		if err != nil {
			return nil, err
		}
		return converter.ConvertMultiple(p.Files, p.Options), nil

	// ── Rename ──────────────────────────────────────────────
	case "previewRename":
		var p struct {
			Files    []string `json:"files"`
			Template string   `json:"template"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return PreviewRename(p.Files, p.Template), nil

	case "renameFiles":
		var p struct {
			Files    []string `json:"files"`
			Template string   `json:"template"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return RenameFiles(p.Files, p.Template), nil

	case "getRenameTemplates":
		return GetRenameTemplates(), nil

	// ── Sources ─────────────────────────────────────────────
	case "getAvailableSources":
		return c.sourceManager.GetSourcesInfo(), nil

	case "getPreferredSource":
		source, ok := c.sourceManager.GetPreferredSource()
		if !ok {
			return nil, nil
		}
		return source.Name(), nil

	case "setPreferredSource":
		var p struct {
			Source   string `json:"source"`
			Fallback *bool  `json:"fallback,omitempty"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		c.sourceManager.SetPreferredSource(p.Source)
		c.config.PreferredSource = p.Source
		if p.Fallback != nil {
			c.config.AutoQualityFallback = *p.Fallback
		}
		if err := SaveConfig(c.config); err != nil {
			return nil, fmt.Errorf("failed to save config: %w", err)
		}
		return map[string]string{"status": "ok"}, nil

	case "updateQobuzCredentials":
		var p struct {
			AppID     string `json:"appId"`
			AppSecret string `json:"appSecret"`
			AuthToken string `json:"authToken"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		c.qobuzSource.SetCredentials(p.AppID, p.AppSecret, p.AuthToken)
		// Persist credentials to config file
		c.config.QobuzAppID = p.AppID
		c.config.QobuzAppSecret = p.AppSecret
		c.config.QobuzAuthToken = p.AuthToken
		c.config.QobuzEnabled = p.AppID != "" && p.AuthToken != ""
		if err := SaveConfig(c.config); err != nil {
			return nil, fmt.Errorf("failed to save config: %w", err)
		}
		return map[string]string{"status": "ok"}, nil

	case "testQobuzConnection":
		if c.qobuzSource == nil || !c.qobuzSource.IsAvailable() {
			return map[string]interface{}{"success": false, "error": "Qobuz not configured"}, nil
		}
		if err := c.qobuzSource.TestConnection(); err != nil {
			return map[string]interface{}{"success": false, "error": err.Error()}, nil
		}
		return map[string]interface{}{"success": true}, nil

	// ── Extensions ─────────────────────────────────────────
	case "getExtensions":
		return c.extensionManager.GetInstalled(), nil

	case "installExtension":
		var p struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return c.extensionManager.Install(p.URL)

	case "uninstallExtension":
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return nil, c.extensionManager.Uninstall(p.ID)

	case "enableExtension":
		var p struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return nil, c.extensionManager.SetEnabled(p.ID, p.Enabled)

	case "setExtensionAuth":
		var p struct {
			ID   string            `json:"id"`
			Data map[string]string `json:"data"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return nil, c.extensionManager.SetAuthData(p.ID, p.Data)

	case "getDownloadFallbacks":
		return c.extensionManager.GetDownloadExtensions(), nil

	case "getExtensionSources":
		return c.extensionManager.GetRegistrySources(), nil

	case "addExtensionSource":
		var p struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if err := c.extensionManager.AddRegistrySource(p.URL); err != nil {
			return nil, err
		}
		return map[string]string{"status": "ok"}, nil

	case "removeExtensionSource":
		var p struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if err := c.extensionManager.RemoveRegistrySource(p.URL); err != nil {
			return nil, err
		}
		return map[string]string{"status": "ok"}, nil

	case "fetchRegistryFromGitHub":
		var p struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		return c.extensionManager.FetchRegistryFromGitHub(p.URL)

	case "getExtensionRegistry":
		var p struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if p.URL == "" {
			p.URL = "https://raw.githubusercontent.com/kushiemoon-dev/flacidal-extensions/main/index.json"
		}
		resp, err := http.Get(p.URL)
		if err != nil {
			return nil, fmt.Errorf("registry fetch failed: %w", err)
		}
		defer resp.Body.Close()
		var registry []map[string]interface{}
		if err := json.NewDecoder(resp.Body).Decode(&registry); err != nil {
			return nil, fmt.Errorf("invalid registry: %w", err)
		}
		return registry, nil

	// ── History ─────────────────────────────────────────────
	case "getHistory":
		if c.db == nil {
			return nil, fmt.Errorf("database not initialized")
		}
		return c.db.GetAllDownloadRecords()

	case "clearHistory":
		if c.db == nil {
			return nil, fmt.Errorf("database not initialized")
		}
		return nil, c.db.ClearAllHistory()

	case "getMatchFailures":
		if c.db == nil {
			return nil, fmt.Errorf("database not initialized")
		}
		return c.db.GetMatchFailures()

	// ── Status ──────────────────────────────────────────────
	case "getLogs":
		return c.logBuffer.GetAll(), nil

	case "clearLogs":
		c.logBuffer.Clear()
		return map[string]string{"status": "ok"}, nil

	case "getVersion":
		return map[string]string{"version": "3.2.1-mobile"}, nil

	case "getCacheStats":
		if c.db == nil {
			return nil, fmt.Errorf("database not initialized")
		}
		total, byMethod, err := c.db.GetCacheStats()
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"total":    total,
			"byMethod": byMethod,
		}, nil

	// ── Queue Persistence ──────────────────────────────────
	case "persistQueue":
		path := filepath.Join(GetDataDir(), "queue.json")
		return nil, c.downloadManager.PersistQueue(path)

	case "restoreQueue":
		path := filepath.Join(GetDataDir(), "queue.json")
		restored, err := c.downloadManager.RestoreQueue(path)
		if err != nil {
			return nil, err
		}
		return map[string]int{"restored": restored}, nil

	case "checkDownloaded":
		var p struct {
			ISRC string `json:"isrc"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if c.db == nil {
			return map[string]bool{"downloaded": false}, nil
		}
		track, _ := c.db.GetCachedTrack(p.ISRC)
		return map[string]bool{"downloaded": track != nil}, nil

	// ── Cover Art ───────────────────────────────────────────
	case "getEmbeddedCoverArt":
		var p struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		// Look for companion .jpg file alongside the .flac
		ext := filepath.Ext(p.Path)
		jpgPath := strings.TrimSuffix(p.Path, ext) + ".jpg"
		data, err := os.ReadFile(jpgPath)
		if err != nil {
			// Try cover.jpg in same directory
			dirPath := filepath.Dir(p.Path)
			data, err = os.ReadFile(filepath.Join(dirPath, "cover.jpg"))
			if err != nil {
				return map[string]string{"coverArt": ""}, nil
			}
		}
		encoded := base64.StdEncoding.EncodeToString(data)
		return map[string]string{"coverArt": encoded}, nil

	// ── YouTube/Cobalt Fallback ────────────────────────────
	case "downloadFromYouTube":
		var p struct {
			URL        string `json:"url"`
			OutputPath string `json:"outputPath"`
			Format     string `json:"format"` // "opus" or "mp3"
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		if p.Format == "" {
			p.Format = "opus"
		}
		cobalt := NewCobaltSource()
		if err := cobalt.Download(p.URL, p.OutputPath, p.Format); err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"success": true,
			"quality": "lossy",
			"format":  p.Format,
			"path":    p.OutputPath,
		}, nil

	// ── Deezer Enrichment ──────────────────────────────────
	case "enrichFromDeezer":
		var p struct {
			ISRC string `json:"isrc"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		deezer := NewDeezerClient()
		meta, err := deezer.EnrichByISRC(p.ISRC)
		if err != nil {
			return nil, err
		}
		return map[string]string{
			"genre":     meta.Genre,
			"label":     meta.Label,
			"copyright": meta.Copyright,
		}, nil

	// ── Multi-service URL Resolution ───────────────────────
	case "resolveURL":
		var p struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		resolved, err := ResolveToTidal(p.URL)
		if err != nil {
			return nil, err
		}
		return map[string]string{
			"tidalId":         resolved.TidalID,
			"type":            resolved.Type,
			"originalService": resolved.OriginalService,
		}, nil

	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

// convertSearchResults converts raw Tidal API search results to flat TidalTrack
// structs with artist names as strings (not nested objects).
func convertSearchResults(results []TidalHifiTrackResponse) []TidalTrack {
	tracks := make([]TidalTrack, len(results))
	for i, r := range results {
		artistStr := r.Artist.Name
		var allArtists []string
		for _, a := range r.Artists {
			allArtists = append(allArtists, a.Name)
		}
		allArtistsStr := artistStr
		if len(allArtists) > 0 {
			allArtistsStr = strings.Join(allArtists, ", ")
		}
		coverURL := ""
		if r.Album.Cover != "" {
			coverURL = fmt.Sprintf("https://resources.tidal.com/images/%s/320x320.jpg",
				FormatCoverUUID(r.Album.Cover))
		}
		tracks[i] = TidalTrack{
			ID:          r.ID,
			Title:       r.Title,
			Artist:      artistStr,
			Artists:     allArtistsStr,
			Album:       r.Album.Title,
			AlbumArtist: r.Album.Artist.Name,
			Duration:    r.Duration,
			ISRC:        r.ISRC,
			CoverURL:    coverURL,
			Explicit:    r.Explicit,
			TrackNum:    r.TrackNumber,
			DiscNum:     r.VolumeNumber,
			ReleaseDate: r.Album.ReleaseDate,
		}
	}
	return tracks
}

// fetchContent fetches content from any supported URL (Tidal, Qobuz, etc.).
func (c *Core) fetchContent(rawURL string) (interface{}, error) {
	// Try multi-source detection first
	if c.sourceManager != nil {
		source, err := c.sourceManager.DetectSource(rawURL)
		if err == nil && source.Name() != "tidal" {
			return c.fetchContentFromSource(source, rawURL)
		}
	}

	// Tidal path (default) — uses HiFi proxy for better results
	id, contentType, err := ParseTidalURL(rawURL)
	if err != nil {
		return nil, err
	}

	result := map[string]interface{}{
		"type":   contentType,
		"id":     id,
		"source": "tidal",
	}

	switch contentType {
	case "track":
		trackID, convErr := strconv.Atoi(id)
		if convErr != nil {
			return nil, fmt.Errorf("invalid track ID: %s", id)
		}
		track, err := c.downloader.GetTrackAsTidalTrack(trackID)
		if err != nil {
			return nil, err
		}
		result["title"] = track.Title
		result["creator"] = track.Artist
		result["coverUrl"] = track.CoverURL
		result["tracks"] = []TidalTrack{*track}
		result["trackCount"] = 1

	case "album":
		album, err := c.downloader.GetAlbumFromProxy(id)
		if err != nil {
			return nil, err
		}
		result["title"] = album.Title
		result["creator"] = album.Artist
		result["coverUrl"] = album.CoverURL
		result["tracks"] = album.Tracks
		result["trackCount"] = len(album.Tracks)

	case "playlist":
		playlist, err := c.downloader.GetPlaylistFromProxy(id)
		if err != nil {
			return nil, err
		}
		result["title"] = playlist.Title
		result["creator"] = playlist.Creator
		result["coverUrl"] = playlist.CoverURL
		result["tracks"] = playlist.Tracks
		result["trackCount"] = len(playlist.Tracks)

	case "artist":
		return c.tidalClient.GetArtistDiscography(id)

	default:
		return nil, fmt.Errorf("unsupported content type: %s", contentType)
	}

	return result, nil
}

// fetchContentFromSource fetches content using the source manager (Qobuz, etc.).
func (c *Core) fetchContentFromSource(source MusicSource, rawURL string) (interface{}, error) {
	id, contentType, err := source.ParseURL(rawURL)
	if err != nil {
		return nil, err
	}

	if !source.IsAvailable() {
		return nil, fmt.Errorf("%s is not available — check your credentials in Settings", source.DisplayName())
	}

	result := map[string]interface{}{
		"type":   contentType,
		"id":     id,
		"source": source.Name(),
	}

	switch contentType {
	case "track":
		track, err := source.GetTrack(id)
		if err != nil {
			return nil, err
		}
		artists := track.Artist
		if len(track.Artists) > 0 {
			artists = strings.Join(track.Artists, ", ")
		}
		result["title"] = track.Title
		result["creator"] = artists
		result["coverUrl"] = track.CoverURL
		result["tracks"] = []SourceTrack{*track}
		result["trackCount"] = 1

	case "album":
		album, err := source.GetAlbum(id)
		if err != nil {
			return nil, err
		}
		result["title"] = album.Title
		result["creator"] = album.Artist
		result["coverUrl"] = album.CoverURL
		result["tracks"] = album.Tracks
		result["trackCount"] = len(album.Tracks)

	case "playlist":
		playlist, err := source.GetPlaylist(id)
		if err != nil {
			return nil, err
		}
		result["title"] = playlist.Title
		result["creator"] = playlist.Creator
		result["coverUrl"] = playlist.CoverURL
		result["tracks"] = playlist.Tracks
		result["trackCount"] = len(playlist.Tracks)

	default:
		return nil, fmt.Errorf("unsupported content type: %s", contentType)
	}

	return result, nil
}

// marshalResult serializes a successful response.
func marshalResult(result interface{}) string {
	resp := RPCResponse{Result: result}
	data, err := json.Marshal(resp)
	if err != nil {
		return marshalError("SERIALIZE_ERROR", err.Error())
	}
	return string(data)
}

// marshalError serializes an error response.
func marshalError(code, message string) string {
	resp := RPCResponse{Error: &RPCError{Code: code, Message: message}}
	data, _ := json.Marshal(resp)
	return string(data)
}
