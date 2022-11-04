/*
 * Copyright (c) 2022, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	cli "github.com/urfave/cli/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	cmdutil "k8s.io/kubectl/pkg/cmd/util"
	"k8s.io/kubectl/pkg/drain"
)

var (
	log = logrus.New()

	nvidiaResourceNamePrefix = "nvidia.com/gpu"
	nvidiaMigResourcePrefix  = "nvidia.com/mig-"
)

// flags for the 'nvdrain' command
type flags struct {
	debug              bool
	dryRun             bool
	kubeconfig         string
	nodeName           string
	deleteEmptyDirData bool
	force              bool
	timeout            string
	gracePeriodSeconds int
}

func main() {
	flags := flags{}

	c := cli.NewApp()
	c.Name = "nvdrain"
	c.Usage = "Drain K8s pods on a node which have been allocated NVIDIA GPU"
	c.Version = "0.1.0"

	c.Flags = []cli.Flag{
		&cli.BoolFlag{
			Name:        "debug",
			Usage:       "Enable debug-level logging",
			Destination: &flags.debug,
			EnvVars:     []string{"NVDRAIN_DEBUG"},
		},
		&cli.BoolFlag{
			Name:        "dry-run",
			Usage:       "Print list of pods to be evicted",
			Destination: &flags.dryRun,
			EnvVars:     []string{"NVDRAIN_DRY_RUN"},
		},
		&cli.StringFlag{
			Name:        "kubeconfig",
			Value:       "",
			Usage:       "Absolute path to the kubeconfig file",
			Destination: &flags.kubeconfig,
			EnvVars:     []string{"KUBECONFIG"},
		},
		&cli.StringFlag{
			Name:        "node-name",
			Value:       "",
			Usage:       "The name of the node to drain",
			Destination: &flags.nodeName,
			EnvVars:     []string{"NODE_NAME"},
		},
		&cli.BoolFlag{
			Name:        "delete-emptydir-data",
			Usage:       "Continue even if there are pods using emptyDir",
			Destination: &flags.deleteEmptyDirData,
			EnvVars:     []string{"NVDRAIN_DELETE_EMPTYDIR_DATA"},
		},
		&cli.BoolFlag{
			Name:        "force",
			Aliases:     []string{"f"},
			Usage:       "Continue even if there are pods not managed by a ReplicationController, ReplicaSet, Job, DaemonSet, or StatefulSet",
			Destination: &flags.force,
			EnvVars:     []string{"NVDRAIN_USE_FORCE"},
		},
		&cli.StringFlag{
			Name:        "timeout",
			Aliases:     []string{"t"},
			Usage:       "The length of time to wait before giving up, zero means infinite",
			Value:       "0s",
			Destination: &flags.timeout,
			EnvVars:     []string{"NVDRAIN_TIMEOUT_SECONDS"},
		},
		&cli.IntFlag{
			Name:        "grace-period",
			Usage:       "Period of time in seconds given to each pod to terminate gracefully. If negative, the default value specified in the pod will be used.",
			Value:       -1,
			Destination: &flags.gracePeriodSeconds,
			EnvVars:     []string{"NVDRAIN_GRACE_PERIOD"},
		},
	}

	c.Before = func(c *cli.Context) error {
		err := validateFlags(&flags)
		if err != nil {
			cli.ShowAppHelp(c)
			return err
		}

		logLevel := logrus.InfoLevel
		if flags.debug {
			logLevel = logrus.DebugLevel
		}
		log.SetLevel(logLevel)
		return nil
	}

	c.Action = func(c *cli.Context) error {
		return nvdrainWrapper(c, &flags)
	}

	err := c.Run(os.Args)
	if err != nil {
		log.Fatalf(err.Error())
	}
}

func validateFlags(f *flags) error {
	var missing []string
	if f.nodeName == "" {
		missing = append(missing, "node-name")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required flags '%v'", strings.Join(missing, ", "))
	}
	return nil
}

func nvdrainWrapper(c *cli.Context, f *flags) error {
	clientConfig, err := clientcmd.BuildConfigFromFlags("", f.kubeconfig)
	if err != nil {
		return fmt.Errorf("error building kubernetes clientcmd config: %s", err)
	}

	clientset, err := kubernetes.NewForConfig(clientConfig)
	if err != nil {
		return fmt.Errorf("error building kubernetes clientset from config: %s", err)
	}

	timeout, err := time.ParseDuration(f.timeout)
	if err != nil {
		return fmt.Errorf("error parsing --timeout flag: %v", err)
	}

	drainHelper := drain.Helper{
		Ctx:                 context.TODO(),
		Client:              clientset,
		Out:                 os.Stdout,
		ErrOut:              os.Stderr,
		ChunkSize:           cmdutil.DefaultChunkSize,
		GracePeriodSeconds:  f.gracePeriodSeconds,
		IgnoreAllDaemonSets: true,
		DeleteEmptyDirData:  f.deleteEmptyDirData,
		Force:               f.force,
		Timeout:             timeout,
		AdditionalFilters:   []drain.PodFilter{gpuPodSpecFilter},
	}

	log.Infof("Getting pods for deletion...")
	podDeleteList, errs := drainHelper.GetPodsForDeletion(f.nodeName)
	for _, p := range podDeleteList.Pods() {
		log.Infof("  %s/%s", p.Namespace, p.Name)
	}

	warnings := podDeleteList.Warnings()
	if warnings != "" {
		log.Debugf("Warnings: %s", warnings)
	}

	if errs != nil {
		log.Infof("ERROR: following errors met when getting pods for deletion")
		for _, err = range errs {
			log.Infof("  %v", err)
		}
		return fmt.Errorf("error getting pods for deletion")
	}

	if len(podDeleteList.Pods()) == 0 {
		log.Infof("No pods to delete. Exiting.")
	}

	if f.dryRun {
		return nil
	}

	log.Debugf("Evicting NVIDIA pods...")
	err = drainHelper.DeleteOrEvictPods(podDeleteList.Pods())
	if err != nil {
		return fmt.Errorf("error evicting NVIDIA pods: %v", err)
	}

	return nil
}

func gpuPodSpecFilter(pod corev1.Pod) drain.PodDeleteStatus {
	gpuInResourceList := func(rl corev1.ResourceList) bool {
		for resourceName := range rl {
			str := string(resourceName)
			if strings.HasPrefix(str, nvidiaResourceNamePrefix) || strings.HasPrefix(str, nvidiaMigResourcePrefix) {
				return true
			}
		}
		return false
	}

	for _, c := range pod.Spec.Containers {
		if gpuInResourceList(c.Resources.Limits) {
			return drain.MakePodDeleteStatusOkay()
		}
		if gpuInResourceList(c.Resources.Requests) {
			return drain.MakePodDeleteStatusOkay()
		}
	}
	return drain.MakePodDeleteStatusSkip()
}
