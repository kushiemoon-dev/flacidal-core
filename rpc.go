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
		id, contentType, err := ParseTidalURL(p.URL)
		if err != nil {
			return nil, err
		}
		return map[string]string{"id": id, "type": contentType}, nil

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
		return c.downloader.SearchTracks(p.Query, p.Limit)

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
			Source string `json:"source"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid params: %w", err)
		}
		c.sourceManager.SetPreferredSource(p.Source)
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
		return map[string]string{"status": "ok"}, nil

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
		return map[string]string{"version": "3.1.0-mobile"}, nil

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

	default:
		return nil, fmt.Errorf("unknown method: %s", method)
	}
}

// fetchContent fetches content from a Tidal URL via HiFi proxy (same as desktop).
func (c *Core) fetchContent(rawURL string) (interface{}, error) {
	id, contentType, err := ParseTidalURL(rawURL)
	if err != nil {
		return nil, err
	}

	result := map[string]interface{}{
		"type": contentType,
		"id":   id,
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
