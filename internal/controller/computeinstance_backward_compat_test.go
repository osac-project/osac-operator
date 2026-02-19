package controller

import (
	"encoding/json"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	osacv1alpha1 "github.com/osac-project/osac-operator/api/v1alpha1"
)

var _ = Describe("ensureBackwardCompatibility", func() {
	var instance *osacv1alpha1.ComputeInstance

	BeforeEach(func() {
		instance = &osacv1alpha1.ComputeInstance{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-instance",
				Namespace: "test-namespace",
			},
			Spec: osacv1alpha1.ComputeInstanceSpec{
				TemplateID: "test-template",
				Image: osacv1alpha1.ImageSpec{
					SourceType: osacv1alpha1.ImageSourceTypeRegistry,
					SourceRef:  "quay.io/fedora/fedora-coreos:stable",
				},
				Cores:       4,
				MemoryGiB:   8,
				BootDisk:    osacv1alpha1.DiskSpec{SizeGiB: 30},
				RunStrategy: osacv1alpha1.RunStrategyAlways,
			},
		}
	})

	Context("when templateParameters is not set", func() {
		It("should populate templateParameters from new fields", func() {
			Expect(ensureBackwardCompatibility(instance)).To(Succeed())

			Expect(instance.Spec.TemplateParameters).ToNot(BeEmpty())

			var params map[string]string
			err := json.Unmarshal([]byte(instance.Spec.TemplateParameters), &params)
			Expect(err).ToNot(HaveOccurred())

			Expect(params["cpu_cores"]).To(Equal("4"))
			Expect(params["memory"]).To(Equal("8Gi"))
			Expect(params["disk_size"]).To(Equal("30Gi"))
			Expect(params["image_source"]).To(Equal("quay.io/fedora/fedora-coreos:stable"))
			Expect(params["exposed_ports"]).To(Equal("22/tcp"))
		})

		It("should include ssh_public_key if sshKey is set", func() {
			instance.Spec.SSHKey = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ..."

			Expect(ensureBackwardCompatibility(instance)).To(Succeed())

			var params map[string]string
			err := json.Unmarshal([]byte(instance.Spec.TemplateParameters), &params)
			Expect(err).ToNot(HaveOccurred())

			Expect(params["ssh_public_key"]).To(Equal("ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQ..."))
		})

		It("should not include ssh_public_key if sshKey is empty", func() {
			instance.Spec.SSHKey = ""

			Expect(ensureBackwardCompatibility(instance)).To(Succeed())

			var params map[string]string
			err := json.Unmarshal([]byte(instance.Spec.TemplateParameters), &params)
			Expect(err).ToNot(HaveOccurred())

			Expect(params).ToNot(HaveKey("ssh_public_key"))
		})

		It("should always include exposed_ports", func() {
			instance.Spec.SSHKey = ""

			Expect(ensureBackwardCompatibility(instance)).To(Succeed())

			var params map[string]string
			err := json.Unmarshal([]byte(instance.Spec.TemplateParameters), &params)
			Expect(err).ToNot(HaveOccurred())

			// exposed_ports should be present even without SSH key
			Expect(params["exposed_ports"]).To(Equal("22/tcp"))
		})

		It("should format memory with Gi suffix", func() {
			instance.Spec.MemoryGiB = 16

			Expect(ensureBackwardCompatibility(instance)).To(Succeed())

			var params map[string]string
			err := json.Unmarshal([]byte(instance.Spec.TemplateParameters), &params)
			Expect(err).ToNot(HaveOccurred())

			Expect(params["memory"]).To(Equal("16Gi"))
		})

		It("should format disk_size with Gi suffix", func() {
			instance.Spec.BootDisk.SizeGiB = 100

			Expect(ensureBackwardCompatibility(instance)).To(Succeed())

			var params map[string]string
			err := json.Unmarshal([]byte(instance.Spec.TemplateParameters), &params)
			Expect(err).ToNot(HaveOccurred())

			Expect(params["disk_size"]).To(Equal("100Gi"))
		})

		It("should handle minimal valid spec", func() {
			instance.Spec.Cores = 1
			instance.Spec.MemoryGiB = 1
			instance.Spec.BootDisk.SizeGiB = 1
			instance.Spec.SSHKey = ""

			Expect(ensureBackwardCompatibility(instance)).To(Succeed())

			var params map[string]string
			err := json.Unmarshal([]byte(instance.Spec.TemplateParameters), &params)
			Expect(err).ToNot(HaveOccurred())

			Expect(params["cpu_cores"]).To(Equal("1"))
			Expect(params["memory"]).To(Equal("1Gi"))
			Expect(params["disk_size"]).To(Equal("1Gi"))
			Expect(params["exposed_ports"]).To(Equal("22/tcp"))
		})
	})

	Context("when templateParameters is already set", func() {
		It("should not overwrite existing templateParameters", func() {
			existingParams := `{"custom_param": "custom_value"}`
			instance.Spec.TemplateParameters = existingParams

			Expect(ensureBackwardCompatibility(instance)).To(Succeed())

			// Should remain unchanged
			Expect(instance.Spec.TemplateParameters).To(Equal(existingParams))
		})

		It("should preserve user-provided templateParameters even if empty fields exist", func() {
			userParams := `{"cpu_cores": "8", "memory": "16Gi"}`
			instance.Spec.TemplateParameters = userParams
			instance.Spec.Cores = 4 // Different from user-provided value

			Expect(ensureBackwardCompatibility(instance)).To(Succeed())

			// User-provided values should take precedence
			Expect(instance.Spec.TemplateParameters).To(Equal(userParams))
		})
	})

	Context("parameter name mapping", func() {
		It("should map cores to cpu_cores", func() {
			instance.Spec.Cores = 2

			Expect(ensureBackwardCompatibility(instance)).To(Succeed())

			var params map[string]string
			err := json.Unmarshal([]byte(instance.Spec.TemplateParameters), &params)
			Expect(err).ToNot(HaveOccurred())

			Expect(params).To(HaveKey("cpu_cores"))
			Expect(params).ToNot(HaveKey("cores"))
		})

		It("should map memoryGiB to memory", func() {
			instance.Spec.MemoryGiB = 4

			Expect(ensureBackwardCompatibility(instance)).To(Succeed())

			var params map[string]string
			err := json.Unmarshal([]byte(instance.Spec.TemplateParameters), &params)
			Expect(err).ToNot(HaveOccurred())

			Expect(params).To(HaveKey("memory"))
			Expect(params).ToNot(HaveKey("memoryGiB"))
		})

		It("should map bootDisk.sizeGiB to disk_size", func() {
			instance.Spec.BootDisk.SizeGiB = 50

			Expect(ensureBackwardCompatibility(instance)).To(Succeed())

			var params map[string]string
			err := json.Unmarshal([]byte(instance.Spec.TemplateParameters), &params)
			Expect(err).ToNot(HaveOccurred())

			Expect(params).To(HaveKey("disk_size"))
			Expect(params).ToNot(HaveKey("bootDisk"))
		})

		It("should map image.sourceRef to image_source", func() {
			instance.Spec.Image.SourceRef = "custom.registry.io/image:tag"

			Expect(ensureBackwardCompatibility(instance)).To(Succeed())

			var params map[string]string
			err := json.Unmarshal([]byte(instance.Spec.TemplateParameters), &params)
			Expect(err).ToNot(HaveOccurred())

			Expect(params).To(HaveKey("image_source"))
			Expect(params).ToNot(HaveKey("image"))
			Expect(params).ToNot(HaveKey("sourceRef"))
		})

		It("should map sshKey to ssh_public_key", func() {
			instance.Spec.SSHKey = "test-key"

			Expect(ensureBackwardCompatibility(instance)).To(Succeed())

			var params map[string]string
			err := json.Unmarshal([]byte(instance.Spec.TemplateParameters), &params)
			Expect(err).ToNot(HaveOccurred())

			Expect(params).To(HaveKey("ssh_public_key"))
			Expect(params).ToNot(HaveKey("sshKey"))
		})
	})
})
