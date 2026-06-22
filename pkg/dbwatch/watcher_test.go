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

package dbwatch

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDBWatch(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DBWatch Suite")
}

type mockQuerier struct {
	mu       sync.Mutex
	tenants  []TenantRecord
	err      error
	calls    int
	closeCh  chan struct{}
	closed   bool
	tenantFn func(call int) ([]TenantRecord, error)
}

func (m *mockQuerier) ListTenants(_ context.Context) ([]TenantRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if m.tenantFn != nil {
		return m.tenantFn(m.calls)
	}
	return m.tenants, m.err
}

func (m *mockQuerier) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	if m.closeCh != nil {
		close(m.closeCh)
	}
	return nil
}

func (m *mockQuerier) getCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func (m *mockQuerier) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

type enqueueCollector struct {
	mu    sync.Mutex
	names []string
}

func (c *enqueueCollector) enqueue(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.names = append(c.names, name)
}

func (c *enqueueCollector) getNames() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]string, len(c.names))
	copy(result, c.names)
	return result
}

func newTestLogger() logr.Logger {
	return logr.Discard()
}

var _ = Describe("Watcher", func() {
	Describe("diff logic", func() {
		var (
			w         *Watcher
			collector *enqueueCollector
		)

		BeforeEach(func() {
			collector = &enqueueCollector{}
			w = &Watcher{
				enqueuer: collector.enqueue,
				log:      newTestLogger(),
			}
		})

		It("treats all records as INSERT on first poll", func() {
			current := map[string]TenantRecord{
				"id-1": {ID: "id-1", Name: "tenant-a"},
				"id-2": {ID: "id-2", Name: "tenant-b"},
			}

			events := w.diff(current)
			Expect(events).To(HaveLen(2))
			for _, e := range events {
				Expect(e.Type).To(Equal(EventInsert))
			}
		})

		It("detects INSERT when a new record appears", func() {
			w.snapshot = map[string]TenantRecord{
				"id-1": {ID: "id-1", Name: "tenant-a"},
			}

			current := map[string]TenantRecord{
				"id-1": {ID: "id-1", Name: "tenant-a"},
				"id-2": {ID: "id-2", Name: "tenant-b"},
			}

			events := w.diff(current)
			Expect(events).To(HaveLen(1))
			Expect(events[0].Type).To(Equal(EventInsert))
			Expect(events[0].Tenant.Name).To(Equal("tenant-b"))
		})

		It("detects UPDATE when a record's fields change", func() {
			w.snapshot = map[string]TenantRecord{
				"id-1": {ID: "id-1", Name: "tenant-a", DisplayName: "Old Name"},
			}

			current := map[string]TenantRecord{
				"id-1": {ID: "id-1", Name: "tenant-a", DisplayName: "New Name"},
			}

			events := w.diff(current)
			Expect(events).To(HaveLen(1))
			Expect(events[0].Type).To(Equal(EventUpdate))
			Expect(events[0].Tenant.DisplayName).To(Equal("New Name"))
		})

		It("detects UPDATE when email domains change", func() {
			w.snapshot = map[string]TenantRecord{
				"id-1": {ID: "id-1", Name: "tenant-a", EmailDomains: []string{"a.com"}},
			}

			current := map[string]TenantRecord{
				"id-1": {ID: "id-1", Name: "tenant-a", EmailDomains: []string{"a.com", "b.com"}},
			}

			events := w.diff(current)
			Expect(events).To(HaveLen(1))
			Expect(events[0].Type).To(Equal(EventUpdate))
		})

		It("detects DELETE when a record disappears", func() {
			w.snapshot = map[string]TenantRecord{
				"id-1": {ID: "id-1", Name: "tenant-a"},
				"id-2": {ID: "id-2", Name: "tenant-b"},
			}

			current := map[string]TenantRecord{
				"id-1": {ID: "id-1", Name: "tenant-a"},
			}

			events := w.diff(current)
			Expect(events).To(HaveLen(1))
			Expect(events[0].Type).To(Equal(EventDelete))
			Expect(events[0].Tenant.ID).To(Equal("id-2"))
			Expect(events[0].Tenant.Name).To(Equal("tenant-b"))
		})

		It("returns no events when nothing changes", func() {
			w.snapshot = map[string]TenantRecord{
				"id-1": {ID: "id-1", Name: "tenant-a"},
			}

			current := map[string]TenantRecord{
				"id-1": {ID: "id-1", Name: "tenant-a"},
			}

			events := w.diff(current)
			Expect(events).To(BeEmpty())
		})

		It("returns no events when both snapshots are empty", func() {
			w.snapshot = map[string]TenantRecord{}

			current := map[string]TenantRecord{}

			events := w.diff(current)
			Expect(events).To(BeEmpty())
		})

		It("detects DELETE for all records when database is emptied", func() {
			w.snapshot = map[string]TenantRecord{
				"id-1": {ID: "id-1", Name: "tenant-a"},
				"id-2": {ID: "id-2", Name: "tenant-b"},
			}

			current := map[string]TenantRecord{}

			events := w.diff(current)
			Expect(events).To(HaveLen(2))
			for _, e := range events {
				Expect(e.Type).To(Equal(EventDelete))
			}
		})

		It("detects mixed INSERT, UPDATE, and DELETE in a single poll", func() {
			w.snapshot = map[string]TenantRecord{
				"id-1": {ID: "id-1", Name: "tenant-a", DisplayName: "A"},
				"id-2": {ID: "id-2", Name: "tenant-b"},
			}

			current := map[string]TenantRecord{
				"id-1": {ID: "id-1", Name: "tenant-a", DisplayName: "A Updated"},
				"id-3": {ID: "id-3", Name: "tenant-c"},
			}

			events := w.diff(current)
			Expect(events).To(HaveLen(3))

			types := map[EventType]int{}
			for _, e := range events {
				types[e.Type]++
			}
			Expect(types[EventInsert]).To(Equal(1))
			Expect(types[EventUpdate]).To(Equal(1))
			Expect(types[EventDelete]).To(Equal(1))
		})
	})

	Describe("poll", func() {
		It("calls enqueuer for each detected change", func() {
			collector := &enqueueCollector{}
			querier := &mockQuerier{
				tenants: []TenantRecord{
					{ID: "id-1", Name: "tenant-a"},
					{ID: "id-2", Name: "tenant-b"},
				},
			}

			w := &Watcher{
				querier:  querier,
				enqueuer: collector.enqueue,
				log:      newTestLogger(),
			}

			w.poll(context.Background())

			names := collector.getNames()
			Expect(names).To(HaveLen(2))
			Expect(names).To(ContainElements("tenant-a", "tenant-b"))
		})

		It("does not call enqueuer when querier returns an error", func() {
			collector := &enqueueCollector{}
			querier := &mockQuerier{
				err: fmt.Errorf("connection refused"),
			}

			w := &Watcher{
				querier:  querier,
				enqueuer: collector.enqueue,
				log:      newTestLogger(),
			}

			w.poll(context.Background())

			Expect(collector.getNames()).To(BeEmpty())
			Expect(w.snapshot).To(BeNil())
		})

		It("recovers after querier error on subsequent poll", func() {
			collector := &enqueueCollector{}
			callCount := 0
			querier := &mockQuerier{
				tenantFn: func(call int) ([]TenantRecord, error) {
					callCount++
					if callCount == 1 {
						return nil, fmt.Errorf("connection refused")
					}
					return []TenantRecord{
						{ID: "id-1", Name: "tenant-a"},
					}, nil
				},
			}

			w := &Watcher{
				querier:  querier,
				enqueuer: collector.enqueue,
				log:      newTestLogger(),
			}

			w.poll(context.Background())
			Expect(collector.getNames()).To(BeEmpty())

			w.poll(context.Background())
			Expect(collector.getNames()).To(ContainElement("tenant-a"))
		})
	})

	Describe("Start", func() {
		It("stops cleanly when context is cancelled", func() {
			querier := &mockQuerier{
				closeCh: make(chan struct{}),
			}
			collector := &enqueueCollector{}

			w := New(querier, 50*time.Millisecond, collector.enqueue, newTestLogger())

			ctx, cancel := context.WithCancel(context.Background())

			done := make(chan error, 1)
			go func() {
				done <- w.Start(ctx)
			}()

			Eventually(func() int {
				return querier.getCalls()
			}, 500*time.Millisecond, 10*time.Millisecond).Should(BeNumerically(">=", 1))

			cancel()

			var err error
			Eventually(done, time.Second).Should(Receive(&err))
			Expect(err).NotTo(HaveOccurred())
			Eventually(querier.isClosed, time.Second).Should(BeTrue())
		})

		It("polls at the configured interval", func() {
			querier := &mockQuerier{
				tenants: []TenantRecord{
					{ID: "id-1", Name: "tenant-a"},
				},
			}
			collector := &enqueueCollector{}

			w := New(querier, 50*time.Millisecond, collector.enqueue, newTestLogger())

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			go w.Start(ctx) //nolint:errcheck

			Eventually(func() int {
				return querier.getCalls()
			}, time.Second, 10*time.Millisecond).Should(BeNumerically(">=", 3))
		})
	})

	Describe("NeedLeaderElection", func() {
		It("returns true", func() {
			w := &Watcher{}
			Expect(w.NeedLeaderElection()).To(BeTrue())
		})
	})

	Describe("TenantLookup", func() {
		Describe("Ready", func() {
			It("returns false before first poll", func() {
				w := &Watcher{
					log: newTestLogger(),
				}
				Expect(w.Ready()).To(BeFalse())
			})

			It("returns true after first successful poll", func() {
				collector := &enqueueCollector{}
				querier := &mockQuerier{
					tenants: []TenantRecord{
						{ID: "id-1", Name: "tenant-a"},
					},
				}
				w := &Watcher{
					querier:  querier,
					enqueuer: collector.enqueue,
					log:      newTestLogger(),
				}

				w.poll(context.Background())
				Expect(w.Ready()).To(BeTrue())
			})

			It("remains false after a failed poll", func() {
				collector := &enqueueCollector{}
				querier := &mockQuerier{
					err: fmt.Errorf("connection refused"),
				}
				w := &Watcher{
					querier:  querier,
					enqueuer: collector.enqueue,
					log:      newTestLogger(),
				}

				w.poll(context.Background())
				Expect(w.Ready()).To(BeFalse())
			})
		})

		Describe("GetTenantByName", func() {
			It("returns the record when tenant exists in snapshot", func() {
				w := &Watcher{log: newTestLogger()}
				w.snapshot = map[string]TenantRecord{
					"id-1": {ID: "id-1", Name: "tenant-a", DisplayName: "Tenant A", EmailDomains: []string{"a.com"}},
				}
				w.ready = true

				record, found := w.GetTenantByName("tenant-a")
				Expect(found).To(BeTrue())
				Expect(record.ID).To(Equal("id-1"))
				Expect(record.DisplayName).To(Equal("Tenant A"))
				Expect(record.EmailDomains).To(Equal([]string{"a.com"}))
			})

			It("returns false when tenant is not in snapshot", func() {
				w := &Watcher{log: newTestLogger()}
				w.snapshot = map[string]TenantRecord{
					"id-1": {ID: "id-1", Name: "tenant-a"},
				}
				w.ready = true

				_, found := w.GetTenantByName("tenant-b")
				Expect(found).To(BeFalse())
			})

			It("returns false when snapshot is nil", func() {
				w := &Watcher{log: newTestLogger()}

				_, found := w.GetTenantByName("tenant-a")
				Expect(found).To(BeFalse())
			})

			It("does not panic under concurrent access with poll", func() {
				collector := &enqueueCollector{}
				callCount := 0
				querier := &mockQuerier{
					tenantFn: func(_ int) ([]TenantRecord, error) {
						callCount++
						return []TenantRecord{
							{ID: "id-1", Name: fmt.Sprintf("tenant-%d", callCount)},
						}, nil
					},
				}
				w := &Watcher{
					querier:  querier,
					enqueuer: collector.enqueue,
					log:      newTestLogger(),
				}

				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()

				// Poll concurrently
				go func() {
					for ctx.Err() == nil {
						w.poll(ctx)
					}
				}()

				// Read concurrently — should not panic
				Consistently(func() bool {
					w.GetTenantByName("tenant-1")
					w.Ready()
					return true
				}, 200*time.Millisecond, 5*time.Millisecond).Should(BeTrue())
			})
		})
	})

	Describe("TenantRecord.equal", func() {
		It("returns true for identical records", func() {
			a := TenantRecord{ID: "1", Name: "a", DisplayName: "A", EmailDomains: []string{"a.com"}}
			b := TenantRecord{ID: "1", Name: "a", DisplayName: "A", EmailDomains: []string{"a.com"}}
			Expect(a.equal(b)).To(BeTrue())
		})

		It("returns true for both nil and empty email domains", func() {
			a := TenantRecord{ID: "1", Name: "a"}
			b := TenantRecord{ID: "1", Name: "a"}
			Expect(a.equal(b)).To(BeTrue())
		})

		It("returns false when names differ", func() {
			a := TenantRecord{ID: "1", Name: "a"}
			b := TenantRecord{ID: "1", Name: "b"}
			Expect(a.equal(b)).To(BeFalse())
		})

		It("returns false when email domains differ", func() {
			a := TenantRecord{ID: "1", Name: "a", EmailDomains: []string{"a.com"}}
			b := TenantRecord{ID: "1", Name: "a", EmailDomains: []string{"b.com"}}
			Expect(a.equal(b)).To(BeFalse())
		})
	})
})
