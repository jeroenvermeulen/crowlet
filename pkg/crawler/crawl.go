package crawler

import (
	"errors"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/yterajima/go-sitemap"
)

// CrawlResult is the result from a single crawling
type CrawlResult struct {
	URL         string        `json:"url"`
	StatusCode  int           `json:"status-code"`
	Time        time.Duration `json:"server-time"`
	LinkingURLs []string      `json:"linking-urls"`
}

// CrawlStats holds crawling related information: status codes, time
// and totals
type CrawlStats struct {
	Total          int
	StatusCodes    map[int]int
	Average200Time time.Duration
	Max200Time     time.Duration
	Non200Urls     []CrawlResult
}

// CrawlConfig holds crawling configuration.
type CrawlConfig struct {
	Throttle   int
	Host       string
	HTTP       HTTPConfig
	Links      CrawlLinksConfig
	HTTPGetter ConcurrentHTTPGetter
}

// CrawlLinksConfig holds the crawling policy for links
type CrawlLinksConfig struct {
	CrawlExternalLinks bool
	CrawlHyperlinks    bool
	CrawlImages        bool
}

// MergeCrawlStats merges two sets of crawling statistics together.
func MergeCrawlStats(statsA, statsB CrawlStats) (stats CrawlStats) {
	stats.StatusCodes = make(map[int]int)
	stats.Total = statsA.Total + statsB.Total

	if statsA.Max200Time > statsB.Max200Time {
		stats.Max200Time = statsA.Max200Time
	} else {
		stats.Max200Time = statsB.Max200Time
	}

	if statsA.StatusCodes != nil {
		for key, value := range statsA.StatusCodes {
			stats.StatusCodes[key] = stats.StatusCodes[key] + value
		}
	}
	if statsB.StatusCodes != nil {
		for key, value := range statsB.StatusCodes {
			stats.StatusCodes[key] = stats.StatusCodes[key] + value
		}
	}

	if statsA.Average200Time != 0 || statsB.Average200Time != 0 {
		total200ns := (statsA.Average200Time.Nanoseconds()*int64(statsA.StatusCodes[200]) +
			statsB.Average200Time.Nanoseconds()*int64(statsB.StatusCodes[200]))
		stats.Average200Time = time.Duration(total200ns/int64(stats.StatusCodes[200])) * time.Nanosecond
	}

	stats.Non200Urls = append(stats.Non200Urls, statsA.Non200Urls...)
	stats.Non200Urls = append(stats.Non200Urls, statsB.Non200Urls...)

	return
}

func addInterruptHandlers() chan struct{} {
	stop := make(chan struct{})
	osSignal := make(chan os.Signal)
	signal.Notify(osSignal, os.Interrupt, syscall.SIGTERM)
	signal.Notify(osSignal, os.Interrupt, syscall.SIGINT)

	go func() {
		<-osSignal
		log.Warn("Interrupt signal received")
		stop <- struct{}{}
	}()

	return stop
}

// GetSitemapUrls returns all URLs found from the sitemap passed as parameter.
// This function will only retrieve URLs in the sitemap pointed, and in
// sitemaps directly listed (i.e. only 1 level deep or less)
func GetSitemapUrls(sitemapURL string) (urls []*url.URL, err error) {
	sitemap, err := sitemap.Get(sitemapURL, nil)

	if err != nil {
		log.Error(err)
		return
	}

	for _, urlEntry := range sitemap.URL {
		newURL, err := url.Parse(urlEntry.Loc)
		if err != nil {
			log.Error(err)
			continue
		}
		urls = append(urls, newURL)
	}

	return
}

// GetSitemapUrlsAsStrings returns all URLs found as string, from in the
// sitemap passed as parameter.
// This function will only retrieve URLs in the sitemap pointed, and in
// sitemaps directly listed (i.e. only 1 level deep or less)
func GetSitemapUrlsAsStrings(sitemapURL string) (urls []string, err error) {
	typedUrls, err := GetSitemapUrls(sitemapURL)
	for _, url := range typedUrls {
		urls = append(urls, url.String())
	}

	return
}

// AsyncCrawl crawls asynchronously URLs from a sitemap and prints related
// information. Throttle is the maximum number of parallel HTTP requests.
// Host overrides the hostname used in the sitemap if provided,
// and user/pass are optional basic auth credentials
func AsyncCrawl(urls []string, config CrawlConfig) (stats CrawlStats,
	stopped bool, err error) {
	if config.Throttle <= 0 {
		log.Warn("Invalid throttle value, defaulting to 1.")
		config.Throttle = 1
	}

	config.HTTP.ParseLinks = config.Links.CrawlExternalLinks || config.Links.CrawlHyperlinks ||
		config.Links.CrawlImages
	results, stats, server200TimeSum := crawlUrls(urls, config)

	if config.HTTP.ParseLinks {
		_, linksStats, linksServer200TimeSum := crawlLinks(results, urls, config)
		stats = MergeCrawlStats(stats, linksStats)
		server200TimeSum += linksServer200TimeSum
	}

	total200 := stats.StatusCodes[200]
	if total200 > 0 {
		stats.Average200Time = server200TimeSum / time.Duration(total200)
	}

	if stats.Total == 0 {
		err = errors.New("No URL crawled")
	} else if stats.Total != stats.StatusCodes[200] {
		err = errors.New("Some URLs had a different status code than 200")
	}

	return
}

func crawlLinks(sourceResults []HTTPResponse, sourceURLs []string, sourceConfig CrawlConfig) ([]HTTPResponse, CrawlStats, time.Duration) {

	linkedUrlsSet := make(map[string][]string)
	for _, result := range sourceResults {
		for _, link := range result.Links {
			if link.IsExternal && !sourceConfig.Links.CrawlExternalLinks {
				continue
			}

			if link.Type == Hyperlink && !sourceConfig.Links.CrawlHyperlinks {
				continue
			}

			if link.Type == Image && !sourceConfig.Links.CrawlImages {
				continue
			}

			linkedUrlsSet[link.TargetURL.String()] = append(linkedUrlsSet[link.TargetURL.String()], result.URL)
		}
	}

	for _, alreadyCrawledURL := range sourceURLs {
		delete(linkedUrlsSet, alreadyCrawledURL)
	}

	linkedUrls := make([]string, 0, len(linkedUrlsSet))
	for url := range linkedUrlsSet {
		linkedUrls = append(linkedUrls, url)
	}

	linksConfig := sourceConfig
	linksConfig.HTTP.ParseLinks = false
	linksConfig.Links = CrawlLinksConfig{
		CrawlExternalLinks: false,
		CrawlImages:        false,
		CrawlHyperlinks:    false}

	linksResults, linksStats, linksServer200TimeSum := crawlUrls(linkedUrls, linksConfig)

	for i, linkResult := range linksStats.Non200Urls {
		linkResult.LinkingURLs = linkedUrlsSet[linkResult.URL]
		linksStats.Non200Urls[i] = linkResult
	}

	return linksResults, linksStats, linksServer200TimeSum
}

func crawlUrls(urls []string, config CrawlConfig) (results []HTTPResponse,
	stats CrawlStats, server200TimeSum time.Duration) {

	quit := addInterruptHandlers()
	stats.StatusCodes = make(map[int]int)
	resultsChan := config.HTTPGetter.ConcurrentHTTPGet(urls, config.HTTP, config.Throttle, quit)
	for {
		select {
		case result, channelOpen := <-resultsChan:
			if !channelOpen {
				return
			}

			updateCrawlStats(result, &stats, &server200TimeSum)
			results = append(results, *result)
		}
	}
}

func updateCrawlStats(result *HTTPResponse, stats *CrawlStats, total200Time *time.Duration) {
	stats.Total++

	statusCode := result.StatusCode
	serverTime := time.Duration(0)
	if result.Result != nil {
		serverTime = result.Result.Total(result.EndTime)
	}

	stats.StatusCodes[statusCode]++

	if statusCode == 200 {
		*total200Time += serverTime

		if serverTime > stats.Max200Time {
			stats.Max200Time = serverTime
		}
	} else {
		stats.Non200Urls = append(stats.Non200Urls, CrawlResult{
			URL:        result.URL,
			Time:       serverTime,
			StatusCode: statusCode,
		})
	}
}
