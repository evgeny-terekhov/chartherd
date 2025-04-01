package cattlechecker

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/evgeny-terekhov/chartherd/pkg/fetchutils"
	"github.com/evgeny-terekhov/chartherd/pkg/releaseutils"
	version "github.com/hashicorp/go-version"
	helmcattleio "github.com/k3s-io/helm-controller/pkg/apis/helm.cattle.io"
	helmcattleiov1 "github.com/k3s-io/helm-controller/pkg/apis/helm.cattle.io/v1"
	"k8s.io/client-go/kubernetes"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type CattleChecker struct {
	logger                   *slog.Logger
	clientset                *kubernetes.Clientset
	namespace                string
	concurrentRequests       int
	cattleCRDName            string
	cattleAPIPath            string
	cattleResourceNamePlural string
	discoveredChartReleases  *releaseutils.DiscoveredChartReleases
	processedReleases        *releaseutils.ProcessedReleases
	knownChartSources        *releaseutils.KnownChartSources
	fetcher                  *fetchutils.Fetcher
	contextTimeout           time.Duration
}

func NewCattleChecker(logger *slog.Logger, clientset *kubernetes.Clientset, namespace string, concurrentRequests int, discoveredChartReleases *releaseutils.DiscoveredChartReleases, processedReleases *releaseutils.ProcessedReleases, knownChartSources *releaseutils.KnownChartSources, fetcher *fetchutils.Fetcher, contextTimeout time.Duration) (*CattleChecker, error) {
	cattleCRDName := fmt.Sprintf("%s.%s", helmcattleiov1.HelmChartResourceName, helmcattleio.GroupName)
	cattleAPIPath := fmt.Sprintf("/apis/%s/v1", helmcattleio.GroupName)
	cattleResourceNamePlural := helmcattleiov1.HelmChartResourceName
	return &CattleChecker{
		logger:                   logger,
		clientset:                clientset,
		namespace:                namespace,
		concurrentRequests:       concurrentRequests,
		cattleCRDName:            cattleCRDName,
		cattleAPIPath:            cattleAPIPath,
		cattleResourceNamePlural: cattleResourceNamePlural,
		discoveredChartReleases:  discoveredChartReleases,
		processedReleases:        processedReleases,
		knownChartSources:        knownChartSources,
		fetcher:                  fetcher,
		contextTimeout:           contextTimeout,
	}, nil
}

func (cc *CattleChecker) Run() error {
	cattleCRDExists, err := cc.checkCattleCRD()
	if err != nil {
		return fmt.Errorf("cannot check if HelmChart CRD exists: %s", err)
	}
	if cattleCRDExists {
		err := cc.checkCattleReleases()
		if err != nil {
			return fmt.Errorf("cannot check HelmChart releases: %s", err)
		}
	}

	return nil
}

func (cc *CattleChecker) checkCattleCRD() (bool, error) {
	var cattleCRDResult map[string]interface{}

	ctx, cancel := context.WithTimeout(context.Background(), cc.contextTimeout)
	defer cancel()

	data, err := cc.clientset.RESTClient().
		Get().
		AbsPath("/apis/apiextensions.k8s.io/v1").
		Resource("customresourcedefinitions").
		Name(cc.cattleCRDName).
		DoRaw(ctx)
	if err != nil {
		return false, fmt.Errorf("cannot get %s CRD: %s", cc.cattleCRDName, err)
	}

	if err := json.Unmarshal(data, &cattleCRDResult); err != nil {
		return false, fmt.Errorf("cannot unmarshall Kubernetes API response: %s", err)
	}

	if cattleCRDResult["kind"].(string) == "Status" && cattleCRDResult["code"].(float64) == 404 {
		cc.logger.Debug(fmt.Sprintf("%s CRD not found in the cluster", cc.cattleCRDName))
		return false, nil
	}
	if cattleCRDResult["kind"].(string) == "CustomResourceDefinition" && cattleCRDResult["metadata"].(map[string]interface{})["name"].(string) == cc.cattleCRDName {
		cc.logger.Debug(fmt.Sprintf("%s CRD found in the cluster", cc.cattleCRDName))
		return true, nil
	}

	return false, nil
}

func (cc *CattleChecker) listCattleResources(ctx context.Context) (*helmcattleiov1.HelmChartList, error) {
	var helmChartsListResult *helmcattleiov1.HelmChartList

	data, err := cc.clientset.RESTClient().
		Get().
		AbsPath(cc.cattleAPIPath).
		Resource(cc.cattleResourceNamePlural).
		DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("cannot list %s/%s resources: %s", cc.cattleAPIPath, cc.cattleResourceNamePlural, err)
	}

	if err := json.Unmarshal(data, &helmChartsListResult); err != nil {
		return nil, fmt.Errorf("cannot unmarshal the Kubernetes API response: %s", err)
	}

	return helmChartsListResult, nil
}

func (cc *CattleChecker) checkCattleReleases() error {
	var waitGroup sync.WaitGroup

	ctx, cancel := context.WithTimeout(context.Background(), cc.contextTimeout)
	defer cancel()

	cattleResourcesList, err := cc.listCattleResources(ctx)
	if err != nil {
		return fmt.Errorf("cannot list HelmChart custom resources, skipping: %s", err)
	}

	cc.logger.Debug("started checking individual HelmChart objects")
	errChan := make(chan error, len(cattleResourcesList.Items))
	limitChan := make(chan struct{}, cc.concurrentRequests)

	for _, cattleResource := range cattleResourcesList.Items {
		if (cc.namespace == "") || (cc.namespace != "" && cattleResource.Spec.TargetNamespace == cc.namespace) {
			limitChan <- struct{}{}
			waitGroup.Add(1)
			go cc.checkCattleRelease(ctx, &waitGroup, errChan, limitChan, &cattleResource)
		}
	}
	waitGroup.Wait()
	close(errChan)

	for checkCattleReleaseErr := range errChan {
		if checkCattleReleaseErr != nil {
			cc.logger.Error(fmt.Sprintf("cannot check an individual HelmChart object: %s", checkCattleReleaseErr))
		}
	}

	cc.logger.Debug("finished checking individual HelmChart objects")
	return nil
}

func (cc *CattleChecker) checkCattleRelease(ctx context.Context, waitGroup *sync.WaitGroup, errChan chan error, limitChan chan struct{}, cattleResource *helmcattleiov1.HelmChart) {
	defer func() {
		<-limitChan
		waitGroup.Done()
	}()

	select {
	case <-ctx.Done():
		errChan <- fmt.Errorf("cannot check '%s' HelmChart object: %w", cattleResource.ObjectMeta.Name, ctx.Err())
		return
	default:
		cc.logger.Debug(fmt.Sprintf("processing %s HelmChart", cattleResource.ObjectMeta.Name))
		if cattleResource.Spec.Version == "" {
			if strings.HasPrefix(cattleResource.Spec.Chart, "oci://") {
				cc.knownChartSources.AddKnownChartSource(cattleResource.ObjectMeta.Name, cattleResource.Spec.Chart)
			} else {
				cc.knownChartSources.AddKnownChartSource(cattleResource.ObjectMeta.Name, cattleResource.Spec.Repo)
			}
			cc.logger.Debug(fmt.Sprintf("%s HelmChart is not versioned, we'll discover its version later", cattleResource.ObjectMeta.Name))
			errChan <- nil
			return
		}
		currentVersion, err := version.NewVersion(cattleResource.Spec.Version)
		if err != nil {
			errChan <- fmt.Errorf("invalid '%s' chart version of %s: %s", cattleResource.ObjectMeta.Name, cattleResource.Spec.Version, err)
			return
		}
		cc.logger.Debug(fmt.Sprintf("%s HelmChart is versioned", cattleResource.ObjectMeta.Name))
		if strings.HasPrefix(cattleResource.Spec.Chart, "oci://") {
			latestVersion, err := cc.fetcher.FetchOCIChartLatestVersion(ctx, cattleResource.Spec.Chart)
			if err != nil {
				errChan <- fmt.Errorf("cannot fetch latest version of chart %s: %s", cattleResource.Spec.Chart, err)
				return
			}
			cc.logger.Debug(fmt.Sprintf("chart %s, current version %s", cattleResource.Spec.Chart, currentVersion.Original()))
			cc.logger.Debug(fmt.Sprintf("chart %s, latest version %s", cattleResource.Spec.Chart, latestVersion.Original()))

			dcr, err := releaseutils.NewDiscoveredChartRelease(cattleResource.ObjectMeta.Name, cattleResource.Spec.Chart, cattleResource.ObjectMeta.Name, cattleResource.Spec.TargetNamespace, currentVersion, latestVersion, "cattleCRD")
			if err != nil {
				errChan <- fmt.Errorf("cannot process %s chart version information: %s", cattleResource.ObjectMeta.Name, err)
				return
			}
			_, err = cc.discoveredChartReleases.AddDiscoveredChartRelease(dcr)
			if err != nil {
				errChan <- fmt.Errorf("cannot process %+v chart version information: %s", dcr, err)
				return
			}

			cc.processedReleases.AddProcessedRelease(&releaseutils.ProcessedRelease{ReleaseName: cattleResource.ObjectMeta.Name, ReleaseNamespace: cattleResource.Spec.TargetNamespace})
			errChan <- nil
			return
		} else {
			latestVersion, err := cc.fetcher.FetchHTTPChartLatestVersion(ctx, cattleResource.Spec.Repo, cattleResource.Spec.Chart)
			if err != nil {
				errChan <- fmt.Errorf("'%s' release in '%s' namespace: could not fetch %s chart version from %s repo: %s", cattleResource.ObjectMeta.Name, cattleResource.Spec.TargetNamespace, cattleResource.Spec.Chart, cattleResource.Spec.Repo, err)
				return
			}
			dcr, err := releaseutils.NewDiscoveredChartRelease(cattleResource.Spec.Chart, cattleResource.Spec.Repo, cattleResource.ObjectMeta.Name, cattleResource.Spec.TargetNamespace, currentVersion, latestVersion, "cattleCRD")
			if err != nil {
				errChan <- fmt.Errorf("cannot process %s chart version information: %s", cattleResource.Spec.Chart, err)
				return
			}

			_, err = cc.discoveredChartReleases.AddDiscoveredChartRelease(dcr)
			if err != nil {
				errChan <- fmt.Errorf("cannot process %s chart version information: %s", cattleResource.ObjectMeta.Name, err)
			} else {
				cc.processedReleases.AddProcessedRelease(&releaseutils.ProcessedRelease{ReleaseName: cattleResource.ObjectMeta.Name, ReleaseNamespace: cattleResource.Spec.TargetNamespace})
				cc.logger.Debug(fmt.Sprintf("'%s' release in '%s' namespace: saved latest version %s", cattleResource.ObjectMeta.Name, cattleResource.Spec.TargetNamespace, latestVersion.Original()))
			}
		}

		errChan <- nil
		return
	}
}
