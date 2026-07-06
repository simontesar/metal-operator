// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and IronCore contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"os"
	"time"

	metalv1alpha1 "github.com/ironcore-dev/metal-operator/api/v1alpha1"
	"github.com/ironcore-dev/metal-operator/cmd/metalprobe-mock-controller/probemock"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(metalv1alpha1.AddToScheme(scheme))
}

func main() {
	var (
		probeAddr             string
		enableLeaderElection  bool
		registryURL           string
		probeDuration         time.Duration
		registryClientTimeout time.Duration
		lldpSyncInterval      time.Duration
		lldpSyncDuration      time.Duration
	)

	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081",
		"The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.StringVar(&registryURL, "registry-url", "",
		"Registry URL where probe agents register server data.")
	flag.DurationVar(&probeDuration, "probe-duration", 100*time.Millisecond,
		"Backoff duration between probe registration retries.")
	flag.DurationVar(&registryClientTimeout, "registry-client-timeout", time.Second,
		"Timeout for HTTP requests to the registry.")
	flag.DurationVar(&lldpSyncInterval, "lldp-sync-interval", 50*time.Millisecond,
		"Interval between lldpctl runs inside the probe agent.")
	flag.DurationVar(&lldpSyncDuration, "lldp-sync-duration", 250*time.Millisecond,
		"Timeout for each lldpctl run inside the probe agent.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if registryURL == "" {
		setupLog.Error(nil, "registry-url is required")
		os.Exit(1)
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "c8f4a2e1.ironcore.dev",
	})
	if err != nil {
		setupLog.Error(err, "Unable to start manager")
		os.Exit(1)
	}

	ctx := ctrl.SetupSignalHandler()
	mocker := probemock.NewProbeMocker(
		mgr.GetClient(),
		ctrl.Log.WithName("probemock"),
		ctx,
		registryURL,
		probeDuration,
		registryClientTimeout,
		lldpSyncInterval,
		lldpSyncDuration,
	)

	if err := ctrl.NewControllerManagedBy(mgr).
		For(&metalv1alpha1.ServerBootConfiguration{}).
		Watches(&metalv1alpha1.Server{}, mocker.EnqueueRequestsForServer()).
		Complete(mocker); err != nil {
		setupLog.Error(err, "Unable to create controller")
		os.Exit(1)
	}

	if err := mgr.Add(mocker); err != nil {
		setupLog.Error(err, "Unable to add ProbeMocker runnable")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "Problem running manager")
		os.Exit(1)
	}
}
