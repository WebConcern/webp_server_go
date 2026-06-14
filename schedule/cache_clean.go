package schedule

import (
	"errors"
	"fmt"
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
			// Fail loudly if we can't even read the cache root, so cleanup doesn't
			// silently report success when it never scanned the tree.
			if p == path {
				return err
			}
			// Cache files can be created/removed while cleanup runs. Don't abort
			// the whole walk over a single transient per-file error (e.g. a file
			// that disappeared); skip the entry and keep going.
			if !os.IsNotExist(err) {
				log.Warnf("Error accessing %s during cache scan: %s", p, err)
			}
			return nil
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

	var deleteErr error
	for _, entry := range entries {
		if totalSize <= maxCacheSizeBytes {
			break
		}
		if err := os.Remove(entry.path); err != nil {
			if os.IsNotExist(err) {
				// Already gone (e.g. removed concurrently); the space is freed.
				totalSize -= entry.size
				continue
			}
			log.Errorf("Error deleting file %s: %s\n", entry.path, err.Error())
			deleteErr = errors.Join(deleteErr, err)
			continue
		}
		totalSize -= entry.size
		log.Infof("Deleted oldest file: %s\n", entry.path)
	}

	if deleteErr != nil {
		return deleteErr
	}
	if totalSize > maxCacheSizeBytes {
		return fmt.Errorf("cache %s still above limit after cleanup: %d > %d bytes", path, totalSize, maxCacheSizeBytes)
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
			// clearCacheFiles returns nil for a missing path, so any error here is
			// a real failure worth logging.
			if err := clearCacheFiles(config.Config.RemoteRawPath, maxCacheSizeBytes); err != nil {
				log.Warnf("Failed to clear remote raw cache: %s", err)
			}
			if err := clearCacheFiles(config.Config.ExhaustPath, maxCacheSizeBytes); err != nil {
				log.Warnf("Failed to clear exhaust cache: %s", err)
			}
			if err := clearCacheFiles(config.Config.MetadataPath, maxCacheSizeBytes); err != nil {
				log.Warnf("Failed to clear metadata cache: %s", err)
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
					if err := os.RemoveAll(target); err != nil {
						log.Warnf("Failed to delete dead vips cache %s: %s", target, err)
					}
				}
			}
		}
	}
}
