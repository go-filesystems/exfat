package filesystem_exfat_test

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ensureImageInCache makes a best-effort attempt to populate
// $HOME/.mock/cache/<encodedFolder>/<filename>. It first tries to run the
// repository's `mock pull` (if present), then falls back to decoding the
// encoded folder name to a URL and performing an HTTP GET.
func ensureImageInCache(t *testing.T, encodedFolder, filename string) (string, error) {
	t.Helper()
	home := os.Getenv("HOME")
	if home == "" {
		return "", fmt.Errorf("HOME not set")
	}
	cacheDir := filepath.Join(home, ".mock", "cache")
	destDir := filepath.Join(cacheDir, encodedFolder)
	destPath := filepath.Join(destDir, filename)
	if _, err := os.Stat(destPath); err == nil {
		return destPath, nil
	}

	// Try to find repo root so we can run local ./bin/mock if available.
	repoRoot, _ := findRepoRoot()
	if repoRoot != "" {
		// prefer system 'mock' first, then ./bin/mock
		mockPath, _ := exec.LookPath("mock")
		if mockPath == "" {
			cand := filepath.Join(repoRoot, "bin", "mock")
			if _, err := os.Stat(cand); err == nil {
				mockPath = cand
			}
		}
		if mockPath != "" {
			cfg := filepath.Join(repoRoot, "mock.hcl")
			// try with config if present
			if _, err := os.Stat(cfg); err == nil {
				cmd := exec.Command(mockPath, "pull", "--config", "mock.hcl")
				cmd.Dir = repoRoot
				out, _ := cmd.CombinedOutput()
				t.Logf("mock pull output:\n%s", out)
				if _, err := os.Stat(destPath); err == nil {
					return destPath, nil
				}
			}
			// best-effort: try 'mock pull' without config
			cmd := exec.Command(mockPath, "pull")
			cmd.Dir = repoRoot
			out, _ := cmd.CombinedOutput()
			t.Logf("mock pull (no config) output:\n%s", out)
			if _, err := os.Stat(destPath); err == nil {
				return destPath, nil
			}
		}
	}

	// Fallback: try to decode encodedFolder into a URL and download.
	// To avoid downloading large images unexpectedly, only perform HTTP
	// downloads when explicitly enabled via environment variable
	// `MOCK_ALLOW_IMAGE_DOWNLOAD=1`.
	if os.Getenv("MOCK_ALLOW_IMAGE_DOWNLOAD") != "1" {
		return "", fmt.Errorf("image missing and HTTP download disabled; set MOCK_ALLOW_IMAGE_DOWNLOAD=1 to enable")
	}
	url := decodeEncodedFolderToURL(encodedFolder)
	if url == "" {
		return "", fmt.Errorf("cannot decode URL from %q", encodedFolder)
	}
	// If decoded URL doesn't already end with filename, append it.
	if filepath.Base(url) != filename {
		if strings.HasSuffix(url, "/") {
			url = url + filename
		} else {
			url = url + "/" + filename
		}
	}
	t.Logf("downloading %s → %s", url, destPath)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir cache dir: %v", err)
	}

	// Perform HTTP GET (best-effort). Large downloads may take time.
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("http get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("http status %d", resp.StatusCode)
	}
	out, err := os.Create(destPath)
	if err != nil {
		return "", fmt.Errorf("create file: %v", err)
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return "", fmt.Errorf("write file: %v", err)
	}
	if _, err := os.Stat(destPath); err != nil {
		return "", fmt.Errorf("downloaded but missing: %v", err)
	}
	return destPath, nil
}

func decodeEncodedFolderToURL(encoded string) string {
	if encoded == "" {
		return ""
	}
	s := encoded
	if strings.HasPrefix(s, "https____") {
		s = strings.Replace(s, "https____", "https://", 1)
	} else if strings.HasPrefix(s, "http____") {
		s = strings.Replace(s, "http____", "http://", 1)
	}
	s = strings.ReplaceAll(s, "_", "/")
	if i := strings.Index(s, "://"); i >= 0 {
		scheme := s[:i+3]
		rest := s[i+3:]
		rest = strings.TrimLeft(rest, "/")
		s = scheme + rest
	}
	return s
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "mock.hcl")); err == nil {
			return dir, nil
		}
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("repo root not found")
}
