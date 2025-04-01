package secretchecker

import (
	"cmp"
	"context"
	"fmt"
	"github.com/evgeny-terekhov/chartherd/pkg/fetchutils"
	"github.com/evgeny-terekhov/chartherd/pkg/releaseutils"
	version "github.com/hashicorp/go-version"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/repo"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type SecretChecker struct {
	logger                  *slog.Logger
	clientset               *kubernetes.Clientset
	useLocalHelmRepos       bool
	localHelmRepos          *repo.File
	checkChartDeps          bool
	namespace               string
	concurrentRequests      int
	discoveredChartReleases *releaseutils.DiscoveredChartReleases
	processedReleases       *releaseutils.ProcessedReleases
	knownChartSources       *releaseutils.KnownChartSources
	fetcher                 *fetchutils.Fetcher
	contextTimeout          time.Duration
}

type releaseNameAndNamespace struct {
	releaseName      string
	releaseNamespace string
}

type releaseNamesAndNamespaces struct {
	releaseNamesAndNamespaces []releaseNameAndNamespace
}

type secretRevision struct {
	revision    int
	releaseData []byte
}

type secretRevisionsByRelease struct {
	secretRevisionsByRelease map[releaseNameAndNamespace][]secretRevision
}

func NewSecretChecker(logger *slog.Logger, clientset *kubernetes.Clientset, useLocalHelmRepos bool, localHelmRepos *repo.File, checkChartDeps bool, namespace string, concurrentRequests int, discoveredChartReleases *releaseutils.DiscoveredChartReleases, processedReleases *releaseutils.ProcessedReleases, knownChartSources *releaseutils.KnownChartSources, fetcher *fetchutils.Fetcher, contextTimeout time.Duration) (*SecretChecker, error) {
	return &SecretChecker{
		logger:                  logger,
		clientset:               clientset,
		useLocalHelmRepos:       useLocalHelmRepos,
		localHelmRepos:          localHelmRepos,
		checkChartDeps:          checkChartDeps,
		namespace:               namespace,
		concurrentRequests:      concurrentRequests,
		discoveredChartReleases: discoveredChartReleases,
		processedReleases:       processedReleases,
		knownChartSources:       knownChartSources,
		fetcher:                 fetcher,
		contextTimeout:          contextTimeout,
	}, nil
}

func (sc *SecretChecker) Run() error {
	err := sc.checkSecretReleases()
	if err != nil {
		sc.logger.Error(fmt.Sprintf("unable to check Secret releases, skipping: %s", err))
		return err
	}

	return nil
}

func newReleaseNameAndNamespace(s *corev1.Secret) (*releaseNameAndNamespace, error) {
	_, err := isHelmSecret(s)
	if err != nil {
		return nil, err
	}

	releaseInfo := strings.Split(s.ObjectMeta.Name, ".")
	releaseName := releaseInfo[4]
	releaseNamespace := s.ObjectMeta.Namespace

	return &releaseNameAndNamespace{
		releaseName:      releaseName,
		releaseNamespace: releaseNamespace,
	}, nil

}

func newSecretRevision(s *corev1.Secret) (*secretRevision, error) {
	_, err := isHelmSecret(s)
	if err != nil {
		return nil, err
	}

	releaseInfo := strings.Split(s.ObjectMeta.Name, ".")
	releaseRevision, err := strconv.Atoi(strings.TrimPrefix(releaseInfo[5], "v"))
	if err != nil {
		return nil, err
	}

	return &secretRevision{
		revision:    releaseRevision,
		releaseData: s.Data["release"],
	}, nil
}

func (rnns *releaseNamesAndNamespaces) newOrExistingReleaseNameAndNamespace(s *corev1.Secret) (releaseNameAndNamespace, error) {
	_, err := isHelmSecret(s)
	if err != nil {
		return releaseNameAndNamespace{}, err
	}

	releaseInfo := strings.Split(s.ObjectMeta.Name, ".")
	// sh.helm.release.v1.{NAME}.v3
	releaseName := releaseInfo[4]
	releaseNamespace := s.ObjectMeta.Namespace
	rnnIdx := slices.IndexFunc(rnns.releaseNamesAndNamespaces, func(rnnInList releaseNameAndNamespace) bool {
		return rnnInList.releaseName == releaseName && rnnInList.releaseNamespace == releaseNamespace
	})
	if rnnIdx == -1 {
		newrnn, err := newReleaseNameAndNamespace(s)
		if err != nil {
			return releaseNameAndNamespace{}, err
		}
		rnns.releaseNamesAndNamespaces = append(rnns.releaseNamesAndNamespaces, *newrnn)
		return *newrnn, nil
	} else {
		return rnns.releaseNamesAndNamespaces[rnnIdx], nil
	}
}

func (srbr *secretRevisionsByRelease) AddsecretRevisionByRelease(rnn releaseNameAndNamespace, sr *secretRevision) (map[releaseNameAndNamespace][]secretRevision, error) {
	if srbr.secretRevisionsByRelease == nil {
		srbr.secretRevisionsByRelease = make(map[releaseNameAndNamespace][]secretRevision)
	}
	if _, ok := srbr.secretRevisionsByRelease[rnn]; ok {
		for _, srItem := range srbr.secretRevisionsByRelease[rnn] {
			if sr.revision == srItem.revision {
				return srbr.secretRevisionsByRelease, fmt.Errorf("non-unique chart revision: releaseName=%s, releaseNamespace=%s, revision=%d", rnn.releaseName, rnn.releaseNamespace, sr.revision)
			}
		}
		srbr.secretRevisionsByRelease[rnn] = append(srbr.secretRevisionsByRelease[rnn], *sr)
	} else {
		srbr.secretRevisionsByRelease[rnn] = []secretRevision{*sr}
	}
	return srbr.secretRevisionsByRelease, nil
}

func (srbr *secretRevisionsByRelease) sortRevisions() map[releaseNameAndNamespace][]secretRevision {
	for rnn := range srbr.secretRevisionsByRelease {
		slices.SortFunc(srbr.secretRevisionsByRelease[rnn], func(a, b secretRevision) int {
			return cmp.Compare(b.revision, a.revision)
		})
	}
	return srbr.secretRevisionsByRelease
}

func isHelmSecret(s *corev1.Secret) (bool, error) {
	if s.Type != "helm.sh/release.v1" {
		return false, fmt.Errorf("expected a Secret of type 'helm.sh/release.v1', got %s", s.Type)
	}
	SecretNameRegexMatch, err := regexp.MatchString(`sh\.helm\.release\.v1\..*\.v\d+`, s.ObjectMeta.Name)
	if err != nil {
		return false, fmt.Errorf("failed to match regexp for %s string: %s", s.ObjectMeta.Name, err.Error())
	}

	if !SecretNameRegexMatch {
		return false, fmt.Errorf("%s Secret name is not valid for a Helm release Secret", s.ObjectMeta.Name)
	}
	return true, nil

}

func (sc *SecretChecker) listSecretResources(ctx context.Context) (*secretRevisionsByRelease, error) {
	secretListResult, err := sc.clientset.CoreV1().Secrets(sc.namespace).List(ctx, metav1.ListOptions{FieldSelector: "type=helm.sh/release.v1"})
	if err != nil {
		return nil, fmt.Errorf("cannot list Secret resources: %s", err)
	}

	releaseNamesAndNamespaces := releaseNamesAndNamespaces{}
	secretRevisionsByRelease := secretRevisionsByRelease{}
	for _, secret := range secretListResult.Items {
		releaseNameAndNamespace, err := releaseNamesAndNamespaces.newOrExistingReleaseNameAndNamespace(&secret)

		if err != nil {
			sc.logger.Error(fmt.Sprintf("failed to process release name and namespace: %s", err))
			continue
		}

		secretRevision, err := newSecretRevision(&secret)
		if err != nil {
			sc.logger.Error(fmt.Sprintf("failed to process release revision: %s", err))
			continue
		}

		_, err = secretRevisionsByRelease.AddsecretRevisionByRelease(releaseNameAndNamespace, secretRevision)
		if err != nil {
			sc.logger.Error(fmt.Sprintf("cannot process release revision: %s", err))
			continue
		}
	}

	secretRevisionsByRelease.sortRevisions()
	return &secretRevisionsByRelease, nil
}

func (sc *SecretChecker) checkSecretReleases() error {
	var waitGroup sync.WaitGroup

	ctx, cancel := context.WithTimeout(context.Background(), sc.contextTimeout)
	defer cancel()

	secretRevisionsByRelease, err := sc.listSecretResources(ctx)
	if err != nil {
		return fmt.Errorf("failed to list Secret resources, skipping: %s", err)
	}

	sc.logger.Debug("start checking individual releases")
	errChan := make(chan error, len(secretRevisionsByRelease.secretRevisionsByRelease))
	limitChan := make(chan struct{}, sc.concurrentRequests)
	for releaseNameAndNamespace, secretRevisions := range secretRevisionsByRelease.secretRevisionsByRelease {
		if existingPRindex := sc.processedReleases.FindByReleaseNameAndNamespace(releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace); existingPRindex != -1 {
			sc.logger.Debug(fmt.Sprintf("release '%s' in '%s' namespace has already been processed, skipping", releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace))
		} else if existingDCRindex := sc.discoveredChartReleases.FindByReleaseNameAndNamespace(releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace); existingDCRindex != -1 {
			sc.logger.Debug(fmt.Sprintf("update for release '%s' in '%s' namespace has already been discovered via %s, skipping", releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace, sc.discoveredChartReleases.DiscoveredChartReleases[existingDCRindex].DiscoveryMethod))
		} else {
			limitChan <- struct{}{}
			waitGroup.Add(1)
			go sc.checkSecretRelease(ctx, &waitGroup, errChan, limitChan, &releaseNameAndNamespace, &secretRevisions)
		}
	}
	waitGroup.Wait()
	close(errChan)

	for checkSecretReleaseErr := range errChan {
		if checkSecretReleaseErr != nil {
			sc.logger.Error(fmt.Sprintf("cannot check Secret object: %s", checkSecretReleaseErr))
		}
	}

	sc.logger.Debug("done checking individual Secret objects")

	return nil
}

func (sc *SecretChecker) checkSecretRelease(ctx context.Context, waitGroup *sync.WaitGroup, errChan chan error, limitChan chan struct{}, releaseNameAndNamespace *releaseNameAndNamespace, secretRevisions *[]secretRevision) {
	defer func() {
		<-limitChan
		waitGroup.Done()
	}()

	select {
	case <-ctx.Done():
		errChan <- fmt.Errorf("cannot check '%s' release revision: %s", releaseNameAndNamespace.releaseName, ctx.Err())
		return
	default:
		for _, secretRevision := range *secretRevisions {
			sc.logger.Debug(fmt.Sprintf("processing '%s' release in '%s' namespace, revision %d", releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace, secretRevision.revision))

			decodedRelease, err := releaseutils.DecodeHelmRelease(secretRevision.releaseData)
			if err != nil {
				sc.logger.Debug(fmt.Sprintf("cannot decode '%s' Helm Release data: %s", releaseNameAndNamespace.releaseName, err))
				continue
			}

			if decodedRelease.Info.Status == "superseded" {
				sc.logger.Debug(fmt.Sprintf("'%s' release in '%s' namespace, revision %d has status of '%s', no more active revisions ahead, skipping this release altogether", releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace, secretRevision.revision, decodedRelease.Info.Status))
				break
			}

			if decodedRelease.Info.Status != "deployed" {
				sc.logger.Debug(fmt.Sprintf("'%s' release in '%s' namespace, revision %d has status of '%s', skipping this revision", releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace, secretRevision.revision, decodedRelease.Info.Status))
				continue
			}

			currentVersion, err := version.NewVersion(decodedRelease.Chart.Metadata.Version)
			if err != nil {
				sc.logger.Debug(fmt.Sprintf("invalid '%s' release in '%s' namespace revision %d current version of %s: %s", releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace, secretRevision.revision, decodedRelease.Chart.Metadata.Version, err))
				continue
			}
			sc.logger.Debug(fmt.Sprintf("'%s' release in '%s' namespace, revision %d: current version %s", releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace, secretRevision.revision, decodedRelease.Chart.Metadata.Version))

			var latestVersion *version.Version
			if kcs, ok := sc.knownChartSources.KnownChartSources[releaseNameAndNamespace.releaseName]; ok {
				if strings.HasPrefix(kcs, "oci://") {
					latestVersion, err = sc.fetcher.FetchOCIChartLatestVersion(ctx, kcs)
					if err != nil {
						sc.logger.Debug(fmt.Sprintf("'%s' release in '%s' namespace, revision %d: could not fetch %s chart version: %s", releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace, secretRevision.revision, kcs, err))
						continue
					}
				} else {
					latestVersion, err = sc.fetcher.FetchHTTPChartLatestVersion(ctx, kcs, decodedRelease.Chart.Metadata.Name)
					if err != nil {
						sc.logger.Debug(fmt.Sprintf("'%s' release in '%s' namespace, revision %d: could not fetch %s chart version from %s repo: %s", releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace, secretRevision.revision, decodedRelease.Chart.Metadata.Name, kcs, err))
						continue
					}
				}

				dcr, err := releaseutils.NewDiscoveredChartRelease(decodedRelease.Chart.Metadata.Name, kcs, releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace, currentVersion, latestVersion, "secret")
				if err != nil {
					sc.logger.Debug(fmt.Sprintf("cannot process %s chart version information: %s", decodedRelease.Chart.Metadata.Name, err))
				}

				_, err = sc.discoveredChartReleases.AddDiscoveredChartRelease(dcr)
				if err != nil {
					sc.logger.Debug(fmt.Sprintf("cannot process %s chart version information: %s", decodedRelease.Chart.Metadata.Name, err))
				} else {
					sc.logger.Debug(fmt.Sprintf("'%s' release in '%s' namespace, revision %d: saved latest version %s", releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace, secretRevision.revision, latestVersion.Original()))
					break
				}
			}

			if sc.useLocalHelmRepos {
				for _, repoEntry := range sc.localHelmRepos.Repositories {
					latestVersion, err = sc.fetcher.FetchHTTPChartLatestVersion(ctx, repoEntry.URL, decodedRelease.Chart.Metadata.Name)
					if err != nil {
						sc.logger.Debug(fmt.Sprintf("'%s' release in '%s' namespace, revision %d: could not fetch %s chart version from %s repo: %s", releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace, secretRevision.revision, decodedRelease.Chart.Metadata.Name, repoEntry.URL, err))
						continue
					}

					dcr, err := releaseutils.NewDiscoveredChartRelease(decodedRelease.Chart.Metadata.Name, repoEntry.URL, releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace, currentVersion, latestVersion, "secret")
					if err != nil {
						sc.logger.Debug(fmt.Sprintf("cannot process %s chart version information: %s", decodedRelease.Chart.Metadata.Name, err))
					}

					_, err = sc.discoveredChartReleases.AddDiscoveredChartRelease(dcr)
					if err != nil {
						sc.logger.Debug(fmt.Sprintf("cannot process %s chart version information: %s", decodedRelease.Chart.Metadata.Name, err))
					} else {
						sc.logger.Debug(fmt.Sprintf("'%s' release in '%s' namespace, revision %d: saved latest version %s", releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace, secretRevision.revision, latestVersion.Original()))
						break
					}
				}
			}

			// last resort: try guessing the chart's source repo by its dependencies
			// might work for some oci:// charts
			if sc.checkChartDeps {
				dependenciesGuessResult := sc.guessRepoByDependencies(ctx, decodedRelease, secretRevision.revision)
				if !dependenciesGuessResult {
					sc.logger.Debug(fmt.Sprintf("'%s' release in '%s' namespace, revision %d: could not guess chart repo from its dependencies", releaseNameAndNamespace.releaseName, releaseNameAndNamespace.releaseNamespace, secretRevision.revision))
					break
				}
			}
		}

		sc.processedReleases.AddProcessedRelease(&releaseutils.ProcessedRelease{ReleaseName: releaseNameAndNamespace.releaseName, ReleaseNamespace: releaseNameAndNamespace.releaseNamespace})
		errChan <- nil
		return
	}
}

func (sc *SecretChecker) guessRepoByDependencies(ctx context.Context, release *release.Release, revision int) bool {
	var processedDependenicesRepos []string
	var guessSuccessful = false
	var maybeLatestVersion *version.Version
	var maybeChartRepo string

	sc.logger.Debug(fmt.Sprintf("'%s' release in '%s' namespace, revision %d: trying to guess chart repo from its dependencies", release.Name, release.Namespace, revision))
	for _, dependency := range release.Chart.Metadata.Dependencies {
		currentVersion, err := version.NewVersion(release.Chart.Metadata.Version)
		if err != nil {
			sc.logger.Debug(fmt.Sprintf("invalid '%s' chart version of %s: %s", release.Name, release.Chart.Metadata.Version, err))
			continue
		}
		if slices.Contains(processedDependenicesRepos, dependency.Repository) {
			sc.logger.Debug(fmt.Sprintf("%s dependency repo has already been processed", dependency.Repository))
			continue
		}
		if strings.HasPrefix(dependency.Repository, "oci://") {
			maybeChartRepo = fmt.Sprintf("%s/%s", dependency.Repository, release.Chart.Metadata.Name)
			maybeLatestVersion, err = sc.fetcher.FetchOCIChartLatestVersion(ctx, maybeChartRepo)
			if err != nil {
				sc.logger.Debug(fmt.Sprintf("cannot fetch latest version of chart %s: %s [guessed from dependency]", maybeChartRepo, err))
				processedDependenicesRepos = append(processedDependenicesRepos, dependency.Repository)
				continue
			}
		} else {
			maybeChartRepo = dependency.Repository
			maybeLatestVersion, err = sc.fetcher.FetchHTTPChartLatestVersion(ctx, dependency.Repository, release.Chart.Metadata.Name)
			if err != nil {
				sc.logger.Debug(fmt.Sprintf("cannot fetch %s chart version from %s repo: %s [guessed from dependency]", release.Chart.Metadata.Name, dependency.Repository, err))
				processedDependenicesRepos = append(processedDependenicesRepos, dependency.Repository)
				continue
			}

		}
		guessSuccessful = true
		sc.logger.Debug(fmt.Sprintf("'%s' release in '%s' namespace, revision %d: latest version %s", release.Name, release.Namespace, revision, maybeLatestVersion.Original()))

		dcr, err := releaseutils.NewDiscoveredChartRelease(release.Chart.Metadata.Name, maybeChartRepo, release.Name, release.Namespace, currentVersion, maybeLatestVersion, "secret")
		if err != nil {
			sc.logger.Debug(fmt.Sprintf("cannot process %s chart version information: %s", release.Chart.Metadata.Name, err))
			processedDependenicesRepos = append(processedDependenicesRepos, dependency.Repository)
			continue
		}
		_, err = sc.discoveredChartReleases.AddDiscoveredChartRelease(dcr)
		if err != nil {
			sc.logger.Debug(fmt.Sprintf("cannot process %s chart version information: %s", release.Chart.Metadata.Name, err))
			processedDependenicesRepos = append(processedDependenicesRepos, dependency.Repository)
			continue

		}

	}
	return guessSuccessful

}
