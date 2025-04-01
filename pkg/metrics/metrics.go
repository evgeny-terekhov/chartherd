package metrics

import (
	"fmt"
	"github.com/evgeny-terekhov/chartherd/pkg/releaseutils"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"log/slog"
	"net/http"
	"time"
)

type MetricsExporter struct {
	logger       *slog.Logger
	chartsMetric *prometheus.GaugeVec
	httpBindTo   string
	httpRoute    string
	metricsChan  <-chan *releaseutils.DiscoveredChartReleases
}

func NewMetricsExporter(logger *slog.Logger, httpBindTo string, httpRoute string, metricsChan <-chan *releaseutils.DiscoveredChartReleases) *MetricsExporter {
	chartsMetric := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "chartherd_charts",
			Help: "The Helm chart in the Kubernetes cluster that has an update available",
		},
		[]string{"chart_name", "chart_repo", "release_name", "release_namespace", "current_version", "available_version", "discovery_method"},
	)

	return &MetricsExporter{
		logger:       logger,
		chartsMetric: chartsMetric,
		httpBindTo:   httpBindTo,
		httpRoute:    httpRoute,
		metricsChan:  metricsChan,
	}
}

func (me *MetricsExporter) Run() {
	prometheus.MustRegister(me.chartsMetric)
	go me.startListener(me.httpBindTo, me.httpRoute)

	go func() {
		for newMetrics := range me.metricsChan {
			me.chartsMetric.Reset()
			for _, dcr := range newMetrics.DiscoveredChartReleases {
				me.chartsMetric.With(prometheus.Labels{
					"chart_name":        dcr.ChartName,
					"chart_repo":        dcr.ChartRepo,
					"release_name":      dcr.ReleaseName,
					"release_namespace": dcr.ReleaseNamespace,
					"current_version":   dcr.InstalledChartVersion.Original(),
					"available_version": dcr.AvailableChartVersion.Original(),
					"discovery_method":  dcr.DiscoveryMethod,
				}).Set(1)
			}
		}
	}()
}

func (me *MetricsExporter) startListener(httpBindTo, httpRoute string) {
	const restartDelaySeconds = 30

	defer func() {
		if r := recover(); r != nil {
			me.logger.Error(fmt.Sprintf("metrics listener has died, restarting in %d seconds: %s", restartDelaySeconds, r.(error)))
			time.Sleep(restartDelaySeconds * time.Second)
			go me.startListener(httpBindTo, httpRoute)
		}
	}()
	me.logger.Info(fmt.Sprintf("starting metrics listener at %s, route %s", httpBindTo, httpRoute))

	http.Handle(httpRoute, promhttp.Handler())
	http.ListenAndServe(httpBindTo, nil)
}
