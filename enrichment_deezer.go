package core

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const deezerAPIBase = "https://api.deezer.com/2.0"

// DeezerMetadata holds enrichment data fetched from the Deezer API.
type DeezerMetadata struct {
	Genre     string `json:"genre"`
	Label     string `json:"label"`
	Copyright string `json:"copyright"`
}

// DeezerClient is a lightweight client for the public Deezer API.
type DeezerClient struct {
	httpClient *http.Client
}

// NewDeezerClient creates a new DeezerClient.
func NewDeezerClient() *DeezerClient {
	return &DeezerClient{
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
}

// EnrichByISRC looks up a track by ISRC on Deezer and returns genre, label, and copyright.
func (dc *DeezerClient) EnrichByISRC(isrc string) (*DeezerMetadata, error) {
	if isrc == "" {
		return nil, fmt.Errorf("ISRC is required")
	}

	// Fetch track by ISRC
	trackURL := fmt.Sprintf("%s/track/isrc:%s", deezerAPIBase, isrc)
	resp, err := dc.httpClient.Get(trackURL)
	if err != nil {
		return nil, fmt.Errorf("deezer track request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("deezer API error (status %d): %s", resp.StatusCode, string(body))
	}

	var trackResp struct {
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error,omitempty"`
		Album struct {
			GenreID int    `json:"genre_id"`
			Label   string `json:"label"`
		} `json:"album"`
		Copyright string `json:"copyright"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&trackResp); err != nil {
		return nil, fmt.Errorf("failed to decode deezer track response: %w", err)
	}

	if trackResp.Error != nil {
		return nil, fmt.Errorf("deezer error: %s", trackResp.Error.Message)
	}

	meta := &DeezerMetadata{
		Label:     trackResp.Album.Label,
		Copyright: trackResp.Copyright,
	}

	// Resolve genre name from genre ID
	if trackResp.Album.GenreID > 0 {
		genreName, err := dc.getGenreName(trackResp.Album.GenreID)
		if err == nil {
			meta.Genre = genreName
		}
	}

	return meta, nil
}

// getGenreName fetches a genre name by ID from the Deezer API.
func (dc *DeezerClient) getGenreName(genreID int) (string, error) {
	genreURL := fmt.Sprintf("%s/genre/%d", deezerAPIBase, genreID)
	resp, err := dc.httpClient.Get(genreURL)
	if err != nil {
		return "", fmt.Errorf("deezer genre request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("deezer genre API error (status %d)", resp.StatusCode)
	}

	var genreResp struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&genreResp); err != nil {
		return "", fmt.Errorf("failed to decode deezer genre response: %w", err)
	}

	return genreResp.Name, nil
}
