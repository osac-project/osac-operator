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

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2" //nolint:revive,staticcheck
	. "github.com/onsi/gomega"    //nolint:revive,staticcheck

	"github.com/osac-project/osac-operator/test/utils"
)

var _ = Describe("Tenant", Ordered, func() {
	const tenantName = "test-tenant-e2e"

	AfterAll(func() {
		By("cleaning up test tenants")
		cmd := exec.Command("kubectl", "delete", "tenant", "--all",
			"-n", operatorNamespace, "--ignore-not-found")
		_, _ = utils.Run(cmd)
	})

	Context("Valid CR creation", func() {
		It("should create a Tenant CR with valid spec", func() {
			By("creating a Tenant with valid displayName and emailDomains")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = createTenantYAML(tenantName, operatorNamespace,
				"ACME Corp", []string{"acme.com", "acme.net"})
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			By("verifying the Tenant exists")
			verifyExists := func() error {
				cmd := exec.Command("kubectl", "get", "tenant", tenantName,
					"-n", operatorNamespace, "-o", "jsonpath={.metadata.name}")
				output, err := utils.Run(cmd)
				if err != nil {
					return err
				}
				if string(output) != tenantName {
					return fmt.Errorf("expected Tenant name %s, got %s", tenantName, string(output))
				}
				return nil
			}
			Eventually(verifyExists, 30*time.Second, time.Second).Should(Succeed())
		})

		It("should have correct spec fields", func() {
			By("verifying displayName")
			cmd := exec.Command("kubectl", "get", "tenant", tenantName,
				"-n", operatorNamespace, "-o", "jsonpath={.spec.displayName}")
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(output)).To(Equal("ACME Corp"))

			By("verifying emailDomains")
			cmd = exec.Command("kubectl", "get", "tenant", tenantName,
				"-n", operatorNamespace, "-o", `jsonpath={.spec.emailDomains}`)
			output, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(output)).To(ContainSubstring("acme.com"))
			Expect(string(output)).To(ContainSubstring("acme.net"))
		})

		It("should show displayName in printcolumn output", func() {
			By("listing tenants and checking display name column")
			cmd := exec.Command("kubectl", "get", "tenant", tenantName,
				"-n", operatorNamespace)
			output, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Expect(string(output)).To(ContainSubstring("ACME Corp"))
		})
	})

	Context("Validation", func() {
		It("should reject Tenant with empty displayName", func() {
			By("attempting to create a Tenant with empty displayName")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = createTenantYAML("tenant-empty-name", operatorNamespace,
				"", []string{"example.com"})
			_, err := utils.Run(cmd)
			Expect(err).To(HaveOccurred())

			By("verifying the Tenant was not created")
			cmd = exec.Command("kubectl", "get", "tenant", "tenant-empty-name",
				"-n", operatorNamespace)
			_, err = utils.Run(cmd)
			Expect(err).To(HaveOccurred())
		})

		It("should reject Tenant with malformed email domain", func() {
			By("attempting to create a Tenant with an invalid email domain")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = createTenantYAML("tenant-bad-domain", operatorNamespace,
				"Bad Domain Corp", []string{"-bad.com"})
			_, err := utils.Run(cmd)
			Expect(err).To(HaveOccurred())

			By("verifying the Tenant was not created")
			cmd = exec.Command("kubectl", "get", "tenant", "tenant-bad-domain",
				"-n", operatorNamespace)
			_, err = utils.Run(cmd)
			Expect(err).To(HaveOccurred())
		})

		It("should reject Tenant without emailDomains", func() {
			By("attempting to create a Tenant without emailDomains field")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = createTenantYAMLNoEmailDomains("tenant-no-domains",
				operatorNamespace, "No Domains Corp")
			_, err := utils.Run(cmd)
			Expect(err).To(HaveOccurred())

			By("verifying the Tenant was not created")
			cmd = exec.Command("kubectl", "get", "tenant", "tenant-no-domains",
				"-n", operatorNamespace)
			_, err = utils.Run(cmd)
			Expect(err).To(HaveOccurred())
		})

		It("should reject Tenant with empty emailDomains list", func() {
			By("attempting to create a Tenant with empty emailDomains list")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = createTenantYAML("tenant-empty-domains", operatorNamespace,
				"Empty Domains Corp", []string{})
			_, err := utils.Run(cmd)
			Expect(err).To(HaveOccurred())

			By("verifying the Tenant was not created")
			cmd = exec.Command("kubectl", "get", "tenant", "tenant-empty-domains",
				"-n", operatorNamespace)
			_, err = utils.Run(cmd)
			Expect(err).To(HaveOccurred())
		})
	})
})

// createTenantYAML returns a Reader with Tenant YAML for kubectl apply.
func createTenantYAML(name, namespace, displayName string, emailDomains []string) *strings.Reader {
	domainsYAML := ""
	for _, d := range emailDomains {
		domainsYAML += fmt.Sprintf("    - %s\n", d)
	}
	yaml := fmt.Sprintf(`apiVersion: osac.openshift.io/v1alpha1
kind: Tenant
metadata:
  name: %s
  namespace: %s
spec:
  displayName: "%s"
  emailDomains:
%s`, name, namespace, displayName, domainsYAML)
	return strings.NewReader(yaml)
}

// createTenantYAMLNoEmailDomains returns a Reader with Tenant YAML that omits the emailDomains field.
func createTenantYAMLNoEmailDomains(name, namespace, displayName string) *strings.Reader {
	yaml := fmt.Sprintf(`apiVersion: osac.openshift.io/v1alpha1
kind: Tenant
metadata:
  name: %s
  namespace: %s
spec:
  displayName: "%s"
`, name, namespace, displayName)
	return strings.NewReader(yaml)
}
