package helper

import (
	"encoding/json"
	"net/url"
	"os"
	"path"
	"testing"
	"webp_server_go/config"
)

func TestGetId(t *testing.T) {
	p := "https://example.com/image.jpg?width=200&height=300"

	t.Run("proxy mode", func(t *testing.T) {
		// Test case 1: Proxy mode
		config.ProxyMode = true
		id, jointPath, santizedPath := getId(p, "")

		// Verify the return values
		expectedId := HashString(p)
		expectedPath := "remote-raw/8d8576343c4cb816.jpg?width=200&height=300"
		expectedSantizedPath := ""
		if id != expectedId || jointPath != expectedPath || santizedPath != expectedSantizedPath {
			t.Errorf("Test case 1 failed: Expected (%s, %s, %s), but got (%s, %s, %s)",
				expectedId, expectedPath, expectedSantizedPath, id, jointPath, santizedPath)
		}
	})
	t.Run("non-proxy mode", func(t *testing.T) {
		// Test case 2: Non-proxy mode
		config.ProxyMode = false
		p = "/image.jpg?width=400&height=500"
		id, jointPath, santizedPath := getId(p, "")

		// Verify the return values
		parsed, _ := url.Parse(p)
		expectedId := HashString(parsed.Path + "?width=400&height=500&max_width=&max_height=")
		expectedPath := path.Join(config.Config.ImgPath, parsed.Path)
		expectedSantizedPath := parsed.Path + "?width=400&height=500&max_width=&max_height="
		if id != expectedId || jointPath != expectedPath || santizedPath != expectedSantizedPath {
			t.Errorf("Test case 2 failed: Expected (%s, %s, %s), but got (%s, %s, %s)",
				expectedId, expectedPath, expectedSantizedPath, id, jointPath, santizedPath)
		}
	})
}

func TestReadMetadataFailureModes(t *testing.T) {
	// Use a non-image extension so WriteMetadata skips libvips (getImageMeta) and
	// the test exercises only the read/write/rebuild logic.
	config.ProxyMode = false
	config.Config.MetadataPath = t.TempDir()
	config.Config.ImgPath = t.TempDir()

	const (
		p      = "/does-not-exist.txt"
		subdir = "local"
	)
	expectedId, _, _ := getId(p, subdir)
	metaPath := path.Join(config.Config.MetadataPath, subdir, expectedId+".json")

	t.Run("creates metadata when file is missing", func(t *testing.T) {
		meta := ReadMetadata(p, "", subdir)
		if meta.Id != expectedId {
			t.Fatalf("expected Id %s, got %s", expectedId, meta.Id)
		}
		if _, err := os.Stat(metaPath); err != nil {
			t.Fatalf("expected metadata file to be created, stat err: %v", err)
		}
	})

	t.Run("rebuilds corrupt metadata without recursing", func(t *testing.T) {
		if err := os.WriteFile(metaPath, []byte("{ not valid json"), 0644); err != nil {
			t.Fatal(err)
		}
		meta := ReadMetadata(p, "", subdir)
		if meta.Id != expectedId {
			t.Fatalf("expected Id %s after rebuild, got %s", expectedId, meta.Id)
		}
		buf, err := os.ReadFile(metaPath)
		if err != nil {
			t.Fatal(err)
		}
		var parsed config.MetaFile
		if err := json.Unmarshal(buf, &parsed); err != nil {
			t.Fatalf("metadata file should be valid JSON after rebuild: %v", err)
		}
	})
}
