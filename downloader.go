package core

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Tidal HiFi API Service - vogel.qqdl.site
// Free Tidal FLAC proxy without credentials (same as mkv-video)

const (
	tidalHifiAPIBase = "https://vogel.qqdl.site"
)

// defaultTidalHifiEndpoints is the built-in list of Tidal HiFi proxy endpoints.
// Users can extend this via TidalHifiEndpoints in config.
var defaultTidalHifiEndpoints = []string{
	"https://hifi-one.spotisaver.net",
	"https://hifi-two.spotisaver.net",
	"https://vogel.qqdl.site",
	"https://triton.squid.wtf",
	"https://maus.qqdl.site",
	"https://hund.qqdl.site",
}

// defaultMetadataEndpoints are hifi-api v2.4 proxy endpoints that support
// album/playlist/mix metadata fetching (/album/, /playlist/, /mix/ paths).
// These are tried in order; the primary download endpoints above do not support
// these metadata paths.
var defaultMetadataEndpoints = []string{
	"https://hifi-one.spotisaver.net",
	"https://hifi-two.spotisaver.net",
	"https://triton.squid.wtf",
}

// TidalHifiService implements FLAC downloading via a pool of proxy endpoints
type TidalHifiService struct {
	pool             *EndpointPool
	parallel         bool
	downloadClient   *http.Client // Separate client for downloads (no timeout)
	options          DownloadOptions
	logger           *LogBuffer // optional — set via SetLogger
	downloadProgress func(written, total int64) // optional byte-level progress callback
}

// downloadProgressWriter wraps an io.Writer and reports byte-level progress during downloads.
type downloadProgressWriter struct {
	writer     io.Writer
	total      int64
	written    int64
	onProgress func(written, total int64)
	lastReport time.Time
}

func (pw *downloadProgressWriter) Write(p []byte) (int, error) {
	n, err := pw.writer.Write(p)
	pw.written += int64(n)
	if time.Since(pw.lastReport) > 250*time.Millisecond {
		pw.lastReport = time.Now()
		if pw.onProgress != nil {
			pw.onProgress(pw.written, pw.total)
		}
	}
	return n, err
}

// TidalHifiTrackResponse represents the track info response from vogel
type TidalHifiTrackResponse struct {
	ID           int    `json:"id"`
	Title        string `json:"title"`
	Duration     int    `json:"duration"`
	TrackNumber  int    `json:"trackNumber"`
	VolumeNumber int    `json:"volumeNumber"` // Disc number for multi-disc albums
	ISRC         string `json:"isrc"`
	Explicit     bool   `json:"explicit"`
	Artist       struct {
		Name string `json:"name"`
	} `json:"artist"`
	Artists []struct {
		Name string `json:"name"`
	} `json:"artists"`
	Album struct {
		ID          int    `json:"id"`
		Title       string `json:"title"`
		Cover       string `json:"cover"`
		ReleaseDate string `json:"releaseDate"`
		Artist      struct {
			Name string `json:"name"`
		} `json:"artist"`
		Artists []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"artists"`
		NumberOfVolumes int `json:"numberOfVolumes"`
		NumberOfTracks  int `json:"numberOfTracks"`
	} `json:"album"`
}

// TidalStreamResponse represents the stream/manifest response
type TidalStreamResponse struct {
	TrackID      int    `json:"trackId"`
	AssetID      int    `json:"assetId,omitempty"`
	AudioMode    string `json:"audioMode"`
	AudioQuality string `json:"audioQuality"`
	Manifest     string `json:"manifest"`
	ManifestType string `json:"manifestMimeType"`
}

// TidalInfoResponse wraps track info with version
type TidalInfoResponse struct {
	Version string                 `json:"version"`
	Data    TidalHifiTrackResponse `json:"data"`
}

// TidalStreamDataResponse wraps stream response with version
type TidalStreamDataResponse struct {
	Version string              `json:"version"`
	Data    TidalStreamResponse `json:"data"`
}

// StreamInfo contains information about the audio stream returned by Tidal
type StreamInfo struct {
	URL          string   `json:"url"`
	URLs         []string `json:"urls,omitempty"`   // All segment URLs (for segmented streams)
	Segmented    bool     `json:"segmented,omitempty"`
	AudioQuality string   `json:"audioQuality"` // HI_RES, LOSSLESS, HIGH, etc.
	AudioMode    string   `json:"audioMode"`    // STEREO, DOLBY_ATMOS, etc.
}

// DownloadResult represents the result of a download
type DownloadResult struct {
	TrackID          int             `json:"trackId"`
	Title            string          `json:"title"`
	Artist           string          `json:"artist"`
	Album            string          `json:"album"`
	FilePath         string          `json:"filePath"`
	FileSize         int64           `json:"fileSize"`
	Quality          string          `json:"quality"`
	RequestedQuality string          `json:"requestedQuality,omitempty"` // Quality that was requested
	QualityMismatch  bool            `json:"qualityMismatch,omitempty"`  // True if server returned different quality
	CoverURL         string          `json:"coverUrl"`
	Success          bool            `json:"success"`
	Error            string          `json:"error,omitempty"`
	Analysis         *AnalysisResult `json:"analysis,omitempty"` // Auto-analysis result if enabled
	Source           string          `json:"source,omitempty"`   // Which source served this track (e.g. "tidal", "qobuz")
	BytesDownloaded  int64           `json:"bytesDownloaded,omitempty"` // Bytes downloaded so far (progress)
	BytesTotal       int64           `json:"bytesTotal,omitempty"`      // Total bytes expected
	Speed            int64           `json:"speed,omitempty"`           // Download speed in bytes/sec
}

// DownloadOptions configures download behavior
type DownloadOptions struct {
	Quality              string   // "HI_RES", "LOSSLESS", "HIGH"
	FileNameFormat       string   // "{artist} - {title}", "{track} - {title}", etc.
	OrganizeFolders      bool     // Create Artist/Album/ subfolders
	FolderTemplate       string   // Folder structure template e.g. "{artist}/{album}"
	EmbedCover           bool     // Embed cover art in FLAC
	SaveCoverFile        bool     // Save cover art as .jpg file next to FLAC
	AutoAnalyze          bool     // Automatically analyze quality after download
	AutoQualityFallback  bool     // Retry with lower quality when requested quality is unavailable
	QualityFallbackOrder []string // Custom quality priority order; nil = use default chain
	FirstArtistOnly      bool     // Use only the first artist in tags and filenames
	SkipExisting         bool     // Skip files already on disk (matched by ISRC)
	ArtistSeparator      string   // Separator for multiple artists (default "; ")
	PlaylistSubfolder    bool     // Create subfolder for playlist downloads
	SaveLyricsFile       bool     // Save lyrics as separate .lrc file
	SaveFolderCover      bool     // Save folder.jpg in album directory
	SeparateSingles      bool     // Separate singles from albums into different folders
}

// qualityFallbackChain defines the descending quality order used for auto-fallback.
var qualityFallbackChain = []string{"HI_RES", "LOSSLESS", "HIGH"}

// NewTidalHifiService creates a new Tidal HiFi download service
func NewTidalHifiService() *TidalHifiService {
	// Use fallback transport so DoH resolves sinkholed proxy domains
	downloadTransport := NewFallbackTransport()
	downloadTransport.MaxIdleConns = 10
	downloadTransport.MaxIdleConnsPerHost = 5

	svc := &TidalHifiService{
		pool:     NewEndpointPool(defaultTidalHifiEndpoints, 5*time.Minute),
		parallel: true,
		downloadClient: &http.Client{
			Timeout:   0, // No timeout for downloads
			Transport: downloadTransport,
		},
		options: DownloadOptions{
			Quality:         "HI_RES",
			FileNameFormat:  "{artist} - {title}",
			OrganizeFolders: false,
			EmbedCover:      true,
			SaveCoverFile:   true,
		},
	}
	return svc
}

// SetLogger attaches a log buffer so endpoint rotation events are visible in the Terminal page.
func (t *TidalHifiService) SetLogger(logger *LogBuffer) {
	t.logger = logger
	t.pool.SetLogger(logger)
}

// SetProxy configures both API and download HTTP clients to use the given proxy.
// Supported schemes: http://, https://, socks5://.
// Pass an empty string to remove the proxy.
func (t *TidalHifiService) SetProxy(proxyURLStr string) error {
	transport, err := BuildProxyTransport(proxyURLStr)
	if err != nil {
		return err
	}
	// When no proxy is configured, keep the DoH fallback dialer so sinkholed
	// proxy domains still resolve correctly via Cloudflare/Google DoH.
	if transport == nil {
		transport = NewFallbackTransport()
	}
	apiClient := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}
	t.pool.SetClient(apiClient)
	t.downloadClient = &http.Client{
		Timeout:   0,
		Transport: transport,
	}
	return nil
}

// SetParallel controls whether the endpoint pool races all endpoints in parallel.
func (t *TidalHifiService) SetParallel(parallel bool) {
	t.parallel = parallel
}

// SetEndpoints replaces the endpoint pool with a custom list (e.g. from config).
// An empty/nil slice reverts to the built-in defaults.
func (t *TidalHifiService) SetEndpoints(urls []string) {
	if len(urls) == 0 {
		urls = defaultTidalHifiEndpoints
	}
	t.pool.SetEndpoints(urls)
}

// makeAPIRequest tries endpoints via the pool until one succeeds.
func (t *TidalHifiService) makeAPIRequest(path string) ([]byte, error) {
	ctx := context.Background()
	var result *PoolResult
	var err error
	if t.parallel {
		result, err = t.pool.RaceRequest(ctx, path)
	} else {
		result, err = t.pool.SequentialRequest(ctx, path)
	}
	if err != nil {
		return nil, fmt.Errorf("all Tidal endpoints failed: %w", err)
	}
	return result.Body, nil
}

// SetOptions updates download options
func (t *TidalHifiService) SetOptions(opts DownloadOptions) {
	t.options = opts
}

// GetOptions returns current download options
func (t *TidalHifiService) GetOptions() DownloadOptions {
	return t.options
}

// IsAvailable checks if the service is reachable
func (t *TidalHifiService) IsAvailable() bool {
	healthy := t.pool.GetHealthy()
	if len(healthy) == 0 {
		healthy = []string{tidalHifiAPIBase}
	}
	// Use the pool's own client via a lightweight HEAD on the first healthy endpoint
	ctx := context.Background()
	result, err := t.pool.SequentialRequest(ctx, "/")
	return err == nil && result != nil
}

// GetTrackAsTidalTrack fetches track info by Tidal ID and returns a TidalTrack.
// Used by FetchTidalContent when Tidal v1 client credentials are revoked.
func (t *TidalHifiService) GetTrackAsTidalTrack(trackID int) (*TidalTrack, error) {
	info, err := t.GetTrackByID(trackID)
	if err != nil {
		return nil, err
	}

	mainArtist := info.Artist.Name
	var artistNames []string
	for _, a := range info.Artists {
		artistNames = append(artistNames, a.Name)
	}
	if mainArtist == "" && len(info.Artists) > 0 {
		mainArtist = info.Artists[0].Name
	}

	// Extract album artist
	albumArtist := ""
	if info.Album.Artist.Name != "" {
		albumArtist = info.Album.Artist.Name
	} else if len(info.Album.Artists) > 0 {
		for _, a := range info.Album.Artists {
			if a.Type == "MAIN" || albumArtist == "" {
				albumArtist = a.Name
			}
		}
	}

	return &TidalTrack{
		ID:          info.ID,
		Title:       info.Title,
		Artist:      mainArtist,
		Artists:     strings.Join(artistNames, ", "),
		AlbumArtist: albumArtist,
		Album:       info.Album.Title,
		ISRC:        info.ISRC,
		Duration:    info.Duration,
		TrackNum:    info.TrackNumber,
		DiscNum:     info.VolumeNumber,
		TotalDiscs:  info.Album.NumberOfVolumes,
		ReleaseDate: info.Album.ReleaseDate,
		CoverURL:    formatTidalImageURL(info.Album.Cover),
		Explicit:    info.Explicit,
		TidalURL:    fmt.Sprintf("https://tidal.com/browse/track/%d", info.ID),
	}, nil
}

// GetTrackByID fetches track info by Tidal ID
func (t *TidalHifiService) GetTrackByID(trackID int) (*TidalHifiTrackResponse, error) {
	body, err := t.makeAPIRequest(fmt.Sprintf("/info/?id=%d", trackID))
	if err != nil {
		return nil, fmt.Errorf("info request failed: %w", err)
	}

	// Try v2.0 wrapper format first
	var infoResp TidalInfoResponse
	if err := json.Unmarshal(body, &infoResp); err != nil {
		return nil, fmt.Errorf("failed to parse track info: %w", err)
	}

	// Check if we got data from the wrapper
	if infoResp.Data.ID > 0 {
		return &infoResp.Data, nil
	}

	// Fallback: try direct format
	var trackInfo TidalHifiTrackResponse
	if err := json.Unmarshal(body, &trackInfo); err != nil {
		return nil, fmt.Errorf("failed to parse track info (direct): %w", err)
	}

	return &trackInfo, nil
}

// GetStreamURL fetches the stream URL using the configured quality setting.
func (t *TidalHifiService) GetStreamURL(trackID int) (*StreamInfo, error) {
	quality := t.options.Quality
	if quality == "" {
		quality = "LOSSLESS"
	}
	return t.getStreamURLForQuality(trackID, quality)
}

// parseStreamBody parses a raw stream/manifest API response into StreamInfo.
func (t *TidalHifiService) parseStreamBody(body []byte, quality string, trackID int) (*StreamInfo, error) {
	// Try v2.0 wrapper format first
	var streamDataResp TidalStreamDataResponse
	if err := json.Unmarshal(body, &streamDataResp); err != nil {
		return nil, fmt.Errorf("failed to parse stream response: %w", err)
	}

	audioQuality := streamDataResp.Data.AudioQuality
	audioMode := streamDataResp.Data.AudioMode
	manifestBase64 := streamDataResp.Data.Manifest

	if manifestBase64 == "" {
		// Fallback: try direct format
		var streamResp TidalStreamResponse
		if err := json.Unmarshal(body, &streamResp); err != nil {
			return nil, fmt.Errorf("failed to parse stream response (direct): %w", err)
		}
		manifestBase64 = streamResp.Manifest
		audioQuality = streamResp.AudioQuality
		audioMode = streamResp.AudioMode
	}

	if manifestBase64 == "" {
		return nil, fmt.Errorf("no manifest in stream response for quality %s", quality)
	}

	manifestBytes, err := base64.StdEncoding.DecodeString(manifestBase64)
	if err != nil {
		return nil, fmt.Errorf("failed to decode manifest: %w", err)
	}

	manifest, err := ParseManifest(manifestBytes)
	if err != nil {
		return nil, fmt.Errorf("quality %s unavailable for track %d: %w", quality, trackID, err)
	}

	return &StreamInfo{
		URL:          manifest.URLs[0],
		URLs:         manifest.URLs,
		Segmented:    manifest.Segmented,
		AudioQuality: audioQuality,
		AudioMode:    audioMode,
	}, nil
}

// getStreamURLForQuality fetches the stream URL requesting a specific quality level.
// It iterates all endpoints and prefers one that returns the exact quality requested.
// If no endpoint matches exactly, the best available downgrade is returned.
func (t *TidalHifiService) getStreamURLForQuality(trackID int, quality string) (*StreamInfo, error) {
	// Normalize UI labels to valid Tidal API parameters
	switch quality {
	case "HI_RES_LOSSLESS", "HI_RES_MAX":
		quality = "HI_RES"
	}

	path := fmt.Sprintf("/track/?id=%d&quality=%s", trackID, quality)
	ctx := context.Background()
	client := t.pool.GetClient()

	toTry := t.pool.GetAvailable()
	if len(toTry) == 0 {
		toTry = []string{tidalHifiAPIBase}
	}

	var bestDowngrade *StreamInfo
	var lastErr error

	for _, endpoint := range toTry {
		body, err := singleEndpointRequest(ctx, client, endpoint, path)
		if err != nil {
			lastErr = err
			t.pool.Blacklist(endpoint)
			if t.logger != nil {
				t.logger.Warn(fmt.Sprintf("Tidal endpoint %s failed: %v", endpoint, err))
			}
			continue
		}

		info, err := t.parseStreamBody(body, quality, trackID)
		if err != nil {
			lastErr = err
			t.pool.Blacklist(endpoint)
			continue
		}

		// Exact quality match — use this endpoint
		if info.AudioQuality == quality || info.AudioQuality == "" {
			return info, nil
		}

		// Quality mismatch — save as fallback, try next endpoint
		if bestDowngrade == nil {
			bestDowngrade = info
			if t.logger != nil {
				t.logger.Warn(fmt.Sprintf("endpoint %s returned %s instead of requested %s for track %d, trying other endpoints",
					endpoint, info.AudioQuality, quality, trackID))
			}
		}
	}

	// No endpoint returned exact quality — accept best downgrade if available
	if bestDowngrade != nil {
		if t.logger != nil {
			t.logger.Warn(fmt.Sprintf("no endpoint supports %s for track %d, accepting %s",
				quality, trackID, bestDowngrade.AudioQuality))
		}
		return bestDowngrade, nil
	}

	return nil, fmt.Errorf("stream request failed for quality %s: %v", quality, lastErr)
}

// singleEndpointRequest performs a single authenticated GET against one endpoint+path.
func singleEndpointRequest(ctx context.Context, client *http.Client, endpoint, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", endpoint+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 500 {
		return nil, fmt.Errorf("server returned %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

// getStreamURLWithFallback tries the configured quality first, then walks down
// the quality order until a stream URL is obtained.
// Returns the StreamInfo and the quality level that succeeded.
func (t *TidalHifiService) getStreamURLWithFallback(trackID int, startQuality string) (*StreamInfo, string, error) {
	chain := qualityFallbackChain
	if len(t.options.QualityFallbackOrder) > 0 {
		chain = t.options.QualityFallbackOrder
	}

	// Find the starting position
	startIdx := 0
	for i, q := range chain {
		if q == startQuality {
			startIdx = i
			break
		}
	}

	var lastErr error
	for _, quality := range chain[startIdx:] {
		streamInfo, err := t.getStreamURLForQuality(trackID, quality)
		if err == nil {
			if quality != startQuality && t.logger != nil {
				t.logger.Warn(fmt.Sprintf("%s unavailable for track %d, falling back to %s",
					startQuality, trackID, quality))
			}
			return streamInfo, quality, nil
		}
		lastErr = err
	}
	return nil, "", fmt.Errorf("all quality levels failed: %v", lastErr)
}

// searchQueryVariants returns the original query plus a normalized variant for
// short all-alpha queries that may represent compressed artist names (e.g. "acdc" → "ac/dc").
// The variant is only added when it differs from the original.
func searchQueryVariants(query string) []string {
	variants := []string{query}

	// Only generate variants for short queries with no spaces or slashes
	q := strings.ToLower(strings.TrimSpace(query))
	if strings.ContainsAny(q, " /\\-") || len(q) < 3 || len(q) > 8 {
		return variants
	}

	// Check it's all alphabetic (band abbreviations like acdc, rhcp, ratm…)
	allAlpha := true
	for _, c := range q {
		if c < 'a' || c > 'z' {
			allAlpha = false
			break
		}
	}
	if !allAlpha {
		return variants
	}

	// Try inserting "/" at the midpoint (acdc → ac/dc)
	mid := len(q) / 2
	variant := q[:mid] + "/" + q[mid:]
	variants = append(variants, variant)
	return variants
}

// mergeTrackResults merges two slices of track results, deduplicating by track ID.
// Tracks with ID=0 are deduplicated by title+artist instead.
func mergeTrackResults(a, b []TidalHifiTrackResponse) []TidalHifiTrackResponse {
	seen := make(map[string]struct{}, len(a))
	out := make([]TidalHifiTrackResponse, 0, len(a)+len(b))
	key := func(t TidalHifiTrackResponse) string {
		if t.ID != 0 {
			return fmt.Sprintf("id:%d", t.ID)
		}
		return "t:" + strings.ToLower(t.Title+"|"+t.Artist.Name)
	}
	for _, t := range a {
		k := key(t)
		if _, exists := seen[k]; !exists {
			seen[k] = struct{}{}
			out = append(out, t)
		}
	}
	for _, t := range b {
		k := key(t)
		if _, exists := seen[k]; !exists {
			seen[k] = struct{}{}
			out = append(out, t)
		}
	}
	return out
}

// parseSearchBody extracts track items from a Tidal search response body.
func parseSearchBody(body []byte) ([]TidalHifiTrackResponse, error) {
	var result struct {
		Version string `json:"version,omitempty"`
		Data    struct {
			Items []TidalHifiTrackResponse `json:"items"`
		} `json:"data,omitempty"`
		Tracks struct {
			Items []TidalHifiTrackResponse `json:"items"`
		} `json:"tracks,omitempty"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse search response: %w", err)
	}
	if len(result.Data.Items) > 0 {
		return result.Data.Items, nil
	}
	return result.Tracks.Items, nil
}

// SearchTrack searches for a track on Tidal via vogel
func (t *TidalHifiService) SearchTrack(query string) (*TidalHifiTrackResponse, error) {
	body, err := t.makeAPIRequest("/search/?s=" + url.QueryEscape(query))
	if err != nil {
		return nil, fmt.Errorf("search request failed: %w", err)
	}

	items, err := parseSearchBody(body)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no tracks found for query: %s", query)
	}
	return &items[0], nil
}

// SearchTracks searches for tracks on Tidal via vogel and returns multiple results.
// For short all-alpha queries (e.g. "acdc"), also tries a normalized variant (e.g. "ac/dc")
// and merges the results so that artist-based matches are included.
func (t *TidalHifiService) SearchTracks(query string, limit int) ([]TidalHifiTrackResponse, error) {
	if limit <= 0 {
		limit = 20
	}

	var merged []TidalHifiTrackResponse
	for _, q := range searchQueryVariants(query) {
		body, err := t.makeAPIRequest("/search/?s=" + url.QueryEscape(q))
		if err != nil {
			continue
		}
		items, err := parseSearchBody(body)
		if err != nil {
			continue
		}
		merged = mergeTrackResults(merged, items)
	}
	if len(merged) == 0 {
		return nil, fmt.Errorf("search request failed for query: %s", query)
	}
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

// SearchAlbumsFromProxy searches for albums via the proxy by extracting unique albums
// from track search results. Used as a fallback when the Tidal v1 API credentials are revoked.
func (t *TidalHifiService) SearchAlbumsFromProxy(query string, limit int) ([]TidalAlbum, error) {
	if limit <= 0 {
		limit = 20
	}

	var items []TidalHifiTrackResponse
	for _, q := range searchQueryVariants(query) {
		body, err := t.makeAPIRequest("/search/?s=" + url.QueryEscape(q))
		if err != nil {
			continue
		}
		got, err := parseSearchBody(body)
		if err != nil {
			continue
		}
		items = mergeTrackResults(items, got)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("album search failed for query: %s", query)
	}

	seen := make(map[string]struct{})
	albums := make([]TidalAlbum, 0)
	for _, track := range items {
		key := track.Album.Title + "|" + track.Album.Artist.Name
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}

		artistName := track.Album.Artist.Name
		if artistName == "" && len(track.Album.Artists) > 0 {
			artistName = track.Album.Artists[0].Name
		}
		if artistName == "" {
			artistName = track.Artist.Name
		}

		albums = append(albums, TidalAlbum{
			ID:          track.Album.ID,
			Title:       track.Album.Title,
			Artist:      artistName,
			ReleaseDate: track.Album.ReleaseDate,
			TrackCount:  track.Album.NumberOfTracks,
			CoverURL:    formatTidalImageURL(track.Album.Cover),
		})

		if len(albums) >= limit {
			break
		}
	}

	return albums, nil
}

// SearchArtistsFromProxy searches for artists via the proxy by extracting unique artists
// from track search results. Used as a fallback when the Tidal v1 API credentials are revoked.
func (t *TidalHifiService) SearchArtistsFromProxy(query string, limit int) ([]TidalArtist, error) {
	if limit <= 0 {
		limit = 20
	}

	var items []TidalHifiTrackResponse
	for _, q := range searchQueryVariants(query) {
		body, err := t.makeAPIRequest("/search/?s=" + url.QueryEscape(q))
		if err != nil {
			continue
		}
		got, err := parseSearchBody(body)
		if err != nil {
			continue
		}
		items = mergeTrackResults(items, got)
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("artist search failed for query: %s", query)
	}

	seen := make(map[string]struct{})
	artists := make([]TidalArtist, 0)
	for _, track := range items {
		name := track.Artist.Name
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}

		artists = append(artists, TidalArtist{
			Name: name,
		})

		if len(artists) >= limit {
			break
		}
	}

	return artists, nil
}

// makeMetadataRequest tries each v2.4 metadata endpoint for album/playlist/mix paths.
// Only HTTP 200 responses are considered successful — 4xx responses indicate the
// endpoint does not support the requested path and cause fallthrough to the next.
func (t *TidalHifiService) makeMetadataRequest(path string) ([]byte, error) {
	var lastErr error
	for _, endpoint := range defaultMetadataEndpoints {
		req, err := http.NewRequest("GET", endpoint+path, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

		resp, err := t.pool.GetClient().Do(req)
		if err != nil {
			lastErr = fmt.Errorf("request to %s failed: %w", endpoint, err)
			if t.logger != nil {
				t.logger.Warn(fmt.Sprintf("metadata endpoint %s failed: %v", endpoint, err))
			}
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == 200 {
			return body, nil
		}
		lastErr = fmt.Errorf("%s returned %d", endpoint, resp.StatusCode)
		if t.logger != nil {
			t.logger.Warn(fmt.Sprintf("metadata endpoint %s returned %d, trying next", endpoint, resp.StatusCode))
		}
	}
	return nil, fmt.Errorf("all metadata endpoints failed: %v", lastErr)
}

// GetAlbumFromProxy fetches album metadata and tracks via the v2.4 proxy endpoint pool.
// Used instead of TidalClient.GetAlbum (Tidal v1 client credentials are revoked).
func (t *TidalHifiService) GetAlbumFromProxy(albumID string) (*TidalAlbum, error) {
	body, err := t.makeMetadataRequest("/album/?id=" + albumID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch album: %w", err)
	}

	var resp struct {
		Data struct {
			ID              int    `json:"id"`
			Title           string `json:"title"`
			Cover           string `json:"cover"`
			ReleaseDate     string `json:"releaseDate"`
			Type            string `json:"type"`
			Copyright       string `json:"copyright"`
			NumberOfVolumes int    `json:"numberOfVolumes"`
			Artists         []struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"artists"`
			Label struct {
				Name string `json:"name"`
			} `json:"label"`
			Items []struct {
				Item struct {
					ID           int    `json:"id"`
					Title        string `json:"title"`
					Duration     int    `json:"duration"`
					ISRC         string `json:"isrc"`
					Explicit     bool   `json:"explicit"`
					StreamReady  *bool  `json:"streamReady"`
					Popularity   int    `json:"popularity"`
					TrackNumber  int    `json:"trackNumber"`
					VolumeNumber int    `json:"volumeNumber"`
					Artists      []struct {
						Name string `json:"name"`
					} `json:"artists"`
					Album struct {
						ID    int    `json:"id"`
						Title string `json:"title"`
						Cover string `json:"cover"`
					} `json:"album"`
				} `json:"item"`
			} `json:"items"`
		} `json:"data"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse album response: %w", err)
	}

	d := resp.Data
	artistName := ""
	for _, a := range d.Artists {
		if a.Type == "MAIN" || artistName == "" {
			artistName = a.Name
		}
	}

	tracks := make([]TidalTrack, 0, len(d.Items))
	for _, it := range d.Items {
		tr := it.Item
		var artistNames []string
		for _, a := range tr.Artists {
			artistNames = append(artistNames, a.Name)
		}
		mainArtist := ""
		if len(tr.Artists) > 0 {
			mainArtist = tr.Artists[0].Name
		}
		available := tr.StreamReady == nil || *tr.StreamReady
		tracks = append(tracks, TidalTrack{
			ID:          tr.ID,
			Title:       tr.Title,
			Artist:      mainArtist,
			Artists:     strings.Join(artistNames, ", "),
			AlbumArtist: artistName,
			Album:       tr.Album.Title,
			AlbumID:     tr.Album.ID,
			ISRC:        tr.ISRC,
			Duration:    tr.Duration,
			TrackNum:    tr.TrackNumber,
			DiscNum:     tr.VolumeNumber,
			TotalDiscs:  d.NumberOfVolumes,
			ReleaseDate: d.ReleaseDate,
			CoverURL:    formatTidalImageURL(tr.Album.Cover),
			Explicit:    tr.Explicit,
			TidalURL:    fmt.Sprintf("https://tidal.com/browse/track/%d", tr.ID),
			Available:   available,
			Popularity:  tr.Popularity,
		})
	}

	return &TidalAlbum{
		ID:          d.ID,
		Title:       d.Title,
		Artist:      artistName,
		ReleaseDate: d.ReleaseDate,
		TrackCount:  len(tracks),
		CoverURL:    formatTidalImageURL(d.Cover),
		AlbumType:   d.Type,
		Copyright:   d.Copyright,
		Label:       d.Label.Name,
		Tracks:      tracks,
	}, nil
}

// playlistProxyResponse is the JSON structure returned by the v2.4 proxy playlist endpoint.
type playlistProxyResponse struct {
	Playlist struct {
		UUID           string `json:"uuid"`
		Title          string `json:"title"`
		Description    string `json:"description"`
		NumberOfTracks int    `json:"numberOfTracks"`
		Creator        struct {
			Name string `json:"name"`
		} `json:"creator"`
		Image       string `json:"image"`
		SquareImage string `json:"squareImage"`
	} `json:"playlist"`
	Items []struct {
		Item struct {
			ID          int    `json:"id"`
			Title       string `json:"title"`
			Duration    int    `json:"duration"`
			ISRC        string `json:"isrc"`
			Explicit    bool   `json:"explicit"`
			StreamReady *bool  `json:"streamReady"`
			Popularity  int    `json:"popularity"`
			TrackNumber int    `json:"trackNumber"`
			Artists     []struct {
				Name string `json:"name"`
			} `json:"artists"`
			Album struct {
				ID    int    `json:"id"`
				Title string `json:"title"`
				Cover string `json:"cover"`
			} `json:"album"`
		} `json:"item"`
	} `json:"items"`
	TotalNumberOfItems int `json:"totalNumberOfItems"`
}

// parsePlaylistItems converts proxy response items to TidalTrack slice.
func parsePlaylistItems(items []struct {
	Item struct {
		ID          int    `json:"id"`
		Title       string `json:"title"`
		Duration    int    `json:"duration"`
		ISRC        string `json:"isrc"`
		Explicit    bool   `json:"explicit"`
		StreamReady *bool  `json:"streamReady"`
		Popularity  int    `json:"popularity"`
		TrackNumber int    `json:"trackNumber"`
		Artists     []struct {
			Name string `json:"name"`
		} `json:"artists"`
		Album struct {
			ID    int    `json:"id"`
			Title string `json:"title"`
			Cover string `json:"cover"`
		} `json:"album"`
	} `json:"item"`
}) []TidalTrack {
	tracks := make([]TidalTrack, 0, len(items))
	for _, it := range items {
		tr := it.Item
		var artistNames []string
		for _, a := range tr.Artists {
			artistNames = append(artistNames, a.Name)
		}
		mainArtist := ""
		if len(tr.Artists) > 0 {
			mainArtist = tr.Artists[0].Name
		}
		available := tr.StreamReady == nil || *tr.StreamReady
		tracks = append(tracks, TidalTrack{
			ID:         tr.ID,
			Title:      tr.Title,
			Artist:     mainArtist,
			Artists:    strings.Join(artistNames, ", "),
			Album:      tr.Album.Title,
			AlbumID:    tr.Album.ID,
			ISRC:       tr.ISRC,
			Duration:   tr.Duration,
			TrackNum:   tr.TrackNumber,
			CoverURL:   formatTidalImageURL(tr.Album.Cover),
			Explicit:   tr.Explicit,
			TidalURL:   fmt.Sprintf("https://tidal.com/browse/track/%d", tr.ID),
			Available:  available,
			Popularity: tr.Popularity,
		})
	}
	return tracks
}

// GetPlaylistFromProxy fetches playlist metadata and tracks via the v2.4 proxy endpoint pool.
// Paginates automatically for playlists with more than 100 tracks.
func (t *TidalHifiService) GetPlaylistFromProxy(playlistUUID string) (*TidalPlaylist, error) {
	// First request — gets metadata + first batch of items (explicit limit to avoid proxy defaults)
	body, err := t.makeMetadataRequest(fmt.Sprintf("/playlist/?id=%s&limit=100&offset=0", playlistUUID))
	if err != nil {
		return nil, fmt.Errorf("failed to fetch playlist: %w", err)
	}

	var resp playlistProxyResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse playlist response: %w", err)
	}

	p := resp.Playlist
	coverImage := p.SquareImage
	if coverImage == "" {
		coverImage = p.Image
	}
	creatorName := p.Creator.Name
	if creatorName == "" {
		creatorName = "Tidal Playlist"
	}
	uuid := p.UUID
	if uuid == "" {
		uuid = playlistUUID
	}

	allTracks := parsePlaylistItems(resp.Items)

	// Determine total — use whichever source reports more tracks
	totalTracks := resp.TotalNumberOfItems
	if p.NumberOfTracks > totalTracks {
		totalTracks = p.NumberOfTracks
	}

	// Paginate if we got fewer items than total
	limit := 100
	offset := len(resp.Items)
	for offset < totalTracks {
		pageBody, err := t.makeMetadataRequest(
			fmt.Sprintf("/playlist/?id=%s&limit=%d&offset=%d", playlistUUID, limit, offset),
		)
		if err != nil {
			if t.logger != nil {
				t.logger.Warn(fmt.Sprintf("Playlist pagination stopped at offset %d/%d: %v", offset, totalTracks, err))
			}
			break
		}

		var pageResp playlistProxyResponse
		if err := json.Unmarshal(pageBody, &pageResp); err != nil {
			if t.logger != nil {
				t.logger.Warn(fmt.Sprintf("Playlist pagination parse error at offset %d: %v", offset, err))
			}
			break
		}

		pageTracks := parsePlaylistItems(pageResp.Items)
		if len(pageTracks) == 0 {
			if t.logger != nil {
				t.logger.Warn(fmt.Sprintf("Playlist pagination: empty page at offset %d/%d", offset, totalTracks))
			}
			break
		}
		allTracks = append(allTracks, pageTracks...)
		offset += len(pageResp.Items)
	}

	if t.logger != nil && len(allTracks) < totalTracks {
		t.logger.Warn(fmt.Sprintf("Playlist incomplete: got %d/%d tracks", len(allTracks), totalTracks))
	}

	return &TidalPlaylist{
		UUID:        uuid,
		Title:       p.Title,
		Description: p.Description,
		Creator:     creatorName,
		CoverURL:    formatTidalImageURL(coverImage),
		TrackCount:  len(allTracks),
		Tracks:      allTracks,
	}, nil
}

// GetMixFromProxy fetches mix metadata and tracks via the v2.4 proxy endpoint pool.
// Used instead of TidalClient.GetMix (Tidal v1 client credentials are revoked).
func (t *TidalHifiService) GetMixFromProxy(mixID string) (*TidalPlaylist, error) {
	body, err := t.makeMetadataRequest("/mix/?id=" + mixID)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch mix: %w", err)
	}

	var resp struct {
		Mix struct {
			ID       string `json:"id"`
			Title    string `json:"title"`
			SubTitle string `json:"subTitle"`
			Images   map[string]struct {
				URL string `json:"url"`
			} `json:"images"`
		} `json:"mix"`
		Items []struct {
			ID          int    `json:"id"`
			Title       string `json:"title"`
			Duration    int    `json:"duration"`
			ISRC        string `json:"isrc"`
			Explicit    bool   `json:"explicit"`
			StreamReady *bool  `json:"streamReady"`
			Popularity  int    `json:"popularity"`
			TrackNumber int    `json:"trackNumber"`
			Artists     []struct {
				Name string `json:"name"`
			} `json:"artists"`
			Album struct {
				ID    int    `json:"id"`
				Title string `json:"title"`
				Cover string `json:"cover"`
			} `json:"album"`
		} `json:"items"`
	}

	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse mix response: %w", err)
	}

	// Extract cover URL — prefer MEDIUM, then LARGE, then SMALL
	coverURL := ""
	for _, size := range []string{"MEDIUM", "LARGE", "SMALL"} {
		if img, ok := resp.Mix.Images[size]; ok && img.URL != "" {
			coverURL = img.URL
			break
		}
	}

	mixTitle := resp.Mix.Title
	if mixTitle == "" {
		mixTitle = "Tidal Mix"
	}

	tracks := make([]TidalTrack, 0, len(resp.Items))
	for _, tr := range resp.Items {
		var artistNames []string
		for _, a := range tr.Artists {
			artistNames = append(artistNames, a.Name)
		}
		mainArtist := ""
		if len(tr.Artists) > 0 {
			mainArtist = tr.Artists[0].Name
		}
		available := tr.StreamReady == nil || *tr.StreamReady
		tracks = append(tracks, TidalTrack{
			ID:         tr.ID,
			Title:      tr.Title,
			Artist:     mainArtist,
			Artists:    strings.Join(artistNames, ", "),
			Album:      tr.Album.Title,
			AlbumID:    tr.Album.ID,
			ISRC:       tr.ISRC,
			Duration:   tr.Duration,
			TrackNum:   tr.TrackNumber,
			CoverURL:   formatTidalImageURL(tr.Album.Cover),
			Explicit:   tr.Explicit,
			TidalURL:   fmt.Sprintf("https://tidal.com/browse/track/%d", tr.ID),
			Available:  available,
			Popularity: tr.Popularity,
		})
	}

	return &TidalPlaylist{
		UUID:       mixID,
		Title:      mixTitle,
		Creator:    "Tidal Mix",
		CoverURL:   coverURL,
		TrackCount: len(tracks),
		Tracks:     tracks,
	}, nil
}

// DownloadTrack downloads a single track to the specified directory
func (t *TidalHifiService) DownloadTrack(trackID int, outputDir string, copyright, label string) (*DownloadResult, error) {
	result := &DownloadResult{
		TrackID: trackID,
		Success: false,
		Source:  "tidal",
	}

	// Get track info
	track, err := t.GetTrackByID(trackID)
	if err != nil {
		result.Error = fmt.Sprintf("failed to get track info: %v", err)
		return result, err
	}

	// Build artist name
	artistName := track.Artist.Name
	if t.options.FirstArtistOnly && len(track.Artists) > 0 {
		artistName = track.Artists[0].Name
	} else if artistName == "" && len(track.Artists) > 0 {
		artistName = track.Artists[0].Name
	}

	// Build multi-artist string with configured separator
	separator := t.options.ArtistSeparator
	if separator == "" {
		separator = "; "
	}
	if !t.options.FirstArtistOnly && len(track.Artists) > 1 {
		var names []string
		for _, a := range track.Artists {
			names = append(names, a.Name)
		}
		artistName = strings.Join(names, separator)
	}

	// Extract album artist
	albumArtist := ""
	if track.Album.Artist.Name != "" {
		albumArtist = track.Album.Artist.Name
	} else if len(track.Album.Artists) > 0 {
		for _, a := range track.Album.Artists {
			if a.Type == "MAIN" || albumArtist == "" {
				albumArtist = a.Name
			}
		}
	}
	if albumArtist == "" {
		albumArtist = artistName
	}

	result.Title = track.Title
	result.Artist = artistName
	result.Album = track.Album.Title

	coverURL := ""
	if track.Album.Cover != "" {
		coverURL = fmt.Sprintf("https://resources.tidal.com/images/%s/1280x1280.jpg",
			strings.ReplaceAll(track.Album.Cover, "-", "/"))
		result.CoverURL = coverURL
	}

	// Determine output path based on options
	finalDir := outputDir
	if t.options.SeparateSingles {
		// Separate singles (1 track) from albums
		isSingle := track.Album.NumberOfTracks <= 1
		if isSingle {
			safeArtist := SanitizeFileName(artistName)
			finalDir = filepath.Join(outputDir, "Singles", safeArtist)
		} else {
			safeArtist := SanitizeFileName(artistName)
			safeAlbum := SanitizeFileName(track.Album.Title)
			if safeAlbum == "" {
				safeAlbum = "Unknown Album"
			}
			finalDir = filepath.Join(outputDir, "Albums", safeArtist, safeAlbum)
		}
	} else if t.options.FolderTemplate != "" {
		// Template-based folder structure
		subDir := t.applyFolderTemplate(t.options.FolderTemplate, track, artistName)
		if subDir != "" {
			finalDir = filepath.Join(outputDir, subDir)
		}
	} else if t.options.OrganizeFolders {
		// Legacy: simple Artist/Album structure
		safeArtist := SanitizeFileName(artistName)
		safeAlbum := SanitizeFileName(track.Album.Title)
		if safeAlbum == "" {
			safeAlbum = "Singles"
		}
		finalDir = filepath.Join(outputDir, safeArtist, safeAlbum)
	}

	// Create output directory
	if err := os.MkdirAll(finalDir, 0755); err != nil {
		result.Error = fmt.Sprintf("failed to create output directory: %v", err)
		return result, err
	}

	// Generate filename based on format
	fileName := t.formatFileName(track, artistName, albumArtist)
	outputPath := filepath.Join(finalDir, fmt.Sprintf("%s.flac", fileName))
	result.FilePath = outputPath

	// Smart ISRC-based skip: check if a file with the same ISRC already exists in the output dir
	if t.options.SkipExisting && track.ISRC != "" {
		isrcMap, _ := ScanFolderISRCs(finalDir)
		if existingPath, found := isrcMap[track.ISRC]; found {
			stat, _ := os.Stat(existingPath)
			if stat != nil && stat.Size() > 0 {
				result.FilePath = existingPath
				result.FileSize = stat.Size()
				result.Success = true
				result.Error = "skipped: ISRC match"
				result.Quality = "existing"
				if t.logger != nil {
					t.logger.Info(fmt.Sprintf("Skipped %s - %s (ISRC %s already exists at %s)",
						artistName, track.Title, track.ISRC, filepath.Base(existingPath)))
				}
				return result, nil
			}
		}
	}

	// Check if file already exists by path (skip if already downloaded)
	if stat, err := os.Stat(outputPath); err == nil && stat.Size() > 0 {
		result.FileSize = stat.Size()
		result.Success = true
		result.Error = "skipped: already exists"
		return result, nil
	}

	// Get stream URL, with optional quality fallback
	requestedQuality := t.options.Quality
	if requestedQuality == "" {
		requestedQuality = "LOSSLESS"
	}

	var streamInfo *StreamInfo
	var actualQuality string
	if t.options.AutoQualityFallback {
		streamInfo, actualQuality, err = t.getStreamURLWithFallback(trackID, requestedQuality)
	} else {
		streamInfo, err = t.getStreamURLForQuality(trackID, requestedQuality)
		actualQuality = requestedQuality
	}
	if err != nil {
		result.Error = fmt.Sprintf("failed to get stream URL: %v", err)
		return result, err
	}

	result.RequestedQuality = requestedQuality
	result.Quality = streamInfo.AudioQuality

	// Log quality mismatch (server returned different quality than requested)
	if streamInfo.AudioQuality != "" && streamInfo.AudioQuality != requestedQuality && actualQuality == requestedQuality {
		result.QualityMismatch = true
		if t.logger != nil {
			t.logger.Warn(fmt.Sprintf("quality mismatch: requested %s but received %s for track %d (%s - %s)",
				requestedQuality, streamInfo.AudioQuality, trackID, artistName, track.Title))
		}
	}

	// Download the FLAC file
	ctx := context.Background()
	if streamInfo.Segmented && len(streamInfo.URLs) > 1 {
		if err := DownloadSegmented(ctx, streamInfo.URLs, outputPath, t.downloadClient, nil); err != nil {
			result.Error = fmt.Sprintf("segmented download failed: %v", err)
			return result, err
		}
	} else {
		if err := t.downloadFile(ctx, streamInfo.URL, outputPath); err != nil {
			result.Error = fmt.Sprintf("download failed: %v", err)
			return result, err
		}
	}

	// Tag the file with metadata
	tagger := NewFLACTagger()
	meta := TrackMetadata{
		Title:        track.Title,
		Artist:       artistName,
		AlbumArtist:  albumArtist,
		Album:        track.Album.Title,
		TrackNumber:  track.TrackNumber,
		DiscNumber:   track.VolumeNumber,
		TotalDiscs:   track.Album.NumberOfVolumes,
		Year:         track.Album.ReleaseDate,
		OriginalDate: track.Album.ReleaseDate,
		ISRC:         track.ISRC,
		Copyright:    copyright,
		Label:        label,
	}

	// Only embed cover if option is enabled
	if t.options.EmbedCover {
		meta.CoverURL = coverURL
	}

	if err := tagger.TagFile(outputPath, meta); err != nil {
		if t.logger != nil {
			t.logger.Warn("Failed to tag file: " + err.Error())
		}
	}

	// Save cover as separate .jpg file if enabled
	if t.options.SaveCoverFile && coverURL != "" {
		coverPath := strings.TrimSuffix(outputPath, ".flac") + ".jpg"
		if err := t.saveCoverFile(coverURL, coverPath); err != nil {
			if t.logger != nil {
				t.logger.Warn("Failed to save cover file: " + err.Error())
			}
		}
	}

	// Save folder.jpg if enabled (one per directory, skip if exists)
	if t.options.SaveFolderCover && coverURL != "" {
		folderCoverPath := filepath.Join(finalDir, "folder.jpg")
		if _, statErr := os.Stat(folderCoverPath); statErr != nil {
			if err := t.saveCoverFile(coverURL, folderCoverPath); err != nil {
				if t.logger != nil {
					t.logger.Warn("Failed to save folder cover: " + err.Error())
				}
			}
		}
	}

	// Auto-analyze the downloaded file if enabled
	if t.options.AutoAnalyze {
		analysis, err := AnalyzeFLAC(outputPath)
		if err == nil {
			result.Analysis = analysis
			if !analysis.IsTrueLossless && t.logger != nil {
				t.logger.Warn(fmt.Sprintf("%s may be upscaled from lossy source (verdict: %s)",
					filepath.Base(outputPath), analysis.VerdictLabel))
			}
		} else if t.logger != nil {
			t.logger.Warn("Failed to analyze file: " + err.Error())
		}
	}

	// Get file size
	stat, _ := os.Stat(outputPath)
	if stat != nil {
		result.FileSize = stat.Size()
	}

	result.Success = true
	return result, nil
}

func (t *TidalHifiService) downloadFile(ctx context.Context, downloadURL, outputPath string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", downloadURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create download request: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")

	resp, err := t.downloadClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to start download: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("download server returned %d", resp.StatusCode)
	}

	outFile, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer outFile.Close()

	var dst io.Writer = outFile
	if t.downloadProgress != nil {
		dst = &downloadProgressWriter{
			writer:     outFile,
			total:      resp.ContentLength,
			onProgress: t.downloadProgress,
			lastReport: time.Now(),
		}
	}

	_, err = io.Copy(dst, resp.Body)
	if err != nil {
		os.Remove(outputPath) // Clean up partial file
		return fmt.Errorf("download interrupted: %w", err)
	}

	// Final progress report so UI reaches 100%
	if t.downloadProgress != nil && resp.ContentLength > 0 {
		t.downloadProgress(resp.ContentLength, resp.ContentLength)
	}

	return nil
}

// saveCoverFile downloads and saves cover art as a separate .jpg file
func (t *TidalHifiService) saveCoverFile(coverURL, outputPath string) error {
	// Skip if file already exists
	if _, err := os.Stat(outputPath); err == nil {
		return nil
	}

	resp, err := http.Get(coverURL)
	if err != nil {
		return fmt.Errorf("failed to download cover: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("cover server returned %d", resp.StatusCode)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read cover data: %w", err)
	}

	return os.WriteFile(outputPath, data, 0644)
}

// SaveLyricsFile writes synced or plain lyrics to a file alongside the FLAC.
// Synced lyrics are saved as .lrc, plain lyrics as .txt.
func SaveLyricsFile(flacPath string, syncedLyrics, plainLyrics string) error {
	basePath := strings.TrimSuffix(flacPath, filepath.Ext(flacPath))

	if syncedLyrics != "" {
		lrcPath := basePath + ".lrc"
		if _, err := os.Stat(lrcPath); err != nil {
			if err := os.WriteFile(lrcPath, []byte(syncedLyrics), 0644); err != nil {
				return fmt.Errorf("failed to write .lrc file: %w", err)
			}
		}
		return nil
	}

	if plainLyrics != "" {
		txtPath := basePath + ".txt"
		if _, err := os.Stat(txtPath); err != nil {
			if err := os.WriteFile(txtPath, []byte(plainLyrics), 0644); err != nil {
				return fmt.Errorf("failed to write lyrics .txt file: %w", err)
			}
		}
	}
	return nil
}

// SanitizeFileName removes invalid characters from filenames
func SanitizeFileName(name string) string {
	if name == "" {
		return "Unknown"
	}

	// Remove characters invalid on Windows/Linux/macOS
	invalid := regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)
	sanitized := invalid.ReplaceAllString(name, "")

	// Replace multiple spaces with single space
	spaces := regexp.MustCompile(`\s+`)
	sanitized = spaces.ReplaceAllString(sanitized, " ")

	// Remove leading/trailing dots and spaces
	sanitized = strings.Trim(sanitized, ". ")

	// Limit length
	if len(sanitized) > 200 {
		sanitized = sanitized[:200]
	}

	if sanitized == "" {
		return "Unknown"
	}

	return sanitized
}

// formatFileName generates filename based on format template
func (t *TidalHifiService) formatFileName(track *TidalHifiTrackResponse, artistName, albumArtist string) string {
	format := t.options.FileNameFormat
	if format == "" {
		format = "{artist} - {title}"
	}

	// Extract year from release date (YYYY-MM-DD → YYYY)
	year := ""
	date := track.Album.ReleaseDate
	if len(date) >= 4 {
		year = date[:4]
	}

	// Replace placeholders
	result := format
	result = strings.ReplaceAll(result, "{artist}", artistName)
	result = strings.ReplaceAll(result, "{albumartist}", albumArtist)
	result = strings.ReplaceAll(result, "{title}", track.Title)
	result = strings.ReplaceAll(result, "{album}", track.Album.Title)
	result = strings.ReplaceAll(result, "{track}", fmt.Sprintf("%02d", track.TrackNumber))
	result = strings.ReplaceAll(result, "{discnumber}", fmt.Sprintf("%d", track.VolumeNumber))
	result = strings.ReplaceAll(result, "{year}", year)
	result = strings.ReplaceAll(result, "{date}", date)
	result = strings.ReplaceAll(result, "{isrc}", track.ISRC)

	return SanitizeFileName(result)
}

// applyFolderTemplate resolves a folder template string using track metadata.
// Supported placeholders: {artist}, {albumartist}, {album}, {year}, {genre}, {label}
func (t *TidalHifiService) applyFolderTemplate(template string, track *TidalHifiTrackResponse, artistName string) string {
	albumArtist := ""
	if track.Album.Artist.Name != "" {
		albumArtist = track.Album.Artist.Name
	} else if len(track.Album.Artists) > 0 {
		albumArtist = track.Album.Artists[0].Name
	}
	if albumArtist == "" {
		albumArtist = artistName
	}

	year := ""
	if track.Album.ReleaseDate != "" {
		if len(track.Album.ReleaseDate) >= 4 {
			year = track.Album.ReleaseDate[:4]
		}
	}

	album := track.Album.Title
	if album == "" {
		album = "Singles"
	}

	replacements := map[string]string{
		"{artist}":      artistName,
		"{albumartist}": albumArtist,
		"{album}":       album,
		"{year}":        year,
		"{label}":       "", // Label info not available in track response
	}

	result := template
	for placeholder, value := range replacements {
		safeValue := SanitizeFileName(value)
		if safeValue == "" {
			safeValue = "Unknown"
		}
		result = strings.ReplaceAll(result, placeholder, safeValue)
	}

	return result
}

// FormatCoverUUID converts a Tidal cover UUID to URL path format
func FormatCoverUUID(uuid string) string {
	// Tidal uses UUIDs like "abc-def-ghi" that need to be "abc/def/ghi"
	return strings.ReplaceAll(uuid, "-", "/")
}

// DownloadedFileInfo represents metadata for a downloaded file
type DownloadedFileInfo struct {
	Path        string `json:"path"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ModTime     string `json:"modTime"`
	Title       string `json:"title"`
	Artist      string `json:"artist"`
	Album       string `json:"album"`
	Source      string `json:"source"`      // tidal, qobuz, or unknown
	Quality     string `json:"quality"`     // e.g. "16-bit/44.1kHz"
	Format      string `json:"format"`      // file extension: flac, mp3, opus
	DiscNumber  int    `json:"discNumber"`  // disc number for grouping
	TrackNumber int    `json:"trackNumber"` // track number for ordering
}

// ListFLACFiles lists all FLAC files in the given directory recursively
// with enriched metadata from FLAC stream info and Vorbis comments.
func ListFLACFiles(folder string) ([]DownloadedFileInfo, error) {
	var files []DownloadedFileInfo
	err := filepath.WalkDir(folder, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".flac") {
			return nil
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}

		fileInfo := DownloadedFileInfo{
			Path:    path,
			Name:    name,
			Size:    info.Size(),
			ModTime: info.ModTime().Format("2006-01-02T15:04:05Z07:00"),
			Format:  "flac",
			Source:  "unknown",
		}

		// Try reading FLAC metadata for enriched info
		meta, metaErr := ReadFLACMetadata(path)
		if metaErr == nil {
			fileInfo.Title = meta.Title
			fileInfo.Artist = meta.Artist
			fileInfo.Album = meta.Album
			fileInfo.TrackNumber = parseTrackNum(meta.TrackNumber)
			fileInfo.DiscNumber = parseTrackNum(meta.DiscNumber)
			// Quality from stream info
			if meta.BitDepth > 0 && meta.SampleRate > 0 {
				sampleRateKHz := float64(meta.SampleRate) / 1000.0
				if sampleRateKHz == float64(int(sampleRateKHz)) {
					fileInfo.Quality = fmt.Sprintf("%d-bit/%gkHz", meta.BitDepth, sampleRateKHz)
				} else {
					fileInfo.Quality = fmt.Sprintf("%d-bit/%.1fkHz", meta.BitDepth, sampleRateKHz)
				}
			}
			// Source from ISRC prefix or comment heuristics
			fileInfo.Source = inferSource(meta)
		} else {
			// Fallback: parse from filename
			baseName := strings.TrimSuffix(name, ".flac")
			if parts := strings.SplitN(baseName, " - ", 2); len(parts) == 2 {
				fileInfo.Artist = parts[0]
				fileInfo.Title = parts[1]
			} else {
				fileInfo.Title = baseName
			}
		}

		files = append(files, fileInfo)
		return nil
	})
	return files, err
}

// inferSource tries to determine the download source from metadata.
func inferSource(meta *FLACMetadata) string {
	// Check comment field for source hints
	comment := strings.ToLower(meta.Comment)
	if strings.Contains(comment, "tidal") {
		return "tidal"
	}
	if strings.Contains(comment, "qobuz") {
		return "qobuz"
	}
	// Tidal files typically have ISRCs; Qobuz too, but Tidal is default source
	if meta.ISRC != "" {
		return "tidal"
	}
	return "unknown"
}

// parseTrackNum extracts the track number from a string like "3" or "3/12".
func parseTrackNum(s string) int {
	if s == "" {
		return 0
	}
	// Handle "3/12" format
	parts := strings.SplitN(s, "/", 2)
	var val int
	fmt.Sscanf(parts[0], "%d", &val)
	return val
}

// DeleteFile deletes a file from the filesystem
func DeleteFile(path string) error {
	// Security check: only allow deleting FLAC files
	if !strings.HasSuffix(strings.ToLower(path), ".flac") {
		return fmt.Errorf("can only delete FLAC files")
	}

	return os.Remove(path)
}
