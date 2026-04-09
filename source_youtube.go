package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

const (
	cobaltAPIURL  = "https://api.cobalt.tools/"
	odesliAPIBase = "https://api.song.link/v1-alpha.1/links"
)

// CobaltSource provides lossy audio downloads via the Cobalt API (YouTube fallback).
type CobaltSource struct {
	httpClient *http.Client
}

// NewCobaltSource creates a new CobaltSource.
func NewCobaltSource() *CobaltSource {
	return &CobaltSource{
		httpClient: &http.Client{Timeout: 120 * time.Second},
	}
}

// Download fetches audio from the given YouTube URL via Cobalt and writes it to outputPath.
// format must be "opus" or "mp3".
func (cs *CobaltSource) Download(youtubeURL string, outputPath string, format string) error {
	if format == "" {
		format = "opus"
	}
	if format != "opus" && format != "mp3" {
		return fmt.Errorf("unsupported format %q: must be \"opus\" or \"mp3\"", format)
	}

	body := map[string]interface{}{
		"url":         youtubeURL,
		"audioFormat": format,
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("failed to marshal cobalt request: %w", err)
	}

	req, err := http.NewRequest("POST", cobaltAPIURL, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("failed to create cobalt request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := cs.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("cobalt request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("cobalt API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var cobaltResp struct {
		Status string `json:"status"`
		URL    string `json:"url"`
		Error  string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cobaltResp); err != nil {
		return fmt.Errorf("failed to decode cobalt response: %w", err)
	}

	if cobaltResp.Status != "stream" && cobaltResp.Status != "redirect" {
		errMsg := cobaltResp.Error
		if errMsg == "" {
			errMsg = "unknown cobalt error, status: " + cobaltResp.Status
		}
		return fmt.Errorf("cobalt: %s", errMsg)
	}

	if cobaltResp.URL == "" {
		return fmt.Errorf("cobalt returned empty download URL")
	}

	// Download the actual audio file
	audioResp, err := cs.httpClient.Get(cobaltResp.URL)
	if err != nil {
		return fmt.Errorf("failed to download audio from cobalt: %w", err)
	}
	defer audioResp.Body.Close()

	if audioResp.StatusCode != http.StatusOK {
		return fmt.Errorf("cobalt audio download failed with status %d", audioResp.StatusCode)
	}

	out, err := os.Create(outputPath)
	if err != nil {
		return fmt.Errorf("failed to create output file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, audioResp.Body); err != nil {
		return fmt.Errorf("failed to write audio file: %w", err)
	}

	return nil
}

// ResolveFromISRC uses the SongLink/Odesli API to find a YouTube URL from an ISRC.
func (cs *CobaltSource) ResolveFromISRC(isrc string) (string, error) {
	if isrc == "" {
		return "", fmt.Errorf("ISRC is required")
	}

	// Odesli doesn't support ISRC directly; build a search-style query URL
	// Use the Tidal ISRC lookup URL as the input for Odesli
	inputURL := fmt.Sprintf("https://tidal.com/browse/track/isrc:%s", isrc)
	return cs.resolveYouTubeURL(inputURL)
}

// resolveYouTubeURL calls the Odesli API with any music URL and extracts the YouTube link.
func (cs *CobaltSource) resolveYouTubeURL(inputURL string) (string, error) {
	apiURL := fmt.Sprintf("%s?url=%s", odesliAPIBase, url.QueryEscape(inputURL))

	resp, err := cs.httpClient.Get(apiURL)
	if err != nil {
		return "", fmt.Errorf("odesli request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("odesli API error (status %d): %s", resp.StatusCode, string(body))
	}

	var odesliResp struct {
		LinksByPlatform map[string]struct {
			URL string `json:"url"`
		} `json:"linksByPlatform"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&odesliResp); err != nil {
		return "", fmt.Errorf("failed to decode odesli response: %w", err)
	}

	// Prefer YouTube Music, fall back to YouTube
	if yt, ok := odesliResp.LinksByPlatform["youtubeMusic"]; ok && yt.URL != "" {
		return yt.URL, nil
	}
	if yt, ok := odesliResp.LinksByPlatform["youtube"]; ok && yt.URL != "" {
		return yt.URL, nil
	}

	return "", fmt.Errorf("no YouTube link found for URL: %s", inputURL)
}
