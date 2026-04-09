package core

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// Multi-service URL patterns
var (
	deezerTrackRegex    = regexp.MustCompile(`deezer\.com/(?:\w+/)?track/(\d+)`)
	deezerAlbumRegex    = regexp.MustCompile(`deezer\.com/(?:\w+/)?album/(\d+)`)
	deezerPageLinkRegex = regexp.MustCompile(`deezer\.page\.link/`)
	ytMusicRegex        = regexp.MustCompile(`music\.youtube\.com/watch\?v=([a-zA-Z0-9_-]+)`)
)

// ResolvedURL holds the result of resolving a non-Tidal URL to a Tidal ID.
type ResolvedURL struct {
	TidalID         string `json:"tidalId"`
	Type            string `json:"type"`            // "track", "album"
	OriginalService string `json:"originalService"` // "deezer", "youtube_music"
}

// ParseMultiServiceURL attempts to parse Deezer and YouTube Music URLs.
// Returns the service name, extracted ID, and content type, or an error
// if the URL is not recognized as a supported non-Tidal service.
func ParseMultiServiceURL(rawURL string) (service string, id string, contentType string, err error) {
	// Deezer track
	if m := deezerTrackRegex.FindStringSubmatch(rawURL); len(m) > 1 {
		return "deezer", m[1], "track", nil
	}
	// Deezer album
	if m := deezerAlbumRegex.FindStringSubmatch(rawURL); len(m) > 1 {
		return "deezer", m[1], "album", nil
	}
	// Deezer short link (page.link) — we can't extract an ID, needs Odesli resolution
	if deezerPageLinkRegex.MatchString(rawURL) {
		return "deezer", "", "unknown", nil
	}
	// YouTube Music
	if m := ytMusicRegex.FindStringSubmatch(rawURL); len(m) > 1 {
		return "youtube_music", m[1], "track", nil
	}

	return "", "", "", fmt.Errorf("URL not recognized as a supported service: %s", rawURL)
}

// ResolveToTidal uses the Odesli/SongLink API to resolve any music URL to a Tidal track/album.
func ResolveToTidal(rawURL string) (*ResolvedURL, error) {
	service, _, _, _ := ParseMultiServiceURL(rawURL)
	if service == "" {
		// Also try the URL directly with Odesli — it may still be resolvable
		service = "unknown"
	}

	apiURL := fmt.Sprintf("%s?url=%s", odesliAPIBase, url.QueryEscape(rawURL))
	client := &http.Client{Timeout: 15 * time.Second}

	resp, err := client.Get(apiURL)
	if err != nil {
		return nil, fmt.Errorf("odesli request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("odesli API error (status %d): %s", resp.StatusCode, string(body))
	}

	var odesliResp struct {
		EntityUniqueID  string `json:"entityUniqueId"`
		LinksByPlatform map[string]struct {
			URL         string `json:"url"`
			EntityUniqueID string `json:"entityUniqueId"`
		} `json:"linksByPlatform"`
		EntitiesByUniqueID map[string]struct {
			ID       string `json:"id"`
			Type     string `json:"type"` // "song", "album"
			Platform string `json:"platform"`
		} `json:"entitiesByUniqueId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&odesliResp); err != nil {
		return nil, fmt.Errorf("failed to decode odesli response: %w", err)
	}

	// Find the Tidal link
	tidalLink, ok := odesliResp.LinksByPlatform["tidal"]
	if !ok || tidalLink.EntityUniqueID == "" {
		return nil, fmt.Errorf("no Tidal equivalent found for URL: %s", rawURL)
	}

	// Get the Tidal entity details
	entity, ok := odesliResp.EntitiesByUniqueID[tidalLink.EntityUniqueID]
	if !ok {
		// Try to parse the Tidal URL directly
		tidalID, contentType, err := ParseTidalURL(tidalLink.URL)
		if err != nil {
			return nil, fmt.Errorf("could not extract Tidal ID from resolved URL")
		}
		return &ResolvedURL{
			TidalID:         tidalID,
			Type:            contentType,
			OriginalService: service,
		}, nil
	}

	// Map Odesli type to our type
	contentType := "track"
	if entity.Type == "album" {
		contentType = "album"
	}

	return &ResolvedURL{
		TidalID:         entity.ID,
		Type:            contentType,
		OriginalService: service,
	}, nil
}
