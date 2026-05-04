/*


Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"flag"
	"fmt"
	"os"

	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	_ "k8s.io/client-go/plugin/pkg/client/auth/gcp"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	powerv1alpha1 "github.com/openshift-kni/kubernetes-power-manager/api/v1alpha1"
	"github.com/openshift-kni/kubernetes-power-manager/internal/scaling"
	"github.com/openshift-kni/kubernetes-power-manager/pkg/podresourcesclient"

	"github.com/intel/power-optimization-library/pkg/power"
	"github.com/openshift-kni/kubernetes-power-manager/controllers"
	"github.com/openshift-kni/kubernetes-power-manager/pkg/podstate"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(powerv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	flag.StringVar(&metricsAddr, "metrics-addr", ":10001", "The address the metric endpoint binds to.")
	logOpts := zap.Options{}
	logOpts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(
		zap.UseDevMode(true),
		func(o *zap.Options) {
			o.TimeEncoder = zapcore.ISO8601TimeEncoder
		},
		zap.UseFlagOptions(&logOpts),
	),
	)
	nodeName := os.Getenv("NODE_NAME")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: server.Options{BindAddress: metricsAddr},
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}
	power.SetLogger(ctrl.Log.WithName("powerLibrary"))
	powerLibrary, err := power.CreateInstance(nodeName)
	if powerLibrary == nil {
		setupLog.Error(err, "unable to create Power Library instance")
		os.Exit(1)
	}

	for id, feature := range powerLibrary.GetFeaturesInfo() {
		setupLog.Info(
			"feature status",
			"feature", feature.Name(),
			"driver", feature.Driver(),
			"error", feature.FeatureError(),
			"available", power.IsFeatureSupported(id))
		if id == power.FrequencyScalingFeature {
			govs := power.GetAvailableGovernors()
			setupLog.Info(fmt.Sprintf("available governors: %v", govs))
		}
		if id == power.CStatesFeature {
			cstates := power.GetAvailableCStates()
			setupLog.Info(fmt.Sprintf("available c-states: %v", cstates))
		}
	}

	powerNodeState, err := podstate.NewState()
	if err != nil {
		setupLog.Error(err, "unable to create internal state")
		os.Exit(1)
	}
	podResourcesClient, err := podresourcesclient.NewDualSocketPodClient()

	if err != nil {
		setupLog.Error(err, "unable to create internal client")
		os.Exit(1)
	}

	dpdkClient := scaling.NewDPDKTelemetryClient(
		ctrl.Log.WithName("clients").WithName("DPDKClient"),
	)
	defer dpdkClient.Close()

	cpuScalingMgr := scaling.NewCPUScalingManager(&powerLibrary, dpdkClient)
	if err = mgr.Add(cpuScalingMgr); err != nil {
		setupLog.Error(err, "unable to register runnable", "runnable", "CPUScalingManager")
		os.Exit(1)
	}

	if err = (&controllers.PowerProfileReconciler{
		Client:       mgr.GetClient(),
		Log:          ctrl.Log.WithName("controllers").WithName("PowerProfile"),
		Scheme:       mgr.GetScheme(),
		PowerLibrary: powerLibrary,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PowerProfile")
		os.Exit(1)
	}
	if err = (&controllers.PowerNodeConfigReconciler{
		Client:       mgr.GetClient(),
		Log:          ctrl.Log.WithName("controllers").WithName("PowerNodeConfig"),
		Scheme:       mgr.GetScheme(),
		PowerLibrary: powerLibrary,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PowerNodeConfig")
		os.Exit(1)
	}
	if err = (&controllers.PowerPodReconciler{
		Client:              mgr.GetClient(),
		Log:                 ctrl.Log.WithName("controllers").WithName("PowerPod"),
		Scheme:              mgr.GetScheme(),
		State:               powerNodeState,
		PodResourcesClient:  *podResourcesClient,
		PowerLibrary:        powerLibrary,
		DPDKTelemetryClient: dpdkClient,
		CPUScalingManager:   cpuScalingMgr,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PowerPod")
		os.Exit(1)
	}
	if err = (&controllers.UncoreReconciler{
		Client:       mgr.GetClient(),
		Log:          ctrl.Log.WithName("controllers").WithName("Uncore"),
		Scheme:       mgr.GetScheme(),
		PowerLibrary: powerLibrary,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Uncore")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
