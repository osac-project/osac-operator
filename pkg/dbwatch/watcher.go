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
	"time"

	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// TenantRecord represents a tenant row from the fulfillment database.
type TenantRecord struct {
	ID           string
	Name         string
	DisplayName  string
	EmailDomains []string
}

func (t TenantRecord) equal(other TenantRecord) bool {
	if t.ID != other.ID || t.Name != other.Name || t.DisplayName != other.DisplayName {
		return false
	}
	if len(t.EmailDomains) != len(other.EmailDomains) {
		return false
	}
	for i := range t.EmailDomains {
		if t.EmailDomains[i] != other.EmailDomains[i] {
			return false
		}
	}
	return true
}

// EventType represents the type of database change detected.
type EventType string

const (
	EventInsert EventType = "INSERT"
	EventUpdate EventType = "UPDATE"
	EventDelete EventType = "DELETE"
)

// TenantEvent represents a detected change in the organizations table.
type TenantEvent struct {
	Type   EventType
	Tenant TenantRecord
}

// Querier abstracts read-only database access for testing.
type Querier interface {
	ListTenants(ctx context.Context) ([]TenantRecord, error)
	Close() error
}

// ReconcileEnqueuer is called when the watcher detects a change.
type ReconcileEnqueuer func(tenantName string)

var (
	pollsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "osac_dbwatch_polls_total",
		Help: "Total number of database poll cycles executed.",
	})
	eventsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "osac_dbwatch_events_total",
		Help: "Total number of tenant change events detected.",
	}, []string{"event_type"})
	errorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "osac_dbwatch_errors_total",
		Help: "Total number of errors encountered during polling.",
	})
	tenantsCurrent = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "osac_dbwatch_tenants_current",
		Help: "Current number of tenants in the database snapshot.",
	})
)

func init() {
	metrics.Registry.MustRegister(pollsTotal, eventsTotal, errorsTotal, tenantsCurrent)
}

// Watcher polls the fulfillment database for tenant changes and calls
// the enqueuer when changes are detected. Implements manager.LeaderElectionRunnable.
type Watcher struct {
	querier  Querier
	interval time.Duration
	enqueuer ReconcileEnqueuer
	log      logr.Logger
	snapshot map[string]TenantRecord
}

// New creates a Watcher.
func New(querier Querier, interval time.Duration, enqueuer ReconcileEnqueuer, logger logr.Logger) *Watcher {
	return &Watcher{
		querier:  querier,
		interval: interval,
		enqueuer: enqueuer,
		log:      logger.WithName("dbwatch"),
	}
}

// Start runs the polling loop. Blocks until ctx is cancelled.
func (w *Watcher) Start(ctx context.Context) error {
	w.log.Info("starting database watcher", "interval", w.interval)
	defer func() {
		if err := w.querier.Close(); err != nil {
			w.log.Error(err, "error closing database connection")
		}
	}()

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	w.poll(ctx)

	for {
		select {
		case <-ctx.Done():
			w.log.Info("stopping database watcher")
			return nil
		case <-ticker.C:
			w.poll(ctx)
		}
	}
}

// NeedLeaderElection returns true — only the leader should poll.
func (w *Watcher) NeedLeaderElection() bool {
	return true
}

func (w *Watcher) poll(ctx context.Context) {
	pollsTotal.Inc()

	tenants, err := w.querier.ListTenants(ctx)
	if err != nil {
		errorsTotal.Inc()
		w.log.Error(err, "failed to list tenants from database")
		return
	}

	tenantsCurrent.Set(float64(len(tenants)))

	current := make(map[string]TenantRecord, len(tenants))
	for _, t := range tenants {
		current[t.ID] = t
	}

	events := w.diff(current)
	for _, evt := range events {
		eventsTotal.WithLabelValues(string(evt.Type)).Inc()
		w.log.Info("tenant change detected",
			"type", evt.Type,
			"tenantID", evt.Tenant.ID,
			"tenantName", evt.Tenant.Name,
		)
		w.enqueuer(evt.Tenant.Name)
	}

	w.snapshot = current
}

func (w *Watcher) diff(current map[string]TenantRecord) []TenantEvent {
	var events []TenantEvent

	if w.snapshot == nil {
		for _, t := range current {
			events = append(events, TenantEvent{Type: EventInsert, Tenant: t})
		}
		return events
	}

	for id, t := range current {
		prev, exists := w.snapshot[id]
		if !exists {
			events = append(events, TenantEvent{Type: EventInsert, Tenant: t})
		} else if !t.equal(prev) {
			events = append(events, TenantEvent{Type: EventUpdate, Tenant: t})
		}
	}

	for id, t := range w.snapshot {
		if _, exists := current[id]; !exists {
			events = append(events, TenantEvent{Type: EventDelete, Tenant: TenantRecord{ID: t.ID, Name: t.Name}})
		}
	}

	return events
}
