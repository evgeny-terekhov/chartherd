package updatechecker

import (
	"encoding/json"
	"fmt"
	"github.com/evgeny-terekhov/chartherd/pkg/cattlechecker"
	"github.com/evgeny-terekhov/chartherd/pkg/fetchutils"
	"github.com/evgeny-terekhov/chartherd/pkg/metrics"
	"github.com/evgeny-terekhov/chartherd/pkg/releaseutils"
	"github.com/evgeny-terekhov/chartherd/pkg/secretchecker"
	"github.com/olekukonko/tablewriter"
	"helm.sh/helm/v3/pkg/repo"
	"k8s.io/client-go/kubernetes"
	"log/slog"
	"os"
	"time"
)

type UpdateChecker struct {
	logger                  *slog.Logger
	clientset               *kubernetes.Clientset
	useLocalHelmRepos       bool
	localHelmRepos          *repo.File
	checkChartDeps          bool
	namespace               string
	concurrentRequests      int
	contextTimeout          time.Duration
	outputFormat            string
	discoveredChartReleases *releaseutils.DiscoveredChartReleases
	processedReleases       *releaseutils.ProcessedReleases
	includeAll              bool
	metricsEnabled          bool
	metricsChan             chan *releaseutils.DiscoveredChartReleases
}

func NewUpdateChecker(logger *slog.Logger, clientset *kubernetes.Clientset, useLocalHelmRepos bool, localHelmRepos *repo.File, checkChartDeps bool, namespace string, concurrentRequests int, contextTimeout time.Duration, outputFormat string, includeAll bool, metricsEnabled bool, metricsBindTo string, metricsRoute string) (*UpdateChecker, error) {
	metricsChan := make(chan *releaseutils.DiscoveredChartReleases)
	if metricsEnabled {
		metricsExporter := metrics.NewMetricsExporter(logger, metricsBindTo, metricsRoute, metricsChan)
		go metricsExporter.Run()
	}

	return &UpdateChecker{
		logger:                  logger,
		clientset:               clientset,
		useLocalHelmRepos:       useLocalHelmRepos,
		localHelmRepos:          localHelmRepos,
		checkChartDeps:          checkChartDeps,
		namespace:               namespace,
		concurrentRequests:      concurrentRequests,
		contextTimeout:          contextTimeout,
		outputFormat:            outputFormat,
		discoveredChartReleases: &releaseutils.DiscoveredChartReleases{},
		includeAll:              includeAll,
		metricsEnabled:          metricsEnabled,
		metricsChan:             metricsChan,
	}, nil
}

func (uc *UpdateChecker) Run() error {
	uc.discoveredChartReleases = releaseutils.NewDiscoveredChartReleases(uc.includeAll)
	fetcher := fetchutils.NewFetcher(uc.logger)
	knownChartSources := &releaseutils.KnownChartSources{}
	processedReleases := &releaseutils.ProcessedReleases{}

	cattleChecker, err := cattlechecker.NewCattleChecker(uc.logger, uc.clientset, uc.namespace, uc.concurrentRequests, uc.discoveredChartReleases, processedReleases, knownChartSources, fetcher, uc.contextTimeout)
	if err != nil {
		uc.logger.Error(fmt.Sprintf("cannot initialize cattle CRD update checker, skipping: %s", err))
	}

	err = cattleChecker.Run()
	if err != nil {
		uc.logger.Error(fmt.Sprintf("cannot check Helm releases with cattle CRD backend, skipping: %s", err))
	}

	secretChecker, err := secretchecker.NewSecretChecker(uc.logger, uc.clientset, uc.useLocalHelmRepos, uc.localHelmRepos, uc.checkChartDeps, uc.namespace, uc.concurrentRequests, uc.discoveredChartReleases, processedReleases, knownChartSources, fetcher, uc.contextTimeout)
	if err != nil {
		uc.logger.Error(fmt.Sprintf("cannot initialize Secret update checker, skipping: %s", err))
	}

	err = secretChecker.Run()
	if err != nil {
		uc.logger.Error(fmt.Sprintf("cannot to check Helm releases with Secret backend, skipping: %s", err))
	}

	if uc.metricsEnabled {
		uc.metricsChan <- uc.discoveredChartReleases
	}

	err = uc.output()
	if err != nil {
		return fmt.Errorf("cannot output the update check results: %s", err)
	}
	// we don't need the struct anymore so set the pointer to nil for GC to take care of it
	// memory consumption in daemon mode grows uncontrollably otherwise
	uc.discoveredChartReleases = nil

	return nil
}

func (uc *UpdateChecker) output() error {
	switch uc.outputFormat {
	case "none":
	case "json":
		dcrJSON, err := json.MarshalIndent(uc.discoveredChartReleases.DiscoveredChartReleases, "", "  ")
		if err != nil {
			return fmt.Errorf("cannot marshal discovered Helm releases to JSON: %s", err)
		}
		fmt.Print(string(dcrJSON))

	case "table":
		table := tablewriter.NewWriter(os.Stdout)
		table.SetHeader([]string{"Chart name", "Chart repo", "Release name", "Release namespace", "Cur ver", "New ver", "Method"})
		table.SetAutoWrapText(false)
		table.SetAutoFormatHeaders(true)
		table.SetHeaderAlignment(tablewriter.ALIGN_LEFT)
		table.SetAlignment(tablewriter.ALIGN_LEFT)
		table.SetCenterSeparator("")
		table.SetColumnSeparator("")
		table.SetRowSeparator("")
		table.SetHeaderLine(false)
		table.SetBorder(false)
		table.SetTablePadding("\t")
		table.SetNoWhiteSpace(true)
		for _, dcr := range uc.discoveredChartReleases.DiscoveredChartReleases {
			table.Append([]string{dcr.ChartName, dcr.ChartRepo, dcr.ReleaseName, dcr.ReleaseNamespace, dcr.InstalledChartVersion.Original(), dcr.AvailableChartVersion.Original(), dcr.DiscoveryMethod})
		}
		table.Render()
	}

	return nil
}
