package handler

import (
	"bytes"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"webp_server_go/config"
	"webp_server_go/helper"

	"github.com/gofiber/fiber/v2"
	"github.com/h2non/filetype"
	"github.com/patrickmn/go-cache"
	log "github.com/sirupsen/logrus"
)

// remoteClient is used for all upstream fetches. Without timeouts a stalled
// origin would hang the request indefinitely while holding ConvertLock, letting
// goroutines and memory pile up until the pod becomes unresponsive.
var (
	remoteClientOnce sync.Once
	remoteClient     *http.Client
)

// getRemoteClient lazily builds the shared upstream HTTP client. It is built on
// first use rather than at package init because config (the overall timeout) is
// only loaded after init runs. The client is reused so connections are pooled.
//
// config.RemoteRequestTimeout is an overall per-request cap; setting it to 0
// disables that cap for users fetching very large images over slow origins,
// while newRemoteTransport's ResponseHeaderTimeout still bounds a stalled origin.
func getRemoteClient() *http.Client {
	remoteClientOnce.Do(func() {
		remoteClient = &http.Client{
			Timeout:   time.Duration(config.Config.RemoteRequestTimeout) * time.Second,
			Transport: newRemoteTransport(),
		}
	})
	return remoteClient
}

// newRemoteTransport clones http.DefaultTransport so we keep its
// proxy-from-environment support (HTTP(S)_PROXY/NO_PROXY) and default
// dial/TLS handshake timeouts, and only adds a response-header timeout to
// guard against a stalled origin. If DefaultTransport has been replaced
// (e.g. in tests) and is not an *http.Transport, fall back to a fresh one so
// package init can't panic.
func newRemoteTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 15 * time.Second,
		}
	}
	t := base.Clone()
	t.ResponseHeaderTimeout = 15 * time.Second
	return t
}

// Given /path/to/node.png
// Delete /path/to/node.png*
func cleanProxyCache(cacheImagePath string) {
	// Delete /node.png*
	files, err := filepath.Glob(cacheImagePath + "*")
	if err != nil {
		log.Infoln(err)
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil {
			log.Info(err)
		}
	}
}

// Download file and return response header
func downloadFile(filepath string, url string) http.Header {
	resp, err := getRemoteClient().Get(url)
	if err != nil {
		log.Errorf("Connection to remote error when downloadFile %s: %s", url, err)
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		log.Errorf("remote returned %s when fetching remote image", resp.Status)
		return resp.Header
	}

	// Copy bytes here
	bodyBytes := new(bytes.Buffer)
	_, err = bodyBytes.ReadFrom(resp.Body)
	if err != nil {
		return nil
	}

	// Check if remote content-type is image using check by filetype instead of content-type returned by origin
	kind, _ := filetype.Match(bodyBytes.Bytes())
	mime := kind.MIME.Value
	if !strings.Contains(mime, "image") && !config.AllowAllExtensions {
		log.Errorf("remote file %s is not image and AllowedTypes is not '*', remote content has MIME type of %s", url, mime)
		return nil
	}

	_ = os.MkdirAll(path.Dir(filepath), 0755)

	// Create Cache here as a lock, so we can prevent incomplete file from being read
	// Key: filepath, Value: true
	config.WriteLock.Set(filepath, true, -1)
	// Always release the lock, even if the write fails (e.g. disk full). The lock
	// is set with no expiration, so an early return without Delete would leave it
	// stuck forever, making ImageExists return false for this file on every future
	// request (with retry sleeps) -> permanent 404s plus added latency.
	defer config.WriteLock.Delete(filepath)

	err = os.WriteFile(filepath, bodyBytes.Bytes(), 0600)
	if err != nil {
		log.Errorf("failed to write downloaded file %s: %s", filepath, err)
		// Remove any partial file so a later request re-fetches cleanly.
		_ = os.Remove(filepath)
		return nil
	}

	return resp.Header
}

func fetchRemoteImg(url string, subdir string) (metaContent config.MetaFile) {
	// url is https://test.webp.sh/mypic/123.jpg?someother=200&somebugs=200
	// How do we know if the remote img is changed? we're using hash(etag+length)
	var etag string

	cacheKey := subdir + ":" + helper.HashString(url)

	if val, found := config.RemoteCache.Get(cacheKey); found {
		if etagVal, ok := val.(string); ok {
			log.Infof("Using cache for remote addr: %s", url)
			etag = etagVal
		} else {
			config.RemoteCache.Delete(cacheKey)
		}
	}

	if etag == "" {
		log.Infof("Remote Addr is %s, pinging for info...", url)
		etag = pingURL(url)
		if etag != "" {
			config.RemoteCache.Set(cacheKey, etag, cache.DefaultExpiration)
		}
	}

	metadata := helper.ReadMetadata(url, etag, subdir)
	remoteFileExtension := path.Ext(url)
	localRawImagePath := path.Join(config.Config.RemoteRawPath, subdir, metadata.Id) + remoteFileExtension
	localExhaustImagePath := path.Join(config.Config.ExhaustPath, subdir, metadata.Id)

	if !helper.ImageExists(localRawImagePath) || metadata.Checksum != helper.HashString(etag) {
		cleanProxyCache(localExhaustImagePath)
		if metadata.Checksum != helper.HashString(etag) {
			// remote file has changed
			log.Info("Remote file changed, updating metadata and fetching image source...")
			helper.DeleteMetadata(url, subdir)
			helper.WriteMetadata(url, etag, subdir)
		} else {
			// local file not exists
			log.Info("Remote file not found in remote-raw, re-fetching...")
		}
		_ = downloadFile(localRawImagePath, url)
		// Update metadata with newly downloaded file
		helper.WriteMetadata(url, etag, subdir)
	}
	return metadata
}

func pingURL(url string) string {
	// this function will try to return identifiable info, currently include etag, content-length as string
	// anything goes wrong, will return ""
	var etag, length string
	resp, err := getRemoteClient().Head(url)
	if err != nil {
		log.Errorln("Connection to remote error when pingUrl:"+url, err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode == fiber.StatusOK {
		etag = resp.Header.Get("etag")
		length = resp.Header.Get("content-length")
	}
	if etag == "" {
		log.Info("Remote didn't return etag in header when getRemoteImageInfo, please check.")
	}
	return etag + length
}
