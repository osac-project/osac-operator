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
	"encoding/json"
	"fmt"

	"github.com/go-logr/logr"
	"github.com/jackc/pgx/v5"
)

// PgQuerier implements Querier using pgx to connect to PostgreSQL.
type PgQuerier struct {
	connString string
	conn       *pgx.Conn
	log        logr.Logger
}

// NewPgQuerier creates a PgQuerier that connects to the given PostgreSQL URL.
// Connection is established lazily on the first ListTenants call.
func NewPgQuerier(connString string, logger logr.Logger) *PgQuerier {
	return &PgQuerier{
		connString: connString,
		log:        logger.WithName("pgquerier"),
	}
}

func (q *PgQuerier) connect(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, q.connString)
	if err != nil {
		return fmt.Errorf("connect to PostgreSQL: %w", err)
	}
	q.conn = conn
	q.log.Info("connected to PostgreSQL")
	return nil
}

// ListTenants queries the organizations table and returns all tenant records.
func (q *PgQuerier) ListTenants(ctx context.Context) ([]TenantRecord, error) {
	if q.conn == nil || q.conn.IsClosed() {
		if err := q.connect(ctx); err != nil {
			return nil, err
		}
	}

	rows, err := q.conn.Query(ctx, "SELECT id, name, data FROM organizations")
	if err != nil {
		_ = q.closeConn()
		return nil, fmt.Errorf("query organizations: %w", err)
	}
	defer rows.Close()

	var tenants []TenantRecord
	for rows.Next() {
		var id, name string
		var data []byte

		if err := rows.Scan(&id, &name, &data); err != nil {
			return nil, fmt.Errorf("scan organization row: %w", err)
		}

		record := TenantRecord{
			ID:   id,
			Name: name,
		}

		if len(data) > 0 {
			record.DisplayName, record.EmailDomains = parseOrganizationData(data)
		}

		tenants = append(tenants, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate organization rows: %w", err)
	}

	return tenants, nil
}

// organizationData represents fields extracted from the JSONB data column.
type organizationData struct {
	DisplayName  string   `json:"displayName"`
	EmailDomains []string `json:"emailDomains"`
}

func parseOrganizationData(data []byte) (displayName string, emailDomains []string) {
	var parsed organizationData
	if err := json.Unmarshal(data, &parsed); err != nil {
		return "", nil
	}
	return parsed.DisplayName, parsed.EmailDomains
}

// Close closes the underlying database connection.
func (q *PgQuerier) Close() error {
	return q.closeConn()
}

func (q *PgQuerier) closeConn() error {
	if q.conn != nil && !q.conn.IsClosed() {
		err := q.conn.Close(context.Background())
		q.conn = nil
		return err
	}
	q.conn = nil
	return nil
}
