package aap_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/innabox/cloudkit-operator/internal/aap"
)

func TestAAP(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "AAP Suite")
}

var _ = Describe("Client", func() {
	var (
		client *aap.Client
		server *httptest.Server
		ctx    context.Context
	)

	BeforeEach(func() {
		ctx = context.Background()
	})

	AfterEach(func() {
		if server != nil {
			server.Close()
		}
	})

	Describe("LaunchJobTemplate", func() {
		Context("when request succeeds", func() {
			BeforeEach(func() {
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					Expect(r.URL.Path).To(Equal("/api/v2/job_templates/test-template/launch/"))
					Expect(r.Method).To(Equal(http.MethodPost))
					Expect(r.Header.Get("Content-Type")).To(Equal("application/json"))
					Expect(r.Header.Get("Authorization")).To(ContainSubstring("Bearer test-token"))

					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode(map[string]interface{}{
						"id": 123,
					})
				}))
				client = aap.NewClient(server.URL, "test-token")
			})

			It("should return job ID", func() {
				resp, err := client.LaunchJobTemplate(ctx, aap.LaunchJobTemplateRequest{
					TemplateName: "test-template",
					ExtraVars:    map[string]interface{}{"key": "value"},
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.JobID).To(Equal(123))
			})
		})

		Context("when request fails", func() {
			BeforeEach(func() {
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusNotFound)
					w.Write([]byte("template not found"))
				}))
				client = aap.NewClient(server.URL, "test-token")
			})

			It("should return error", func() {
				_, err := client.LaunchJobTemplate(ctx, aap.LaunchJobTemplateRequest{
					TemplateName: "missing-template",
				})
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("404"))
			})
		})
	})

	Describe("LaunchWorkflowTemplate", func() {
		Context("when request succeeds", func() {
			BeforeEach(func() {
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					Expect(r.URL.Path).To(Equal("/api/v2/workflow_job_templates/test-workflow/launch/"))
					Expect(r.Method).To(Equal(http.MethodPost))

					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode(map[string]interface{}{
						"id": 456,
					})
				}))
				client = aap.NewClient(server.URL, "test-token")
			})

			It("should return job ID", func() {
				resp, err := client.LaunchWorkflowTemplate(ctx, aap.LaunchWorkflowTemplateRequest{
					TemplateName: "test-workflow",
					ExtraVars:    map[string]interface{}{"workflow_var": "value"},
				})
				Expect(err).NotTo(HaveOccurred())
				Expect(resp.JobID).To(Equal(456))
			})
		})
	})

	Describe("GetJob", func() {
		Context("when job exists", func() {
			BeforeEach(func() {
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					Expect(r.URL.Path).To(Equal("/api/v2/jobs/789/"))
					Expect(r.Method).To(Equal(http.MethodGet))

					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode(map[string]interface{}{
						"id":               789,
						"status":           "successful",
						"started":          time.Now().UTC().Format(time.RFC3339),
						"finished":         time.Now().UTC().Add(time.Minute).Format(time.RFC3339),
						"extra_vars":       map[string]interface{}{"key": "value"},
						"result_traceback": "",
					})
				}))
				client = aap.NewClient(server.URL, "test-token")
			})

			It("should return job details", func() {
				job, err := client.GetJob(ctx, 789)
				Expect(err).NotTo(HaveOccurred())
				Expect(job.ID).To(Equal(789))
				Expect(job.Status).To(Equal("successful"))
			})
		})

		Context("when job does not exist", func() {
			BeforeEach(func() {
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusNotFound)
				}))
				client = aap.NewClient(server.URL, "test-token")
			})

			It("should return error", func() {
				_, err := client.GetJob(ctx, 999)
				Expect(err).To(HaveOccurred())
			})
		})
	})

	Describe("GetTemplateByName", func() {
		Context("when template is a job_template", func() {
			BeforeEach(func() {
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v2/job_templates/" && r.URL.Query().Get("name") == "my-job" {
						w.WriteHeader(http.StatusOK)
						json.NewEncoder(w).Encode(map[string]interface{}{
							"count": 1,
							"results": []map[string]interface{}{
								{"id": 19, "name": "my-job"},
							},
						})
					} else {
						w.WriteHeader(http.StatusOK)
						json.NewEncoder(w).Encode(map[string]interface{}{"count": 0, "results": []interface{}{}})
					}
				}))
				client = aap.NewClient(server.URL, "test-token")
			})

			It("should return job template", func() {
				template, err := client.GetTemplateByName(ctx, "my-job")
				Expect(err).NotTo(HaveOccurred())
				Expect(template.Type).To(Equal(aap.TemplateTypeJob))
				Expect(template.Name).To(Equal("my-job"))
				Expect(template.ID).To(Equal(19))
			})
		})

		Context("when template is a workflow_job_template", func() {
			BeforeEach(func() {
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/api/v2/workflow_job_templates/" && r.URL.Query().Get("name") == "my-workflow" {
						w.WriteHeader(http.StatusOK)
						json.NewEncoder(w).Encode(map[string]interface{}{
							"count": 1,
							"results": []map[string]interface{}{
								{"id": 20, "name": "my-workflow"},
							},
						})
					} else {
						w.WriteHeader(http.StatusOK)
						json.NewEncoder(w).Encode(map[string]interface{}{"count": 0, "results": []interface{}{}})
					}
				}))
				client = aap.NewClient(server.URL, "test-token")
			})

			It("should return workflow template", func() {
				template, err := client.GetTemplateByName(ctx, "my-workflow")
				Expect(err).NotTo(HaveOccurred())
				Expect(template.Type).To(Equal(aap.TemplateTypeWorkflow))
				Expect(template.Name).To(Equal("my-workflow"))
				Expect(template.ID).To(Equal(20))
			})
		})

		Context("when template does not exist", func() {
			BeforeEach(func() {
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode(map[string]interface{}{"count": 0, "results": []interface{}{}})
				}))
				client = aap.NewClient(server.URL, "test-token")
			})

			It("should return error", func() {
				_, err := client.GetTemplateByName(ctx, "nonexistent")
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("not found"))
			})
		})
	})

	Describe("GetTemplate", func() {
		Context("with caching", func() {
			var requestCount int

			BeforeEach(func() {
				requestCount = 0
				server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requestCount++
					if r.URL.Path == "/api/v2/job_templates/" && r.URL.Query().Get("name") == "cached-job" {
						w.WriteHeader(http.StatusOK)
						json.NewEncoder(w).Encode(map[string]interface{}{
							"count": 1,
							"results": []map[string]interface{}{
								{"id": 1, "name": "cached-job"},
							},
						})
					} else {
						w.WriteHeader(http.StatusOK)
						json.NewEncoder(w).Encode(map[string]interface{}{"count": 0, "results": []interface{}{}})
					}
				}))
				client = aap.NewClient(server.URL, "test-token")
			})

			It("should cache result and avoid repeated AAP queries", func() {
				// First call queries AAP and caches
				template, err := client.GetTemplate(ctx, "cached-job")
				Expect(err).NotTo(HaveOccurred())
				Expect(template.Type).To(Equal(aap.TemplateTypeJob))
				Expect(requestCount).To(Equal(1))

				// Second call uses cache without querying AAP
				template, err = client.GetTemplate(ctx, "cached-job")
				Expect(err).NotTo(HaveOccurred())
				Expect(template.Type).To(Equal(aap.TemplateTypeJob))
				Expect(requestCount).To(Equal(1)) // No additional requests
			})
		})
	})

	Describe("InvalidateTemplateCache", func() {
		var requestCount int

		BeforeEach(func() {
			requestCount = 0
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount++
				if r.URL.Path == "/api/v2/job_templates/" && r.URL.Query().Get("name") == "my-job" {
					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode(map[string]interface{}{
						"count": 1,
						"results": []map[string]interface{}{
							{"id": 1, "name": "my-job"},
						},
					})
				} else {
					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode(map[string]interface{}{"count": 0, "results": []interface{}{}})
				}
			}))
			client = aap.NewClient(server.URL, "test-token")
		})

		It("should remove template from cache", func() {
			// Populate cache
			_, err := client.GetTemplate(ctx, "my-job")
			Expect(err).NotTo(HaveOccurred())
			Expect(requestCount).To(Equal(1))

			// Invalidate cache
			client.InvalidateTemplateCache("my-job")

			// Should query AAP again
			_, err = client.GetTemplate(ctx, "my-job")
			Expect(err).NotTo(HaveOccurred())
			Expect(requestCount).To(Equal(2))
		})
	})

	Describe("ClearTemplateCache", func() {
		var requestCount int

		BeforeEach(func() {
			requestCount = 0
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestCount++
				if r.URL.Path == "/api/v2/job_templates/" && r.URL.Query().Get("name") == "job1" {
					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode(map[string]interface{}{
						"count": 1,
						"results": []map[string]interface{}{
							{"id": 1, "name": "job1"},
						},
					})
				} else if r.URL.Path == "/api/v2/workflow_job_templates/" && r.URL.Query().Get("name") == "workflow1" {
					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode(map[string]interface{}{
						"count": 1,
						"results": []map[string]interface{}{
							{"id": 2, "name": "workflow1"},
						},
					})
				} else {
					w.WriteHeader(http.StatusOK)
					json.NewEncoder(w).Encode(map[string]interface{}{"count": 0, "results": []interface{}{}})
				}
			}))
			client = aap.NewClient(server.URL, "test-token")
		})

		It("should clear all cached templates", func() {
			// Populate cache with multiple entries
			_, err := client.GetTemplate(ctx, "job1")
			Expect(err).NotTo(HaveOccurred())
			_, err = client.GetTemplate(ctx, "workflow1")
			Expect(err).NotTo(HaveOccurred())
			initialCount := requestCount

			// Clear cache
			client.ClearTemplateCache()

			// Both should query AAP again
			_, err = client.GetTemplate(ctx, "job1")
			Expect(err).NotTo(HaveOccurred())
			_, err = client.GetTemplate(ctx, "workflow1")
			Expect(err).NotTo(HaveOccurred())
			Expect(requestCount).To(Equal(initialCount * 2))
		})
	})
})
