package schedule

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"webp_server_go/config"

	log "github.com/sirupsen/logrus"
)

type cacheEntry struct {
	path    string
	size    int64
	modTime time.Time
}

// Clear cache: delete oldest files first until the total size of the directory
// is at or below maxCacheSizeBytes. The directory tree is walked once to build a
// snapshot, then files are deleted in oldest-first order from that snapshot,
// avoiding the previous O(files^2) repeated full-tree walks.
func clearCacheFiles(path string, maxCacheSizeBytes int64) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}

	var totalSize int64
	var entries []cacheEntry
	err := filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			totalSize += info.Size()
			entries = append(entries, cacheEntry{path: p, size: info.Size(), modTime: info.ModTime()})
		}
		return nil
	})
	if err != nil {
		log.Errorf("Error getting directory size: %s\n", err.Error())
		return err
	}

	if totalSize <= maxCacheSizeBytes {
		return nil
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].modTime.Before(entries[j].modTime)
	})

	for _, entry := range entries {
		if totalSize <= maxCacheSizeBytes {
			break
		}
		if err := os.Remove(entry.path); err != nil {
			log.Errorf("Error deleting file %s: %s\n", entry.path, err.Error())
			continue
		}
		totalSize -= entry.size
		log.Infof("Deleted oldest file: %s\n", entry.path)
	}
	return nil
}

func CleanCache() {
	log.Info("MaxCacheSize is not 0, starting cache cleaning service")
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// MB to bytes
			maxCacheSizeBytes := int64(config.Config.MaxCacheSize) * 1024 * 1024
			err := clearCacheFiles(config.Config.RemoteRawPath, maxCacheSizeBytes)
			if err != nil {
				log.Warn("Failed to clear remote raw cache")
			}
			err = clearCacheFiles(config.Config.ExhaustPath, maxCacheSizeBytes)
			if err != nil && err != os.ErrNotExist {
				log.Warn("Failed to clear remote raw cache")
			}
			err = clearCacheFiles(config.Config.MetadataPath, maxCacheSizeBytes)
			if err != nil && err != os.ErrNotExist {
				log.Warn("Failed to clear remote raw cache")
			}
		}
	}
}

func DeleteDeadCache() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	tempDir := os.TempDir()
	for {
		select {
		case <-ticker.C:
			// Recompute the threshold each tick instead of once at startup.
			threshold := time.Now().Add(-10 * time.Minute)
			entries, err := os.ReadDir(tempDir)
			if err != nil {
				log.Debugf("Cannot read temp dir %s: %s", tempDir, err)
				continue
			}
			for _, entry := range entries {
				// libvips spills large images to files/dirs named "vips-*" in TempDir.
				if !strings.HasPrefix(entry.Name(), "vips-") {
					continue
				}
				info, err := entry.Info()
				if err != nil {
					continue
				}
				if info.ModTime().Before(threshold) {
					target := filepath.Join(tempDir, entry.Name())
					log.Warnf("Deleting dead vips cache: %s", target)
					_ = os.RemoveAll(target)
				}
			}
		}
	}
}
