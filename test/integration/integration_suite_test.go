/*
Copyright 2025.

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

package integration

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	"github.com/osac-project/osac-operator/test/utils"
)

const (
	operatorImage     = "localhost/osac-operator:latest"
	operatorNamespace = "osac-operator-system"
)

var _ = BeforeSuite(func() {
	By("installing CRDs via Helm")
	cmd := exec.Command("helm", "upgrade", "--install", "osac-operator-crds",
		"charts/operator-crds/", "--wait")
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	By("installing fake CRDs")
	cmd = exec.Command("kubectl", "apply", "--server-side", "-f", "config/crd/fakes/")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	By("building the operator image")
	cmd = exec.Command("make", "image-build", fmt.Sprintf("IMG=%s", operatorImage))
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	By("loading the operator image into the kind cluster")
	err = utils.LoadImageToKindClusterWithName(operatorImage)
	Expect(err).NotTo(HaveOccurred())

	By("deploying the operator via Helm")
	cmd = exec.Command("helm", "upgrade", "--install", "osac-operator",
		"charts/operator/",
		"--namespace", operatorNamespace,
		"--create-namespace",
		"--set", "image.repository=localhost/osac-operator",
		"--set", "image.tag=latest",
		"--set", "image.pullPolicy=Never",
		"--wait", "--timeout", "5m")
	_, err = utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred())

	By("waiting for controller-manager pod to be ready with zero restarts")
	// pod.status.phase stays "Running" even during CrashLoopBackOff — only
	// container status reveals the real state. Check that all containers are
	// ready AND have zero restarts to detect crash-looping controllers early.
	Eventually(func() error {
		cmd := exec.Command("kubectl", "get", "pods",
			"-l", "control-plane=controller-manager",
			"-n", operatorNamespace,
			"-o", "json")
		output, err := utils.Run(cmd)
		if err != nil {
			return err
		}

		var podList struct {
			Items []struct {
				Metadata struct {
					Name              string     `json:"name"`
					DeletionTimestamp *time.Time `json:"deletionTimestamp"`
				} `json:"metadata"`
				Status struct {
					Phase            string `json:"phase"`
					ContainerStatuses []struct {
						Ready        bool  `json:"ready"`
						RestartCount int64 `json:"restartCount"`
					} `json:"containerStatuses"`
				} `json:"status"`
			} `json:"items"`
		}
		if err := json.Unmarshal(output, &podList); err != nil {
			return fmt.Errorf("failed to parse pod list: %w", err)
		}

		var running int
		for _, pod := range podList.Items {
			if pod.Metadata.DeletionTimestamp != nil {
				continue
			}
			if pod.Status.Phase != "Running" {
				return fmt.Errorf("pod %s in %s phase", pod.Metadata.Name, pod.Status.Phase)
			}
			for _, cs := range pod.Status.ContainerStatuses {
				if !cs.Ready {
					return fmt.Errorf("pod %s has unready container", pod.Metadata.Name)
				}
				if cs.RestartCount > 0 {
					return fmt.Errorf("pod %s has %d restarts (crash-looping)", pod.Metadata.Name, cs.RestartCount)
				}
			}
			running++
		}
		if running == 0 {
			return fmt.Errorf("no running controller-manager pods found")
		}
		return nil
	}, 5*time.Minute, 5*time.Second).Should(Succeed())
})

var _ = AfterSuite(func() {
	By("undeploying the operator")
	cmd := exec.Command("helm", "uninstall", "osac-operator",
		"--namespace", operatorNamespace, "--ignore-not-found")
	_, _ = utils.Run(cmd)

	By("uninstalling CRDs")
	cmd = exec.Command("helm", "uninstall", "osac-operator-crds", "--ignore-not-found")
	_, _ = utils.Run(cmd)
})

func TestIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting osac-operator suite\n")
	RunSpecs(t, "integration suite")
}
