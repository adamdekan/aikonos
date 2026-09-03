// broker/internal/db/mcp_connections.go
// Repository for the MCP connection registry. Every query sets
// app.current_tenant for RLS. No raw SQL outside this package.
package db

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"
)

type McpConnection struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	Name          string
	URL           string
	Transport     string
	AuthType      string
	AuthSecretRef string // Vault path; empty until broker layer writes it
	CreatedBy     string
	CreatedAt     time.Time
}

type McpConnectionRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewMcpConnectionRepo(pool *pgxpool.Pool, logger *zap.Logger) *McpConnectionRepo {
	return &McpConnectionRepo{pool: pool, logger: logger}
}

const mcpConnectionCols = `id, tenant_id, name, url, transport, auth_type, auth_secret_ref, created_by, created_at`

func scanMcpConnection(row interface {
	Scan(dest ...any) error
}) (*McpConnection, error) {
	var c McpConnection
	if err := row.Scan(&c.ID, &c.TenantID, &c.Name, &c.URL, &c.Transport,
		&c.AuthType, &c.AuthSecretRef, &c.CreatedBy, &c.CreatedAt); err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *McpConnectionRepo) List(ctx context.Context, tenantID string) ([]*McpConnection, error) {
	return withConn(ctx, r.pool, tenantID, func(conn *pgxpool.Conn) ([]*McpConnection, error) {
		rows, err := conn.Query(ctx, `SELECT `+mcpConnectionCols+` FROM mcp_connections ORDER BY name`)
		if err != nil {
			return nil, fmt.Errorf("list mcp_connections: %w", err)
		}
		defer rows.Close()
		var out []*McpConnection
		for rows.Next() {
			mc, err := scanMcpConnection(rows)
			if err != nil {
				return nil, err
			}
			out = append(out, mc)
		}
		return out, rows.Err()
	})
}

func (r *McpConnectionRepo) Add(ctx context.Context, conn *McpConnection) error {
	return withConnErr(ctx, r.pool, conn.TenantID.String(), func(c *pgxpool.Conn) error {
		_, err := c.Exec(ctx, `
		INSERT INTO mcp_connections (id, tenant_id, name, url, transport, auth_type, auth_secret_ref, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			conn.ID, conn.TenantID, conn.Name, conn.URL, conn.Transport,
			conn.AuthType, conn.AuthSecretRef, conn.CreatedBy,
		)
		if err != nil {
			return fmt.Errorf("insert mcp_connection: %w", err)
		}
		return nil
	})
}

func (r *McpConnectionRepo) Update(ctx context.Context, tenantID string, conn *McpConnection) error {
	return withConnErr(ctx, r.pool, tenantID, func(c *pgxpool.Conn) error {
		tag, err := c.Exec(ctx, `
		UPDATE mcp_connections
		SET name = $1, url = $2, transport = $3, auth_type = $4, auth_secret_ref = $5
		WHERE id = $6`,
			conn.Name, conn.URL, conn.Transport, conn.AuthType, conn.AuthSecretRef, conn.ID,
		)
		if err != nil {
			return fmt.Errorf("update mcp_connection: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("mcp_connection %s not found", conn.ID)
		}
		return nil
	})
}

func (r *McpConnectionRepo) Get(ctx context.Context, tenantID, id string) (*McpConnection, error) {
	return withConn(ctx, r.pool, tenantID, func(c *pgxpool.Conn) (*McpConnection, error) {
		row := c.QueryRow(ctx, `SELECT `+mcpConnectionCols+` FROM mcp_connections WHERE id = $1`, id)
		mc, err := scanMcpConnection(row)
		if err != nil {
			return nil, fmt.Errorf("get mcp_connection %s: %w", id, err)
		}
		return mc, nil
	})
}

func (r *McpConnectionRepo) Delete(ctx context.Context, tenantID, id string) error {
	return withConnErr(ctx, r.pool, tenantID, func(c *pgxpool.Conn) error {
		tag, err := c.Exec(ctx, `DELETE FROM mcp_connections WHERE id = $1`, id)
		if err != nil {
			return fmt.Errorf("delete mcp_connection: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("mcp_connection %s not found", id)
		}
		return nil
	})
}
