package releaseutils

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	version "github.com/hashicorp/go-version"
	"helm.sh/helm/v3/pkg/release"
	"io"
	"slices"
)

type DiscoveredChartRelease struct {
	ChartName             string           `json:"chartName"`
	ChartRepo             string           `json:"chartRepo"`
	ReleaseName           string           `json:"releaseName"`
	ReleaseNamespace      string           `json:"releaseNamespace"`
	InstalledChartVersion *version.Version `json:"installedChartVersion"`
	AvailableChartVersion *version.Version `json:"availableChartVersion"`
	DiscoveryMethod       string           `json:"discoveryMethod"`
}

// a structure to keep track of charts installed via cattle HelmCharts that have an explicit repo defined
// the key is the release name, the value is the chart repo
// this saves some extra time and repo queries because we know exactly which repo the release was installed from
type KnownChartSources struct {
	KnownChartSources map[string]string
}

type DiscoveredChartReleases struct {
	DiscoveredChartReleases []DiscoveredChartRelease `json:"charts"`
	includeAll              bool
}

func (kcs *KnownChartSources) AddKnownChartSource(c, r string) (map[string]string, error) {
	if kcs.KnownChartSources == nil {
		kcs.KnownChartSources = make(map[string]string)
	}
	if _, ok := kcs.KnownChartSources[c]; !ok {
		kcs.KnownChartSources[c] = r
	}
	return kcs.KnownChartSources, nil
}

func NewDiscoveredChartRelease(chartName, chartRepo, releaseName, releaseNamespace string, installedChartVersion, availableChartVersion *version.Version, discoveryMethod string) (*DiscoveredChartRelease, error) {
	switch discoveryMethod {
	case "cattleCRD", "secret", "pgsql":
		return &DiscoveredChartRelease{
			ChartName:             chartName,
			ChartRepo:             chartRepo,
			ReleaseName:           releaseName,
			ReleaseNamespace:      releaseNamespace,
			InstalledChartVersion: installedChartVersion,
			AvailableChartVersion: availableChartVersion,
			DiscoveryMethod:       discoveryMethod,
		}, nil
	}
	return nil, fmt.Errorf("'%s' is not a valid discoveryMethod", discoveryMethod)
}

func NewDiscoveredChartReleases(includeAll bool) *DiscoveredChartReleases {
	return &DiscoveredChartReleases{[]DiscoveredChartRelease{}, includeAll}
}

func (dcrs *DiscoveredChartReleases) AddDiscoveredChartRelease(dcr *DiscoveredChartRelease) ([]DiscoveredChartRelease, error) {
	for _, dcrItem := range dcrs.DiscoveredChartReleases {
		if dcr.ReleaseName == dcrItem.ReleaseName && dcr.ReleaseNamespace == dcrItem.ReleaseNamespace {
			return dcrs.DiscoveredChartReleases, fmt.Errorf("Non-unique chart release: ReleaseName=%s, ReleaseNamespace=%s", dcr.ReleaseName, dcr.ReleaseNamespace)
		}

	}
	if (!dcrs.includeAll && dcr.AvailableChartVersion.GreaterThan(dcr.InstalledChartVersion)) || dcrs.includeAll {
		dcrs.DiscoveredChartReleases = append(dcrs.DiscoveredChartReleases, *dcr)
	}
	return dcrs.DiscoveredChartReleases, nil
}

// avoid adding duplicate entries
func (dcrs *DiscoveredChartReleases) FindByReleaseNameAndNamespace(releaseName, releaseNamespace string) int {
	return slices.IndexFunc(dcrs.DiscoveredChartReleases, func(dcr DiscoveredChartRelease) bool {
		return dcr.ReleaseName == releaseName && dcr.ReleaseNamespace == releaseNamespace
	})
}

type ProcessedRelease struct {
	ReleaseName      string
	ReleaseNamespace string
}

type ProcessedReleases struct {
	ProcessedReleases []ProcessedRelease
}

func (prs *ProcessedReleases) AddProcessedRelease(pr *ProcessedRelease) []ProcessedRelease {
	for _, prItem := range prs.ProcessedReleases {
		if pr.ReleaseName == prItem.ReleaseName && pr.ReleaseNamespace == prItem.ReleaseNamespace {
			return prs.ProcessedReleases
		}
	}

	prs.ProcessedReleases = append(prs.ProcessedReleases, *pr)
	return prs.ProcessedReleases
}

func (prs *ProcessedReleases) FindByReleaseNameAndNamespace(releaseName, releaseNamespace string) int {
	return slices.IndexFunc(prs.ProcessedReleases, func(pr ProcessedRelease) bool {
		return pr.ReleaseName == releaseName && pr.ReleaseNamespace == releaseNamespace
	})
}

func DecodeHelmRelease(releaseData []byte) (*release.Release, error) {
	var decodedRelease *release.Release

	gzippedReleaseData, err := base64.StdEncoding.DecodeString(string(releaseData))
	if err != nil {
		return nil, fmt.Errorf("cannot decode Helm release data: %s", err)
	}

	buf := bytes.NewBuffer(gzippedReleaseData)

	gzReader, err := gzip.NewReader(buf)
	if err != nil {
		return nil, fmt.Errorf("cannot gunzip Helm release data: %s", err)
	}
	defer gzReader.Close()

	uncompressedReleaseData, err := io.ReadAll(gzReader)
	if err != nil {
		return nil, fmt.Errorf("cannot gunzip Helm release data: %s", err)
	}

	if err := json.Unmarshal(uncompressedReleaseData, &decodedRelease); err != nil {
		return nil, fmt.Errorf("cannot unmarshal Helm release data: %s", err)
	}

	return decodedRelease, nil
}
