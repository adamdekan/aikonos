// broker/internal/policy/fga.go
// OpenFGA (ReBAC) Check client.
//
// CheckFGA answers "does <user> have <relation> on <object>?" against an
// OpenFGA store via its HTTP Check API (POST /stores/{store_id}/check). This is
// the relationship half of the hybrid authz model; OPA (engine.go) is the
// attribute half.
//
// Dev fallback: when no store id is configured the client is disabled and
// CheckFGA allows all relationships (the documented Phase 3/4 stub). OPA still
// enforces ABAC/approvals, so this fallback never silently widens an
// attribute-gated path — only the relationship gate is short-circuited.
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// fgaClient talks to an OpenFGA store's Check API.
type fgaClient struct {
	endpoint   string // base URL, e.g. http://openfga:8080
	storeID    string
	modelID    string // optional; pins an authorization model version
	httpClient *http.Client
	logger     *zap.Logger
}

func newFGAClient(endpoint, storeID, modelID string, logger *zap.Logger) *fgaClient {
	return &fgaClient{
		endpoint:   strings.TrimRight(endpoint, "/"),
		storeID:    strings.TrimSpace(storeID),
		modelID:    strings.TrimSpace(modelID),
		httpClient: &http.Client{Timeout: 5 * time.Second},
		logger:     logger,
	}
}

// enabled reports whether a real store is configured.
func (c *fgaClient) enabled() bool { return c != nil && c.storeID != "" }

type fgaCheckRequest struct {
	TupleKey             fgaTupleKey `json:"tuple_key"`
	AuthorizationModelID string      `json:"authorization_model_id,omitempty"`
}

type fgaTupleKey struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

type fgaCheckResponse struct {
	Allowed bool `json:"allowed"`
}

type fgaWriteRequest struct {
	Writes               fgaTupleKeys `json:"writes"`
	AuthorizationModelID string       `json:"authorization_model_id,omitempty"`
}

type fgaTupleKeys struct {
	TupleKeys []fgaTupleKey `json:"tuple_keys"`
}

// check performs an OpenFGA Check. A transport/protocol error is returned to
// the caller (which treats it as deny) — a configured ReBAC gate must fail
// closed.
func (c *fgaClient) check(ctx context.Context, user, relation, object string) (bool, error) {
	reqBody := fgaCheckRequest{
		TupleKey:             fgaTupleKey{User: user, Relation: relation, Object: object},
		AuthorizationModelID: c.modelID,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return false, fmt.Errorf("marshal fga check: %w", err)
	}

	url := fmt.Sprintf("%s/stores/%s/check", c.endpoint, c.storeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return false, fmt.Errorf("build fga request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return false, fmt.Errorf("fga request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("fga returned status %d", resp.StatusCode)
	}

	var out fgaCheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, fmt.Errorf("decode fga response: %w", err)
	}
	return out.Allowed, nil
}

// write persists relationship tuples (OpenFGA Write API). Used to record the
// relationships a freshly-created resource has (e.g. task owner/approver) so
// later Check calls resolve. Writing a tuple that already exists is treated as
// success — re-creation must be idempotent.
func (c *fgaClient) write(ctx context.Context, tuples []fgaTupleKey) error {
	body, err := json.Marshal(fgaWriteRequest{Writes: fgaTupleKeys{TupleKeys: tuples}})
	if err != nil {
		return fmt.Errorf("marshal fga write: %w", err)
	}

	url := fmt.Sprintf("%s/stores/%s/write", c.endpoint, c.storeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build fga write request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fga write request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	// OpenFGA returns 400 with code "write_failed_due_to_invalid_input" when a
	// tuple already exists; that's benign for our idempotent create path.
	if resp.StatusCode == http.StatusBadRequest {
		var e struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
			c.logger.Warn("fga: decode write response body", zap.Error(err))
		}
		if strings.Contains(e.Code, "already_exists") || strings.Contains(e.Code, "write_failed") {
			return nil
		}
		return fmt.Errorf("fga write rejected: %s", e.Code)
	}
	return fmt.Errorf("fga write returned status %d", resp.StatusCode)
}

type fgaReadRequest struct {
	TupleKey          *fgaReadTupleKey `json:"tuple_key,omitempty"`
	PageSize          int              `json:"page_size,omitempty"`
	ContinuationToken string           `json:"continuation_token,omitempty"`
}

// fgaReadTupleKey is the partial filter for the Read API. OpenFGA requires a
// fully-qualified object id OR a user — a bare type prefix (object id and user
// both empty) is rejected with a 400. An all-empty filter is therefore sent as
// no tuple_key at all, which reads every tuple in the store.
type fgaReadTupleKey struct {
	User     string `json:"user,omitempty"`
	Relation string `json:"relation,omitempty"`
	Object   string `json:"object,omitempty"`
}

type fgaReadResponse struct {
	Tuples []struct {
		Key fgaTupleKey `json:"key"`
	} `json:"tuples"`
	ContinuationToken string `json:"continuation_token"`
}

// read returns one page of tuples matching filter plus a continuation token
// ("" when exhausted). The caller drives pagination.
func (c *fgaClient) read(ctx context.Context, filter fgaReadTupleKey, pageSize int, cont string) ([]fgaTupleKey, string, error) {
	var tk *fgaReadTupleKey
	if filter != (fgaReadTupleKey{}) {
		tk = &filter
	}
	body, err := json.Marshal(fgaReadRequest{TupleKey: tk, PageSize: pageSize, ContinuationToken: cont})
	if err != nil {
		return nil, "", fmt.Errorf("marshal fga read: %w", err)
	}

	url := fmt.Sprintf("%s/stores/%s/read", c.endpoint, c.storeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("build fga read request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fga read request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("fga read returned status %d", resp.StatusCode)
	}

	var out fgaReadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", fmt.Errorf("decode fga read response: %w", err)
	}
	keys := make([]fgaTupleKey, 0, len(out.Tuples))
	for _, t := range out.Tuples {
		keys = append(keys, t.Key)
	}
	return keys, out.ContinuationToken, nil
}

type fgaListObjectsRequest struct {
	Type                 string `json:"type"`
	Relation             string `json:"relation"`
	User                 string `json:"user"`
	AuthorizationModelID string `json:"authorization_model_id,omitempty"`
}

type fgaListObjectsResponse struct {
	Objects []string `json:"objects"`
}

// listObjects calls the OpenFGA ListObjects API (POST /stores/<id>/list-objects)
// and returns the full object refs like ["agent:abc", ...].
func (c *fgaClient) listObjects(ctx context.Context, user, relation, objectType string) ([]string, error) {
	reqBody := fgaListObjectsRequest{
		Type:                 objectType,
		Relation:             relation,
		User:                 user,
		AuthorizationModelID: c.modelID,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal fga list-objects: %w", err)
	}

	url := fmt.Sprintf("%s/stores/%s/list-objects", c.endpoint, c.storeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build fga list-objects request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fga list-objects request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fga list-objects returned status %d", resp.StatusCode)
	}

	var out fgaListObjectsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode fga list-objects response: %w", err)
	}
	return out.Objects, nil
}

type fgaDeleteRequest struct {
	Deletes              fgaTupleKeys `json:"deletes"`
	AuthorizationModelID string       `json:"authorization_model_id,omitempty"`
}

// delete removes relationship tuples (OpenFGA Write API, deletes field).
// Deleting a tuple that doesn't exist is treated as success — revoke must be
// idempotent.
func (c *fgaClient) delete(ctx context.Context, tuples []fgaTupleKey) error {
	body, err := json.Marshal(fgaDeleteRequest{Deletes: fgaTupleKeys{TupleKeys: tuples}})
	if err != nil {
		return fmt.Errorf("marshal fga delete: %w", err)
	}

	url := fmt.Sprintf("%s/stores/%s/write", c.endpoint, c.storeID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build fga delete request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("fga delete request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	// OpenFGA returns 400 when the tuple is absent; benign for idempotent revoke.
	if resp.StatusCode == http.StatusBadRequest {
		var e struct {
			Code string `json:"code"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
			c.logger.Warn("fga: decode delete response body", zap.Error(err))
		}
		if strings.Contains(e.Code, "not_found") || strings.Contains(e.Code, "write_failed") || strings.Contains(e.Code, "invalid_input") {
			return nil
		}
		return fmt.Errorf("fga delete rejected: %s", e.Code)
	}
	return fmt.Errorf("fga delete returned status %d", resp.StatusCode)
}
