package provisioning_test

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/osac-project/osac-operator/pkg/provisioning"
)

var _ = Describe("ExtraVarsContext", func() {
	Describe("AdminKubeconfig", func() {
		It("should round-trip a kubeconfig value", func() {
			ctx := context.Background()
			kubeconfig := "apiVersion: v1\nclusters:\n- cluster:\n    server: https://example.com\n"

			ctx = provisioning.WithAdminKubeconfig(ctx, kubeconfig)
			result := provisioning.AdminKubeconfigFromContext(ctx)

			Expect(result).To(Equal(kubeconfig))
		})

		It("should return empty string from a context without kubeconfig", func() {
			ctx := context.Background()

			result := provisioning.AdminKubeconfigFromContext(ctx)

			Expect(result).To(BeEmpty())
		})
	})
})
