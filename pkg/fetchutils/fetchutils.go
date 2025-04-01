package fetchutils

import (
	"context"
	"fmt"
	version "github.com/hashicorp/go-version"
	"helm.sh/helm/v3/pkg/cli"
	"helm.sh/helm/v3/pkg/getter"
	"helm.sh/helm/v3/pkg/registry"
	"helm.sh/helm/v3/pkg/repo"
	"log/slog"
	"strings"
)

type Fetcher struct {
	logger     *slog.Logger
	fetchCache *FetchCache
}

type FetchCache struct {
	logger     *slog.Logger
	FetchCache map[chartNameAndRepo]*version.Version
}

type chartNameAndRepo struct {
	chartName string
	chartRepo string
}

func NewFetcher(logger *slog.Logger) *Fetcher {
	return &Fetcher{logger: logger, fetchCache: &FetchCache{logger: logger}}
}

func (fc *FetchCache) addCacheEntry(cn, cr string, cv *version.Version) map[chartNameAndRepo]*version.Version {
	if fc.FetchCache == nil {
		fc.FetchCache = make(map[chartNameAndRepo]*version.Version)
	}
	for k := range fc.FetchCache {
		if k.chartName == cn && k.chartRepo == cr {
			fc.logger.Debug(fmt.Sprintf("%s version for %s/%s chart is already cached", cv.Original(), cr, cn))
			return fc.FetchCache
		}
	}

	cnar := chartNameAndRepo{chartName: cn, chartRepo: cr}
	fc.FetchCache[cnar] = cv
	fc.logger.Debug(fmt.Sprintf("cached %s version for %s/%s chart", cv.Original(), cr, cn))
	return fc.FetchCache
}

func (fc *FetchCache) getCacheEntry(cn, cr string) (bool, *version.Version) {
	var cachedVersion *version.Version
	foundInCache := false

	if fc.FetchCache != nil {
		for k := range fc.FetchCache {
			if k.chartName == cn && k.chartRepo == cr {
				foundInCache = true
				cachedVersion = fc.FetchCache[k]
				break
			}
		}
	}
	return foundInCache, cachedVersion

}

func (f *Fetcher) FetchOCIChartLatestVersion(ctx context.Context, chartURI string) (*version.Version, error) {
	var latestVersion *version.Version
	select {
	case <-ctx.Done():
		return latestVersion, ctx.Err()
	default:
		foundInCache, latestVersion := f.fetchCache.getCacheEntry(chartURI, "")
		if foundInCache {
			f.logger.Debug(fmt.Sprintf("got cached version %s for %s chart", latestVersion.Original(), chartURI))
			return latestVersion, nil
		}

		registryClient, err := registry.NewClient()
		if err != nil {
			return latestVersion, err
		}
		tags, err := registryClient.Tags(strings.TrimPrefix(chartURI, "oci://"))
		if err != nil {
			return latestVersion, err
		}

		latestVersionRaw := tags[0]
		latestVersion, err = version.NewVersion(latestVersionRaw)
		if err != nil {
			return latestVersion, fmt.Errorf("invalid '%s' chart latest version of %s: %s", chartURI, latestVersionRaw, err)
		}
		f.logger.Debug(fmt.Sprintf("fetched %s version for %s chart", latestVersion.Original(), chartURI))
		f.fetchCache.addCacheEntry(chartURI, "", latestVersion)

		return latestVersion, nil
	}
}

func (f *Fetcher) FetchHTTPChartLatestVersion(ctx context.Context, repoURI, chartName string) (*version.Version, error) {
	var latestVersion *version.Version
	select {
	case <-ctx.Done():
		return latestVersion, ctx.Err()
	default:
		foundInCache, latestVersion := f.fetchCache.getCacheEntry(chartName, repoURI)
		if foundInCache {
			f.logger.Debug(fmt.Sprintf("got cached version %s for %s chart from %s repo", latestVersion.Original(), chartName, repoURI))
			return latestVersion, nil
		}

		repoName := fmt.Sprintf("%s-repo", chartName)
		repository, err := repo.NewChartRepository(&repo.Entry{
			Name: repoName,
			URL:  repoURI,
		}, getter.All(&cli.EnvSettings{}))
		if err != nil {
			return latestVersion, fmt.Errorf("cannot initialize Helm repo %s: %s", repoURI, err)
		}

		idx, err := repository.DownloadIndexFile()
		if err != nil {
			return latestVersion, fmt.Errorf("%s is not a valid chart repository or cannot be reached: %s", repoURI, err)
		}

		repoIndex, err := repo.LoadIndexFile(idx)
		if err != nil {
			return latestVersion, err
		}

		latestVersionCV, err := repoIndex.Get(chartName, "")
		if err != nil {
			return latestVersion, fmt.Errorf("%s not found in %s repository", chartName, repoURI)
		}
		latestVersionRaw := latestVersionCV.Metadata.Version
		latestVersion, err = version.NewVersion(latestVersionRaw)
		if err != nil {
			return latestVersion, fmt.Errorf("invalid '%s' chart latest version of %s: %s", chartName, latestVersionRaw, err)
		}
		f.logger.Debug(fmt.Sprintf("fetched %s version for %s chart from %s repo", latestVersion.Original(), chartName, repoURI))
		f.fetchCache.addCacheEntry(chartName, repoURI, latestVersion)

		return latestVersion, nil
	}
}
