// Copyright © 2017 The virtual-kubelet authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/liqotech/liqo/cmd/virtual-kubelet/internal/commands/providers"
	"github.com/liqotech/liqo/cmd/virtual-kubelet/internal/commands/root"
	"github.com/liqotech/liqo/cmd/virtual-kubelet/internal/commands/version"
	"github.com/liqotech/liqo/cmd/virtual-kubelet/internal/provider"
	"github.com/liqotech/liqo/internal/log"
	logruslogger "github.com/liqotech/liqo/internal/log/logrus"
	"github.com/liqotech/liqo/internal/trace"
	"github.com/liqotech/liqo/internal/trace/opencensus"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

var (
	buildVersion      = "N/A"
	buildTime         = "N/A"
	defaultK8sVersion = "v1.18.2" // This should follow the version of k8s.io/kubernetes we are importing
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
	}()

	log.L = logruslogger.FromLogrus(logrus.NewEntry(logrus.StandardLogger()))
	trace.T = opencensus.Adapter{}

	var opts root.Opts
	optsErr := root.SetDefaultOpts(&opts)

	opts.Version = getK8sVersion(ctx, opts.KubeConfigPath)

	s := provider.NewStore()

	rootCmd := root.NewCommand(ctx, filepath.Base(os.Args[0]), s, opts)
	rootCmd.AddCommand(version.NewCommand(buildVersion, buildTime), providers.NewCommand(s))
	preRun := rootCmd.PreRunE

	var logLevel string
	rootCmd.PreRunE = func(cmd *cobra.Command, args []string) error {
		if optsErr != nil {
			return optsErr
		}
		if preRun != nil {
			return preRun(cmd, args)
		}
		return nil
	}

	rootCmd.PersistentFlags().StringVar(&logLevel, "log-level", "info", `set the log level, e.g. "debug", "info", "warn", "error"`)

	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		if logLevel != "" {
			lvl, err := logrus.ParseLevel(logLevel)
			if err != nil {
				return errors.Wrap(err, "could not parse log level")
			}
			logrus.SetLevel(lvl)
		}
		return nil
	}

	if err := registerKubernetes(s); err != nil {
		log.G(ctx).Fatal(err)
	}

	if err := rootCmd.Execute(); err != nil && errors.Cause(err) != context.Canceled {
		log.G(ctx).Fatal(err)
	}

}

func getK8sVersion(ctx context.Context, defaultConfigPath string) string {
	var config *rest.Config
	var configPath string

	for i := 1; i < len(os.Args); i++ {
		if os.Args[i] == "--kubeconfig" {
			configPath = os.Args[i+1]
		}
	}
	if configPath == "" {
		configPath = defaultConfigPath
	}

	// Check if the kubeConfig file exists.
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		// Get the kubeconfig from the filepath.
		config, err = clientcmd.BuildConfigFromFlags("", configPath)
		if err != nil {
			log.G(ctx).Warnf("Cannot read k8s version: using default version %v; error: %v", defaultK8sVersion, err)
			return defaultK8sVersion
		}
	} else {
		// Set to in-cluster config.
		config, err = rest.InClusterConfig()
		if err != nil {
			log.G(ctx).Warnf("Cannot read k8s version: using default version %v; error: %v", defaultK8sVersion, err)
			return defaultK8sVersion
		}
	}

	c, err := discovery.NewDiscoveryClientForConfig(config)
	if err != nil {
		log.G(ctx).Warnf("Cannot read k8s version: using default version %v; error: %v", defaultK8sVersion, err)
		return defaultK8sVersion
	}
	v, err := c.ServerVersion()
	if err != nil {
		log.G(ctx).Warnf("Cannot read k8s version: using default version %v; error: %v", defaultK8sVersion, err)
		return defaultK8sVersion
	}

	return v.GitVersion
}
