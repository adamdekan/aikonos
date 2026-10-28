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
	// AG-09, auth_type=oauth only: the client and the endpoints discovery
	// found when the admin saved the connection. The secret is in Vault.
	OAuthClientID string
	OAuthScopes   []string
	OAuthIssuer   string
	OAuthAuthURL  string
	OAuthTokenURL string
	OAuthResource string
}

type McpConnectionRepo struct {
	pool   *pgxpool.Pool
	logger *zap.Logger
}

func NewMcpConnectionRepo(pool *pgxpool.Pool, logger *zap.Logger) *McpConnectionRepo {
	return &McpConnectionRepo{pool: pool, logger: logger}
}

const mcpConnectionCols = `id, tenant_id, name, url, transport, auth_type, auth_secret_ref, created_by, created_at,
	oauth_client_id, oauth_scopes, oauth_issuer, oauth_auth_url, oauth_token_url, oauth_resource`

func scanMcpConnection(row interface {
	Scan(dest ...any) error
}) (*McpConnection, error) {
	var c McpConnection
	if err := row.Scan(&c.ID, &c.TenantID, &c.Name, &c.URL, &c.Transport,
		&c.AuthType, &c.AuthSecretRef, &c.CreatedBy, &c.CreatedAt,
		&c.OAuthClientID, &c.OAuthScopes, &c.OAuthIssuer, &c.OAuthAuthURL, &c.OAuthTokenURL, &c.OAuthResource); err != nil {
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
		INSERT INTO mcp_connections (id, tenant_id, name, url, transport, auth_type, auth_secret_ref, created_by,
			oauth_client_id, oauth_scopes, oauth_issuer, oauth_auth_url, oauth_token_url, oauth_resource)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)`,
			conn.ID, conn.TenantID, conn.Name, conn.URL, conn.Transport,
			conn.AuthType, conn.AuthSecretRef, conn.CreatedBy,
			conn.OAuthClientID, nonNilStrings(conn.OAuthScopes), conn.OAuthIssuer, conn.OAuthAuthURL, conn.OAuthTokenURL, conn.OAuthResource,
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
		SET name = $1, url = $2, transport = $3, auth_type = $4, auth_secret_ref = $5,
			oauth_client_id = $7, oauth_scopes = $8, oauth_issuer = $9, oauth_auth_url = $10,
			oauth_token_url = $11, oauth_resource = $12
		WHERE id = $6`,
			conn.Name, conn.URL, conn.Transport, conn.AuthType, conn.AuthSecretRef, conn.ID,
			conn.OAuthClientID, nonNilStrings(conn.OAuthScopes), conn.OAuthIssuer, conn.OAuthAuthURL, conn.OAuthTokenURL, conn.OAuthResource,
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

// nonNilStrings keeps a nil slice off a NOT NULL TEXT[] column.
func nonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
