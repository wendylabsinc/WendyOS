package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const wendyLiteReleasesURL = "https://api.github.com/repos/wendylabsinc/wendy-lite/releases"

// wendyLiteRelease describes a GitHub release for wendy-lite firmware.
type wendyLiteRelease struct {
	TagName    string               `json:"tag_name"`
	Prerelease bool                 `json:"prerelease"`
	Assets     []githubReleaseAsset `json:"assets"`
}

// firmwareAsset holds the resolved .bin asset info.
type firmwareAsset struct {
	Name        string
	DownloadURL string
	Size        int64
	Version     string
}

// fetchWendyLiteRelease finds the latest stable or prerelease from GitHub.
func fetchWendyLiteRelease(nightly bool) (*wendyLiteRelease, error) {
	client := &http.Client{Timeout: 30 * time.Second}

	if !nightly {
		// Use the "latest" endpoint for stable releases.
		resp, err := client.Get(wendyLiteReleasesURL + "/latest")
		if err != nil {
			return nil, fmt.Errorf("fetching latest wendy-lite release: %w", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
		}

		var release wendyLiteRelease
		if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
			return nil, fmt.Errorf("decoding release: %w", err)
		}
		return &release, nil
	}

	// For nightly, list releases and find the latest prerelease.
	resp, err := client.Get(wendyLiteReleasesURL)
	if err != nil {
		return nil, fmt.Errorf("fetching wendy-lite releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned status %d", resp.StatusCode)
	}

	var releases []wendyLiteRelease
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("decoding releases: %w", err)
	}

	for _, r := range releases {
		if r.Prerelease {
			return &r, nil
		}
	}

	return nil, fmt.Errorf("no nightly (prerelease) wendy-lite release found")
}

// findBinAsset returns the first .bin asset from a release.
func findBinAsset(release *wendyLiteRelease) (*firmwareAsset, error) {
	for _, a := range release.Assets {
		if strings.HasSuffix(a.Name, ".bin") {
			return &firmwareAsset{
				Name:        a.Name,
				DownloadURL: a.BrowserDownloadURL,
				Version:     release.TagName,
			}, nil
		}
	}
	return nil, fmt.Errorf("no .bin asset found in release %s", release.TagName)
}

// downloadFirmware downloads a firmware .bin to a temp file, reporting progress.
func downloadFirmware(asset *firmwareAsset, progressFn func(downloaded, total int64)) (string, error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(asset.DownloadURL)
	if err != nil {
		return "", fmt.Errorf("downloading firmware: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	tmpFile, err := os.CreateTemp("", "wendy-lite-*.bin")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}

	total := resp.ContentLength
	var downloaded int64

	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := tmpFile.Write(buf[:n]); err != nil {
				tmpFile.Close()
				os.Remove(tmpFile.Name())
				return "", fmt.Errorf("writing firmware: %w", err)
			}
			downloaded += int64(n)
			if progressFn != nil {
				progressFn(downloaded, total)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
			return "", fmt.Errorf("reading firmware: %w", readErr)
		}
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("closing temp file: %w", err)
	}

	return tmpFile.Name(), nil
}
