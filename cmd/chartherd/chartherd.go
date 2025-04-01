package main

import (
	"flag"
	"fmt"
	"github.com/evgeny-terekhov/chartherd/pkg/updatechecker"
	"go.senan.xyz/flagconf"
	"helm.sh/helm/v3/pkg/repo"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"log/slog"
	"os"
	"time"
)

func main() {
	const (
		kubeconfigEnvVarName      = "KUBECONFIG"
		inClusterEnvVarName       = "KUBERNETES_SERVICE_HOST"
		defaultInterval           = "1h"
		kubeconfigDefaultContext  = "default"
		contextTimeoutRaw         = "30s"
		defaultNamespace          = ""
		defaultConcurrentRequests = 10
		defaultOutput             = "table"
		defaultMetricsBindTo      = ":9420"
		defaultMetricsRoute       = "/metrics"
	)

	var (
		kubeconfigDefaultPath     = fmt.Sprintf("%s/.kube/config", os.Getenv("HOME"))
		defaultLocalHelmReposPath = fmt.Sprintf("%s/.config/helm/repositories.yaml", os.Getenv("HOME"))
		localHelmRepos            *repo.File
		flagKubeconfigPath        string
		flagKubeconfigContext     string
		flagDebug                 bool
		flagDaemon                bool
		flagIncludeAll            bool
		flagUseLocalHelmRepos     bool
		flagInterval              string
		flagLocalHelmReposPath    string
		flagCheckChartDeps        bool
		flagNamespace             string
		flagOutput                string
		flagConcurrentRequests    int
		flagMetricsEnabled        bool
		flagMetricsBindTo         string
		flagMetricsRoute          string
		logger                    *slog.Logger
		err                       error
		logLevel                  = new(slog.LevelVar)
		interval                  time.Duration
		clusterConfig             *rest.Config
		runningInCluster          bool
		realKubeconfigPath        string
		clientset                 *kubernetes.Clientset
	)

	flag.BoolVar(&flagDebug, "debug", false, "Enable debug logging.")
	flag.BoolVar(&flagUseLocalHelmRepos, "use-local-helm-repos", false, "Use Helm repositories data (repositories.yaml) from the local machine. Please note that every repo from the file might be queried for any installed chart, so be cautious about potential rate limiting and leaking your installed chart names. If the application is run in a Kubernetes cluster, the repositories file can be mounted to the Pod as a Volume.")
	flag.StringVar(&flagLocalHelmReposPath, "helm-repos-path", defaultLocalHelmReposPath, "Path to Helm repositories data (repositories.yaml) on the local machine. If the application is run in a Kubernetes cluster, make sure the file is mounted as a Volume.")
	flag.BoolVar(&flagCheckChartDeps, "check-chart-deps", false, "If a chart's source repo cannot be determined, try looking for it in its dependencies repos.")
	flag.BoolVar(&flagDaemon, "daemon", false, "Run continuously in the foreground. If not set and chartherd runs outside of a Kubernetes cluster, it will execute once and exit.")
	// flag.BoolVar(&flagDaemon, "d", false, "Run continuously in the foreground. If not set and the application is run outside of a Kubernetes cluster, it will run once and exit.")
	flag.StringVar(&flagKubeconfigPath, "kubeconfig", kubeconfigDefaultPath, "Path to the kubeconfig file. Can also be set via KUBECONFIG environment variable.")
	flag.StringVar(&flagKubeconfigContext, "kubeconfig-context", kubeconfigDefaultContext, "Kubeconfig context to use.")
	flag.StringVar(&flagInterval, "interval", defaultInterval, "Check interval when running in daemon mode.")
	// flag.StringVar(&flagInterval, "i", defaultInterval, "Check interval when running in daemon mode.")
	flag.StringVar(&flagNamespace, "namespace", defaultNamespace, "Limit the checks to releases in a single namespace. If not set, all namespaces will be checked for Helm releases.")
	// flag.StringVar(&flagNamespace, "n", defaultNamespace, "Limit the checks to releases in a single namespace. If not set, all namespaces will be checked for Helm releases.")
	flag.IntVar(&flagConcurrentRequests, "concurrent-requests", defaultConcurrentRequests, "Limit the number of concurrent Helm releases being checked and by extension the number of concurrent HTTP requests sent to repos/registries.")
	flag.StringVar(&flagOutput, "output", defaultOutput, "Output format of the results. Valid options are 'table', 'json' and 'none'.")
	// flag.StringVar(&flagOutput, "o", defaultOutput, "Output format of the results. Valid options are 'table', 'json' and 'none'")
	flag.BoolVar(&flagIncludeAll, "include-all", false, "Whether to report all Helm charts instead of only the ones that have an update.")
	// flag.BoolVar(&flagIncludeAll, "a", false, "Whether to report all Helm charts instead of only the ones that have an update")
	flag.BoolVar(&flagMetricsEnabled, "metrics-enabled", false, "Export the resulting output as Prometheus metrics.")
	flag.StringVar(&flagMetricsBindTo, "metrics-http-bind-to", defaultMetricsBindTo, "IP address and TCP port to bind the metrics exporter to in [HOST]:PORT format.")
	flag.StringVar(&flagMetricsRoute, "metrics-route", defaultMetricsRoute, "HTTP route for the metrics exporter.")

	flag.Parse()
	err = flagconf.ParseEnv()
	if err != nil {
		logger.Error(fmt.Sprintf("cannot parse environment variables: %s", err))
		panic("cannot parse environment variables, exiting")
	}

	switch flagOutput {
	case "table", "json", "none":
	default:
		panic(fmt.Sprintf("unknown output format '%s', exiting", flagOutput))
	}

	if flagDebug {
		logLevel.Set(slog.LevelDebug)
	}

	logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: logLevel,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key != slog.TimeKey {
				return a
			}
			t := a.Value.Time()
			a.Value = slog.StringValue(t.Format("2006-01-02 15:04:05.000"))
			return a
		},
	}))
	slog.SetDefault(logger)

	interval, err = time.ParseDuration(flagInterval)
	if err != nil && flagInterval != defaultInterval && flagDaemon {
		logger.Error(fmt.Sprintf("cannot parse check interval of %s, using default interval %s", flagInterval, defaultInterval))
		interval, err = time.ParseDuration(defaultInterval)
		if err != nil {
			logger.Error(fmt.Sprintf("something is very wrong: %s", err))
			panic("something is very wrong, exiting")
		}
	}

	contextTimeout, err := time.ParseDuration(contextTimeoutRaw)
	if err != nil {
		logger.Error(fmt.Sprintf("cannot parse context timeout of %s: %s", contextTimeoutRaw, err))
		panic("cannot parse context timeout, exiting")
	}

	if flagUseLocalHelmRepos {
		localHelmRepos, err = repo.LoadFile(flagLocalHelmReposPath)
		if err != nil {
			logger.Error(fmt.Sprintf("helm local repo file at %s could not be processed: %s", flagLocalHelmReposPath, err))
			panic("helm local repo file could not be processed")
		}
	}

	if os.Getenv(inClusterEnvVarName) != "" {
		runningInCluster = true
		logger.Info("running in a Kubernetes cluster, proceeding")
	} else {
		runningInCluster = false
		logger.Info("running outside of a Kubernetes, will try to locate the kubeconfig file now")
		if os.Getenv(kubeconfigEnvVarName) != "" {
			realKubeconfigPath = os.Getenv(kubeconfigEnvVarName)
			logger.Info(fmt.Sprintf("file '%s' found from %s environment variable, using it as kubeconfig", realKubeconfigPath, kubeconfigEnvVarName))
		} else {
			if _, err := os.Stat(flagKubeconfigPath); err == nil {
				realKubeconfigPath = flagKubeconfigPath
				logger.Info(fmt.Sprintf("file '%s' found from CLI argument, using it as kubeconfig", realKubeconfigPath))
			} else {
				logger.Warn(fmt.Sprintf("file '%s' could not be opened: %s", flagKubeconfigPath, err))
				panic("could not locate a kubeconfig file, exiting")
			}
		}
	}

	if runningInCluster {
		clusterConfig, err = rest.InClusterConfig()
		if err != nil {
			logger.Error(fmt.Sprintf("cannot connect to Kubernetes cluster: %s", err))
			panic("cannot connect to Kubernetes cluster, exiting")
		}
	} else {
		clusterConfigLoadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: realKubeconfigPath}
		clusterConfigOverrides := &clientcmd.ConfigOverrides{CurrentContext: flagKubeconfigContext}

		clusterConfig, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clusterConfigLoadingRules, clusterConfigOverrides).ClientConfig()
		if err != nil {
			logger.Error(fmt.Sprintf("cannot connect to Kubernetes cluster using '%s' kubeconfig file and '%s' context: %s", realKubeconfigPath, flagKubeconfigContext, err))
			panic("cannot connect to Kubernetes cluster, exiting")
		}
	}

	clientset, err = kubernetes.NewForConfig(clusterConfig)
	if err != nil {
		logger.Error(fmt.Sprintf("cannot connect to Kubernetes cluster: %s", err))
		panic("cannot connect to Kubernetes cluster, exiting")
	}

	logger.Info(fmt.Sprintf("successfully connected to the Kubernetes cluster at %s", clusterConfig.Host))

	updateChecker, err := updatechecker.NewUpdateChecker(logger, clientset, flagUseLocalHelmRepos, localHelmRepos, flagCheckChartDeps, flagNamespace, flagConcurrentRequests, contextTimeout, flagOutput, flagIncludeAll, flagMetricsEnabled, flagMetricsBindTo, flagMetricsRoute)
	if err != nil {
		logger.Error(fmt.Sprintf("cannot initialize update checker: %s", err))
		panic("cannot initialize update checker, exiting")
	}

	err = updateChecker.Run()
	if err != nil {
		logger.Error(fmt.Sprintf("error while performing Helm releases update check: %s", err))
	}

	if flagDaemon || runningInCluster {
		logger.Info(fmt.Sprintf("will perform the next check in %s", interval))
		ticker := time.NewTicker(interval)
		for {
			<-ticker.C
			err = updateChecker.Run()
			if err != nil {
				logger.Error(fmt.Sprintf("error while performing Helm releases update check: %s", err))
			}
			logger.Info(fmt.Sprintf("will perform the next check in %s", interval))
		}
	}
}
