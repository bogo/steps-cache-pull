package main

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/bitrise-io/go-steputils/stepconf"
	"github.com/bitrise-io/go-utils/log"
	"github.com/bitrise-steplib/steps-cache-push/model"
)

const (
	stepID = "cache-pull"
)

const (
	cachePullEndTimePath = "/tmp/cache_pull_end_time"
)

// Config stores the step inputs.
type Config struct {
	CacheAPIURL           string `env:"cache_api_url"`
	DebugMode             bool   `env:"is_debug_mode,opt[true,false]"`
	AllowFallback         bool   `env:"allow_fallback,opt[true,false]"`
	ExtractToRelativePath bool   `env:"extract_to_relative_path,opt[true,false]"`
	IgnoreStackDifference bool   `env:"ignore_stack_difference,opt[true,false]"`

	StackID   string `env:"BITRISEIO_STACK_ID"`
	BuildSlug string `env:"BITRISE_BUILD_SLUG"`
}

func main() {
	// Possible values of GOARCH: https://go.dev/doc/install/source#environment
	const currentArchitecture = runtime.GOARCH

	var conf Config
	if err := stepconf.Parse(&conf); err != nil {
		failf(err.Error())
	}

	stepconf.Print(conf)
	log.Printf("- architecture: %s", currentArchitecture)
	log.SetEnableDebugLog(conf.DebugMode)

	if conf.CacheAPIURL == "" {
		log.Warnf("No Cache API URL specified, there's no cache to use, exiting.")
		return
	}

	startTime := time.Now()

	var cacheReader io.Reader
	var cacheURI string

	if strings.HasPrefix(conf.CacheAPIURL, "file://") {
		cacheURI = conf.CacheAPIURL

		fmt.Println()
		log.Infof("Using local cache archive")

		pth := strings.TrimPrefix(conf.CacheAPIURL, "file://")

		var err error
		cacheReader, err = os.Open(pth)
		if err != nil {
			failf("Failed to open cache archive file: %s", err)
		}
	} else {
		fmt.Println()
		log.Infof("Downloading remote cache archive")

		var err error
		if isBitriseCacheAPIURL(conf.CacheAPIURL) {
			cacheURI, err = getCacheDownloadURL(conf.CacheAPIURL)
			if err != nil {
				failf("Failed to get cache download url: %s", err)
			}
		} else {
			cacheURI = conf.CacheAPIURL
		}

		cacheReader, err = performRequest(cacheURI)
		if err != nil {
			failf("Failed to perform cache download request: %s", err)
		}
	}

	cacheRecorderReader := NewRestoreReader(cacheReader)

	r, hdr, compressed, err := readFirstEntry(cacheRecorderReader)
	if err != nil {
		failf("Failed to get first archive entry: %s", err)
	}

	cacheRecorderReader.Restore()

	currentStackInfo := model.ArchiveInfo{
		StackID:      strings.TrimSpace(conf.StackID),
		Architecture: currentArchitecture,
	}
	if currentStackInfo.StackID != "" {
		fmt.Println()
		log.Infof("Checking archive and current stacks")
		log.Printf("current stack: %s", currentStackInfo)

		if filepath.Base(hdr.Name) == "archive_info.json" {
			b, err := ioutil.ReadAll(r)
			if err != nil {
				failf("Failed to read first archive entry: %s", err)
			}

			archiveStackInfo, err := parseArchiveInfo(b)
			if err != nil {
				failf("Failed to parse first archive entry: %s", err)
			}
			log.Printf("archive stack: %s", archiveStackInfo)

			if !conf.IgnoreStackDifference && !isSameStack(archiveStackInfo, currentStackInfo) {
				log.Warnf("Cache was created on stack: %s, current stack: %s", archiveStackInfo, currentStackInfo)
				log.Warnf("Skipping cache pull, as the stack has changed")

				if err := writeCachePullTimestamp(); err != nil {
					failf("Couldn't save cache pull timestamp: %s", err)
				}

				os.Exit(0)
			}

			if archiveStackInfo.Version < model.Version {
				if archiveStackInfo.Architecture == "" {
					log.Warnf("Cache has missing architecture info so default (amd64) architecture is assumed")
				}

				log.Warnf("Please update your cache-push step to the latest version")
			}
		} else {
			log.Warnf("cache archive does not contain stack information, skipping stack check")
		}
	}

	fmt.Println()
	log.Infof("Extracting cache archive")

	if err := extractCacheArchive(cacheRecorderReader, conf.ExtractToRelativePath, compressed); err != nil {
		if !conf.AllowFallback {
			failf("Failed to uncompress cache archive stream: %s", err)
		}

		log.Warnf("Failed to uncompress cache archive stream: %s", err)
		log.Warnf("Downloading the archive file and trying to uncompress using tar tool")
		data := map[string]interface{}{
			"archive_bytes_read": cacheRecorderReader.BytesRead,
			"build_slug":         conf.BuildSlug,
		}
		log.RInfof(stepID, "cache_archive_fallback", data, "Failed to uncompress cache archive stream: %s", err)

		pth, err := downloadCacheArchive(cacheURI, conf.BuildSlug)
		if err != nil {
			failf("Fallback failed, unable to download cache archive: %s", err)
		}

		if err := uncompressArchive(pth, conf.ExtractToRelativePath, compressed); err != nil {
			failf("Fallback failed, unable to uncompress cache archive file: %s", err)
		}
	} else {
		data := map[string]interface{}{
			"cache_archive_size": cacheRecorderReader.BytesRead,
			"build_slug":         conf.BuildSlug,
		}
		log.Debugf("Size of extracted cache archive: %d Bytes", cacheRecorderReader.BytesRead)
		log.RInfof(stepID, "cache_archive_size", data, "Size of extracted cache archive: %d Bytes", cacheRecorderReader.BytesRead)
	}

	if err := writeCachePullTimestamp(); err != nil {
		failf("Couldn't save cache pull timestamp: %s", err)
	}

	fmt.Println()
	log.Donef("Done")
	log.Printf("Took: " + time.Since(startTime).String())
}

// Helpers

// failf prints an error and terminates the step.
func failf(format string, args ...interface{}) {
	log.Errorf(format, args...)
	os.Exit(1)
}

// parseArchiveInfo reads the stack id and architecture from the given json bytes.
func parseArchiveInfo(b []byte) (info model.ArchiveInfo, err error) {
	err = json.Unmarshal(b, &info)
	return
}

func isSameStack(archiveStackInfo model.ArchiveInfo, currentStackInfo model.ArchiveInfo) bool {
	// TODO This check is a temporary solution to support GEN2 VMs having different ids for same stack types
	r := regexp.MustCompile("^(.+)-gen2.*$")
	currentStackInfo.StackID = r.ReplaceAllString(currentStackInfo.StackID, "$1")
	archiveStackInfo.StackID = r.ReplaceAllString(archiveStackInfo.StackID, "$1")

	if archiveStackInfo.StackID != currentStackInfo.StackID {
		return false
	}

	if archiveStackInfo.Version < model.Version && archiveStackInfo.Architecture == "" {
		return true
	}

	return archiveStackInfo.Architecture == currentStackInfo.Architecture
}

func isBitriseCacheAPIURL(url string) bool {
	return url == os.Getenv("BITRISE_CACHE_API_URL")
}

func writeCachePullTimestamp() (err error) {
	f, err := os.Create(cachePullEndTimePath)
	if err != nil {
		return err
	}

	defer func() {
		if fErr := f.Close(); fErr != nil {
			err = fErr
		}
	}()

	_, err = f.WriteString(strconv.FormatInt(time.Now().UnixNano()/int64(time.Millisecond), 10))

	return err
}
