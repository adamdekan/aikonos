// broker/internal/scim/scim.go
// AG-16: a minimal SCIM 2.0 service provider (RFC 7643/7644) for Users and
// Groups. The gateway forwards each /scim/v2 request over the south RPC
// ScimCall after the broker has resolved the bearer token to a tenant; Serve
// turns it into a status and a SCIM JSON body.
//
// The directory is authoritative for two things only: whether a user may act
// (Users.active=false sets the AG-06 user block) and group membership (written
// as FGA `user:<subject> member group:<fga_group>` tuples). A user's subject is
// its externalId, which must equal the OIDC subject claim (Entra: objectId).
// Every change is audited with the token as actor.
package scim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/db"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	"github.com/adamdekan/aikonos/broker/internal/provisioning"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

const (
	schemaUser  = "urn:ietf:params:scim:schemas:core:2.0:User"
	schemaGroup = "urn:ietf:params:scim:schemas:core:2.0:Group"
	schemaList  = "urn:ietf:params:scim:api:messages:2.0:ListResponse"
	schemaError = "urn:ietf:params:scim:api:messages:2.0:Error"

	// BlockedBy marks AG-06 blocks set by SCIM. Reactivation lifts only these,
	// never a block an admin set by hand.
	BlockedBy = db.ScimBlockedBy
)

// Store is the persisted SCIM state; *db.ScimRepo satisfies it.
type Store interface {
	ListUsers(ctx context.Context, tenantID string) ([]db.ScimUser, error)
	GetUser(ctx context.Context, tenantID string, id uuid.UUID) (db.ScimUser, error)
	CreateUser(ctx context.Context, tenantID string, u *db.ScimUser) error
	UpdateUser(ctx context.Context, tenantID string, u *db.ScimUser) error
	DeleteUser(ctx context.Context, tenantID string, id uuid.UUID) error
	ListGroups(ctx context.Context, tenantID string) ([]db.ScimGroup, error)
	GetGroup(ctx context.Context, tenantID string, id uuid.UUID) (db.ScimGroup, error)
	CreateGroup(ctx context.Context, tenantID string, g *db.ScimGroup) error
	UpdateGroup(ctx context.Context, tenantID string, g *db.ScimGroup) error
	DeleteGroup(ctx context.Context, tenantID string, id uuid.UUID) error
}

// Tuples writes FGA group membership; *policy.Engine satisfies it.
type Tuples interface {
	WriteRelations(ctx context.Context, rels ...policy.Relation) error
	DeleteRelations(ctx context.Context, rels ...policy.Relation) error
	ReadTuples(ctx context.Context, f policy.ReadFilter) ([]policy.Relation, error)
}

// groupSubjectTypes are the FGA types with a relation that accepts
// group#member (policies/fga/model.fga; TestGroupSubjectTypesMatchModel keeps
// the two in step).
var groupSubjectTypes = []string{
	"user", "group", "tenant", "workspace", "skill", "mcp_connector", "task",
	"agent", "document", "artifact", "agentskill", "skill_submission",
}

// Blocks is the AG-06 user block; *revocation.Checker satisfies it.
type Blocks interface {
	BlockUser(ctx context.Context, tenantID, subject, reason, by string) (bool, error)
	UnblockUser(ctx context.Context, tenantID, subject, by string) (bool, error)
	UserBlocks(ctx context.Context, tenantID string) (map[string]db.UserBlock, error)
}

// Emitter is the audit sink.
type Emitter interface {
	Emit(ctx context.Context, event *auditv1.AuditEvent) error
}

// Handler serves SCIM requests for one broker.
type Handler struct {
	Store  Store
	Tuples Tuples
	Blocks Blocks
	Audit  Emitter
	Logger *zap.Logger
}

// Request is one SCIM call. Path is relative to the SCIM base URL
// ("/Users/<id>"); Actor names the token ("scim-token:<id>").
type Request struct {
	Tenant, Actor string
	Method        string
	Path          string
	Query         string
	Body          []byte
}

// Error is a SCIM error response (RFC 7644 §3.12).
type Error struct {
	Status   int
	ScimType string
	Detail   string
}

func (e *Error) Error() string { return e.Detail }

func badRequest(scimType, format string, a ...any) *Error {
	return &Error{Status: http.StatusBadRequest, ScimType: scimType, Detail: fmt.Sprintf(format, a...)}
}

// ErrorBody renders a SCIM error body.
func ErrorBody(status int, scimType, detail string) []byte {
	m := map[string]any{"schemas": []string{schemaError}, "status": strconv.Itoa(status), "detail": detail}
	if scimType != "" {
		m["scimType"] = scimType
	}
	b, _ := json.Marshal(m)
	return b
}

// Serve dispatches req and returns the HTTP status and SCIM JSON body
// (nil for 204).
func (h *Handler) Serve(ctx context.Context, req Request) (int, []byte) {
	status, body, err := h.route(ctx, req)
	if err != nil {
		var se *Error
		if !errors.As(err, &se) {
			h.Logger.Warn("scim: internal error", zap.String("method", req.Method), zap.String("path", req.Path), zap.Error(err))
			se = &Error{Status: http.StatusInternalServerError, Detail: "internal error"}
		}
		return se.Status, ErrorBody(se.Status, se.ScimType, se.Detail)
	}
	if body == nil {
		return status, nil
	}
	b, err := json.Marshal(body)
	if err != nil {
		return http.StatusInternalServerError, ErrorBody(500, "", "internal error")
	}
	return status, b
}

func (h *Handler) route(ctx context.Context, req Request) (int, any, error) {
	parts := strings.Split(strings.Trim(req.Path, "/"), "/")
	if len(parts) == 0 || len(parts) > 2 {
		return 0, nil, &Error{Status: http.StatusNotFound, Detail: "unknown endpoint"}
	}
	q, err := url.ParseQuery(req.Query)
	if err != nil {
		return 0, nil, badRequest("invalidSyntax", "bad query string")
	}
	var id uuid.UUID
	if len(parts) == 2 {
		if id, err = uuid.Parse(parts[1]); err != nil {
			return 0, nil, &Error{Status: http.StatusNotFound, Detail: "resource not found"}
		}
	}
	m, coll := strings.ToUpper(req.Method), parts[0]
	switch {
	case coll == "Users" && len(parts) == 1 && m == http.MethodGet:
		return h.listUsers(ctx, req.Tenant, q)
	case coll == "Users" && len(parts) == 1 && m == http.MethodPost:
		return h.createUser(ctx, req, req.Body)
	case coll == "Users" && m == http.MethodGet:
		u, err := h.getUser(ctx, req.Tenant, id)
		return http.StatusOK, userJSON(u), err
	case coll == "Users" && m == http.MethodPut:
		return h.putUser(ctx, req, id)
	case coll == "Users" && m == http.MethodPatch:
		return h.patchUser(ctx, req, id)
	case coll == "Users" && m == http.MethodDelete:
		return h.deleteUser(ctx, req, id)
	case coll == "Groups" && len(parts) == 1 && m == http.MethodGet:
		return h.listGroups(ctx, req.Tenant, q)
	case coll == "Groups" && len(parts) == 1 && m == http.MethodPost:
		return h.createGroup(ctx, req)
	case coll == "Groups" && m == http.MethodGet:
		g, err := h.getGroup(ctx, req.Tenant, id)
		return http.StatusOK, groupJSON(g, excludesMembers(q)), err
	case coll == "Groups" && m == http.MethodPut:
		return h.putGroup(ctx, req, id)
	case coll == "Groups" && m == http.MethodPatch:
		return h.patchGroup(ctx, req, id)
	case coll == "Groups" && m == http.MethodDelete:
		return h.deleteGroup(ctx, req, id)
	case coll == "Users" || coll == "Groups":
		return 0, nil, &Error{Status: http.StatusMethodNotAllowed, Detail: "method not allowed"}
	}
	return 0, nil, &Error{Status: http.StatusNotFound, Detail: "unknown endpoint"}
}

// ── representation ───────────────────────────────────────────────────────────

func meta(kind string, created, modified time.Time) map[string]any {
	return map[string]any{
		"resourceType": kind,
		"created":      created.UTC().Format(time.RFC3339),
		"lastModified": modified.UTC().Format(time.RFC3339),
	}
}

func userJSON(u db.ScimUser) map[string]any {
	emails := u.Emails
	if emails == nil {
		emails = []db.ScimEmail{}
	}
	m := map[string]any{
		"schemas":    []string{schemaUser},
		"id":         u.ID.String(),
		"externalId": u.ExternalID,
		"userName":   u.UserName,
		"emails":     emails,
		"active":     u.Active,
		"meta":       meta("User", u.CreatedAt, u.UpdatedAt),
	}
	if u.DisplayName != "" {
		m["displayName"] = u.DisplayName
	}
	return m
}

func groupJSON(g db.ScimGroup, noMembers bool) map[string]any {
	m := map[string]any{
		"schemas":     []string{schemaGroup},
		"id":          g.ID.String(),
		"displayName": g.DisplayName,
		"meta":        meta("Group", g.CreatedAt, g.UpdatedAt),
	}
	if g.ExternalID != "" {
		m["externalId"] = g.ExternalID
	}
	if !noMembers {
		members := make([]map[string]string, 0, len(g.Members))
		for _, id := range g.Members {
			members = append(members, map[string]string{"value": id.String()})
		}
		m["members"] = members
	}
	return m
}

func excludesMembers(q url.Values) bool {
	return strings.Contains(strings.ToLower(q.Get("excludedAttributes")), "members")
}

func listResponse(q url.Values, items []any) (int, any, error) {
	start, _ := strconv.Atoi(q.Get("startIndex"))
	if start < 1 {
		start = 1
	}
	count := len(items)
	if c, err := strconv.Atoi(q.Get("count")); err == nil && c >= 0 && c < count {
		count = c
	}
	page := []any{}
	if start-1 < len(items) {
		page = items[start-1:]
		if count < len(page) {
			page = page[:count]
		}
	}
	return http.StatusOK, map[string]any{
		"schemas":      []string{schemaList},
		"totalResults": len(items),
		"startIndex":   start,
		"itemsPerPage": len(page),
		"Resources":    page,
	}, nil
}

// filterRe accepts the one filter shape provisioning clients use for lookups:
// `<attr> eq "<value>"`.
var filterRe = regexp.MustCompile(`(?i)^\s*([A-Za-z.]+)\s+eq\s+("(?:[^"\\]|\\.)*")\s*$`)

func parseFilter(f string, allowed ...string) (attr, value string, err error) {
	if f == "" {
		return "", "", nil
	}
	m := filterRe.FindStringSubmatch(f)
	if m == nil {
		return "", "", badRequest("invalidFilter", "only `<attribute> eq \"<value>\"` filters are supported")
	}
	value, uerr := strconv.Unquote(m[2])
	if uerr != nil {
		return "", "", badRequest("invalidFilter", "bad filter value")
	}
	for _, a := range allowed {
		if strings.EqualFold(m[1], a) {
			return a, value, nil
		}
	}
	return "", "", badRequest("invalidFilter", "filtering on %q is not supported", m[1])
}

// ── users ────────────────────────────────────────────────────────────────────

func (h *Handler) listUsers(ctx context.Context, tenant string, q url.Values) (int, any, error) {
	attr, val, err := parseFilter(q.Get("filter"), "userName", "externalId", "id")
	if err != nil {
		return 0, nil, err
	}
	users, err := h.Store.ListUsers(ctx, tenant)
	if err != nil {
		return 0, nil, err
	}
	items := []any{}
	for _, u := range users {
		if attr == "userName" && !strings.EqualFold(u.UserName, val) ||
			attr == "externalId" && canonicalSubject(u.ExternalID) != canonicalSubject(val) ||
			attr == "id" && u.ID.String() != val {
			continue
		}
		items = append(items, userJSON(u))
	}
	return listResponse(q, items)
}

func (h *Handler) getUser(ctx context.Context, tenant string, id uuid.UUID) (db.ScimUser, error) {
	u, err := h.Store.GetUser(ctx, tenant, id)
	if errors.Is(err, db.ErrNotFound) {
		return u, &Error{Status: http.StatusNotFound, Detail: "user not found"}
	}
	return u, err
}

// userBody is the writable part of a User resource.
type userBody struct {
	UserName    string         `json:"userName"`
	ExternalID  string         `json:"externalId"`
	DisplayName string         `json:"displayName"`
	Emails      []db.ScimEmail `json:"emails"`
	Active      any            `json:"active"`
}

func decodeUser(body []byte) (userBody, *bool, error) {
	var b userBody
	if err := json.Unmarshal(body, &b); err != nil {
		return b, nil, badRequest("invalidSyntax", "body is not a SCIM User")
	}
	if b.Active == nil {
		return b, nil, nil
	}
	a, ok := parseBool(b.Active)
	if !ok {
		return b, nil, badRequest("invalidValue", "active must be a boolean")
	}
	return b, &a, nil
}

// parseBool accepts a JSON boolean or Entra's "True"/"False" strings.
func parseBool(v any) (bool, bool) {
	switch t := v.(type) {
	case bool:
		return t, true
	case string:
		b, err := strconv.ParseBool(strings.ToLower(t))
		return b, err == nil
	}
	return false, false
}

// canonicalSubject puts a GUID-shaped externalId in the form OIDC tokens carry
// it (Entra oid, Keycloak sub: lower-case, hyphenated). Blocks and tuples
// match the token subject byte for byte, so an upper-case GUID from the IdP
// would block nobody. Other subjects are kept as sent: they may be
// case-sensitive.
func canonicalSubject(s string) string {
	if id, err := uuid.Parse(s); err == nil {
		return id.String()
	}
	return s
}

// validateUser checks u and canonicalises its externalId in place.
func validateUser(u *db.ScimUser) error {
	u.ExternalID = canonicalSubject(u.ExternalID)
	if strings.TrimSpace(u.UserName) == "" {
		return badRequest("invalidValue", "userName is required")
	}
	// externalId is the principal: it must be the OIDC subject (Entra
	// objectId). svc- subjects are reserved for service principals.
	if strings.TrimSpace(u.ExternalID) == "" {
		return badRequest("invalidValue", "externalId is required: map it to the user's OIDC subject (Entra objectId)")
	}
	// group- is the group memory segment, refused as a north subject (F175).
	if strings.HasPrefix(u.ExternalID, "svc-") || strings.HasPrefix(u.ExternalID, "group-") || strings.ContainsAny(u.ExternalID, " #:*") {
		return badRequest("invalidValue", "externalId %q is not a valid subject", u.ExternalID)
	}
	if len(u.UserName) > 256 || len(u.ExternalID) > 256 {
		return badRequest("invalidValue", "userName and externalId are limited to 256 bytes")
	}
	return nil
}

func (h *Handler) createUser(ctx context.Context, req Request, body []byte) (int, any, error) {
	b, active, err := decodeUser(body)
	if err != nil {
		return 0, nil, err
	}
	u := db.ScimUser{UserName: b.UserName, ExternalID: b.ExternalID, DisplayName: b.DisplayName, Emails: b.Emails, Active: active == nil || *active}
	if err := validateUser(&u); err != nil {
		return 0, nil, err
	}
	// Block before the row exists: a failure leaves no active user behind.
	if err := h.applyActive(ctx, req.Tenant, u.ExternalID, u.Active); err != nil {
		return 0, nil, err
	}
	if err := h.Store.CreateUser(ctx, req.Tenant, &u); err != nil {
		if errors.Is(err, db.ErrScimConflict) {
			return 0, nil, &Error{Status: http.StatusConflict, ScimType: "uniqueness", Detail: "userName or externalId already exists"}
		}
		return 0, nil, err
	}
	h.emit(ctx, req, "aikonos.broker.scim.user.created", "user:"+u.ExternalID, map[string]any{"scim_id": u.ID.String(), "user_name": u.UserName, "active": u.Active})
	return http.StatusCreated, userJSON(u), nil
}

func (h *Handler) putUser(ctx context.Context, req Request, id uuid.UUID) (int, any, error) {
	u, err := h.getUser(ctx, req.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	b, active, err := decodeUser(req.Body)
	if err != nil {
		return 0, nil, err
	}
	next := u
	next.UserName, next.DisplayName, next.Emails = b.UserName, b.DisplayName, b.Emails
	if b.ExternalID != "" {
		next.ExternalID = b.ExternalID
	}
	if active != nil { // an omitted active keeps the current state
		next.Active = *active
	}
	return h.saveUser(ctx, req, u, next)
}

// patchOp is one RFC 7644 §3.5.2 operation. Entra capitalises op names and
// may send active as "False".
type patchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

func decodePatch(body []byte) ([]patchOp, error) {
	var p struct {
		Operations []patchOp `json:"Operations"`
	}
	if err := json.Unmarshal(body, &p); err != nil || len(p.Operations) == 0 {
		return nil, badRequest("invalidSyntax", "body is not a SCIM PatchOp with Operations")
	}
	for i := range p.Operations {
		p.Operations[i].Op = strings.ToLower(p.Operations[i].Op)
		switch p.Operations[i].Op {
		case "add", "replace", "remove":
		default:
			return nil, badRequest("invalidSyntax", "unknown op %q", p.Operations[i].Op)
		}
	}
	return p.Operations, nil
}

func (h *Handler) patchUser(ctx context.Context, req Request, id uuid.UUID) (int, any, error) {
	u, err := h.getUser(ctx, req.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	ops, err := decodePatch(req.Body)
	if err != nil {
		return 0, nil, err
	}
	next := u
	next.Emails = append([]db.ScimEmail(nil), u.Emails...)
	for _, op := range ops {
		if op.Path == "" { // {"op":"replace","value":{"active":false,...}}
			var attrs map[string]json.RawMessage
			if err := json.Unmarshal(op.Value, &attrs); err != nil {
				return 0, nil, badRequest("invalidValue", "a patch without path needs an object value")
			}
			for k, v := range attrs {
				if err := setUserAttr(&next, op.Op, k, v); err != nil {
					return 0, nil, err
				}
			}
			continue
		}
		if err := setUserAttr(&next, op.Op, op.Path, op.Value); err != nil {
			return 0, nil, err
		}
	}
	return h.saveUser(ctx, req, u, next)
}

// emailPathRe matches Entra's `emails[type eq "work"].value`.
var emailPathRe = regexp.MustCompile(`(?i)^emails\[type eq "([^"]*)"\]\.value$`)

// setUserAttr applies one patch to the attributes this provider stores.
// Attributes it does not store (name.*, title, enterprise extension) are
// accepted and ignored.
func setUserAttr(u *db.ScimUser, op, path string, raw json.RawMessage) error {
	str := func() (string, error) {
		var s string
		if op == "remove" {
			return "", nil
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", badRequest("invalidValue", "%s must be a string", path)
		}
		return s, nil
	}
	var err error
	switch lp := strings.ToLower(path); {
	case lp == "active":
		if op == "remove" {
			return badRequest("mutability", "active cannot be removed")
		}
		var v any
		_ = json.Unmarshal(raw, &v)
		a, ok := parseBool(v)
		if !ok {
			return badRequest("invalidValue", "active must be a boolean")
		}
		u.Active = a
	case lp == "username":
		u.UserName, err = str()
	case lp == "externalid":
		u.ExternalID, err = str()
	case lp == "displayname":
		u.DisplayName, err = str()
	case lp == "emails":
		if op == "remove" {
			u.Emails = nil
			return nil
		}
		var es []db.ScimEmail
		if err := json.Unmarshal(raw, &es); err != nil {
			return badRequest("invalidValue", "emails must be a list")
		}
		u.Emails = es
	case emailPathRe.MatchString(path):
		typ := emailPathRe.FindStringSubmatch(path)[1]
		v, err := str()
		if err != nil {
			return err
		}
		kept := u.Emails[:0]
		for _, e := range u.Emails {
			if !strings.EqualFold(e.Type, typ) {
				kept = append(kept, e)
			}
		}
		u.Emails = kept
		if op != "remove" {
			u.Emails = append(u.Emails, db.ScimEmail{Value: v, Type: typ, Primary: len(kept) == 0})
		}
	}
	return err
}

// saveUser validates next against prev, moves the block, then persists.
func (h *Handler) saveUser(ctx context.Context, req Request, prev, next db.ScimUser) (int, any, error) {
	if err := validateUser(&next); err != nil {
		return 0, nil, err
	}
	// A row stored before canonicalisation keeps its stored subject.
	if canonicalSubject(prev.ExternalID) == next.ExternalID {
		next.ExternalID = prev.ExternalID
	}
	// The subject keys every block and tuple; moving it would leave both
	// behind on the old subject.
	if next.ExternalID != prev.ExternalID {
		return 0, nil, badRequest("mutability", "externalId cannot change: deprovision the user and create a new one")
	}
	if err := h.applyActive(ctx, req.Tenant, next.ExternalID, next.Active); err != nil {
		return 0, nil, err
	}
	if err := h.Store.UpdateUser(ctx, req.Tenant, &next); err != nil {
		if errors.Is(err, db.ErrScimConflict) {
			return 0, nil, &Error{Status: http.StatusConflict, ScimType: "uniqueness", Detail: "userName already exists"}
		}
		return 0, nil, err
	}
	event := "aikonos.broker.scim.user.updated"
	switch {
	case prev.Active && !next.Active:
		event = "aikonos.broker.scim.user.deactivated"
	case !prev.Active && next.Active:
		event = "aikonos.broker.scim.user.reactivated"
	}
	h.emit(ctx, req, event, "user:"+next.ExternalID, map[string]any{"scim_id": next.ID.String(), "user_name": next.UserName, "active": next.Active})
	return http.StatusOK, userJSON(next), nil
}

// applyActive moves the AG-06 block to match active. Reactivation lifts only
// a block SCIM set: an admin's own block stays until an admin lifts it.
func (h *Handler) applyActive(ctx context.Context, tenant, subject string, active bool) error {
	if !active {
		_, err := h.Blocks.BlockUser(ctx, tenant, subject, "deactivated in the identity provider (SCIM)", BlockedBy)
		return err
	}
	blocks, err := h.Blocks.UserBlocks(ctx, tenant)
	if err != nil {
		return err
	}
	if b, ok := blocks[subject]; ok && b.By == BlockedBy {
		_, err = h.Blocks.UnblockUser(ctx, tenant, subject, BlockedBy)
	}
	return err
}

// deleteUser is a deactivation that also forgets the user: the block stays,
// the user's group tuples go.
func (h *Handler) deleteUser(ctx context.Context, req Request, id uuid.UUID) (int, any, error) {
	u, err := h.getUser(ctx, req.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	if err := h.applyActive(ctx, req.Tenant, u.ExternalID, false); err != nil {
		return 0, nil, err
	}
	groups, err := h.Store.ListGroups(ctx, req.Tenant)
	if err != nil {
		return 0, nil, err
	}
	var removed []string
	for _, g := range groups {
		for _, m := range g.Members {
			if m == id {
				if err := h.Tuples.DeleteRelations(ctx, memberTuple(u.ExternalID, g.FGAGroup)); err != nil {
					return 0, nil, err
				}
				removed = append(removed, g.FGAGroup)
			}
		}
	}
	if err := h.Store.DeleteUser(ctx, req.Tenant, id); err != nil && !errors.Is(err, db.ErrNotFound) {
		return 0, nil, err
	}
	h.emit(ctx, req, "aikonos.broker.scim.user.deleted", "user:"+u.ExternalID, map[string]any{"scim_id": id.String(), "user_name": u.UserName, "groups_removed": strings.Join(removed, ",")})
	return http.StatusNoContent, nil, nil
}

// ── groups ───────────────────────────────────────────────────────────────────

func (h *Handler) listGroups(ctx context.Context, tenant string, q url.Values) (int, any, error) {
	attr, val, err := parseFilter(q.Get("filter"), "displayName", "externalId", "id")
	if err != nil {
		return 0, nil, err
	}
	groups, err := h.Store.ListGroups(ctx, tenant)
	if err != nil {
		return 0, nil, err
	}
	items := []any{}
	for _, g := range groups {
		if attr == "displayName" && !strings.EqualFold(g.DisplayName, val) ||
			attr == "externalId" && g.ExternalID != val ||
			attr == "id" && g.ID.String() != val {
			continue
		}
		items = append(items, groupJSON(g, excludesMembers(q)))
	}
	return listResponse(q, items)
}

func (h *Handler) getGroup(ctx context.Context, tenant string, id uuid.UUID) (db.ScimGroup, error) {
	g, err := h.Store.GetGroup(ctx, tenant, id)
	if errors.Is(err, db.ErrNotFound) {
		return g, &Error{Status: http.StatusNotFound, Detail: "group not found"}
	}
	return g, err
}

var slugRe = regexp.MustCompile(`[^a-z0-9._-]+`)

// FGAGroupName derives the FGA group id from a displayName: lowercase, runs
// of other characters become "-", at most 64 bytes ("Finance Team" →
// "finance-team"). It must pass provisioning.ValidateGroups.
func FGAGroupName(displayName string) (string, error) {
	s := slugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(displayName)), "-")
	s = strings.TrimLeft(s, "._-")
	if len(s) > 64 {
		s = s[:64]
	}
	s = strings.TrimRight(s, "-")
	if err := provisioning.ValidateGroups([]string{s}); err != nil {
		return "", badRequest("invalidValue", "displayName %q gives no valid group name", displayName)
	}
	return s, nil
}

type memberRef struct {
	Value string `json:"value"`
}

type groupBody struct {
	DisplayName string      `json:"displayName"`
	ExternalID  string      `json:"externalId"`
	Members     []memberRef `json:"members"`
}

func memberTuple(subject, group string) policy.Relation {
	return policy.Relation{User: "user:" + subject, Relation: "member", Object: "group:" + group}
}

// subjects maps member ids to subjects; every id must be a provisioned user.
func (h *Handler) subjects(ctx context.Context, tenant string, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	users, err := h.Store.ListUsers(ctx, tenant)
	if err != nil {
		return nil, err
	}
	all := make(map[uuid.UUID]string, len(users))
	for _, u := range users {
		all[u.ID] = u.ExternalID
	}
	out := make(map[uuid.UUID]string, len(ids))
	for _, id := range ids {
		s, ok := all[id]
		if !ok {
			return nil, badRequest("invalidValue", "member %s is not a provisioned user", id)
		}
		out[id] = s
	}
	return out, nil
}

func parseMembers(refs []memberRef) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, 0, len(refs))
	seen := map[uuid.UUID]bool{}
	for _, r := range refs {
		id, err := uuid.Parse(r.Value)
		if err != nil {
			return nil, badRequest("invalidValue", "member %q is not a user id", r.Value)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

// writeMembers adds the FGA tuples of added and drops those of removed. A
// duplicate tuple counts as written, so a retried request converges.
func (h *Handler) writeMembers(ctx context.Context, tenant, group string, added, removed []uuid.UUID) error {
	subj, err := h.subjects(ctx, tenant, append(append([]uuid.UUID(nil), added...), removed...))
	if err != nil {
		return err
	}
	for _, id := range added {
		if err := h.Tuples.WriteRelations(ctx, memberTuple(subj[id], group)); err != nil && !provisioning.IsDuplicateTuple(err) {
			return err
		}
	}
	for _, id := range removed {
		if err := h.Tuples.DeleteRelations(ctx, memberTuple(subj[id], group)); err != nil {
			return err
		}
	}
	return nil
}

func diff(prev, next []uuid.UUID) (added, removed []uuid.UUID) {
	in := func(s []uuid.UUID, id uuid.UUID) bool {
		for _, x := range s {
			if x == id {
				return true
			}
		}
		return false
	}
	for _, id := range next {
		if !in(prev, id) {
			added = append(added, id)
		}
	}
	for _, id := range prev {
		if !in(next, id) {
			removed = append(removed, id)
		}
	}
	return added, removed
}

func (h *Handler) createGroup(ctx context.Context, req Request) (int, any, error) {
	var b groupBody
	if err := json.Unmarshal(req.Body, &b); err != nil {
		return 0, nil, badRequest("invalidSyntax", "body is not a SCIM Group")
	}
	if strings.TrimSpace(b.DisplayName) == "" || len(b.DisplayName) > 256 {
		return 0, nil, badRequest("invalidValue", "displayName is required (at most 256 bytes)")
	}
	fga, err := FGAGroupName(b.DisplayName)
	if err != nil {
		return 0, nil, err
	}
	members, err := parseMembers(b.Members)
	if err != nil {
		return 0, nil, err
	}
	if _, err := h.subjects(ctx, req.Tenant, members); err != nil {
		return 0, nil, err
	}
	if inUse, err := h.groupInUse(ctx, fga); err != nil {
		return 0, nil, err
	} else if inUse {
		h.emit(ctx, req, "aikonos.broker.scim.group.claim_refused", "group:"+fga, map[string]any{"display_name": b.DisplayName})
		return 0, nil, &Error{Status: http.StatusConflict, ScimType: "uniqueness",
			Detail: fmt.Sprintf("group %q is already managed in aikonOS; rename the IdP group or have an admin clear its tuples first", fga)}
	}
	g := db.ScimGroup{DisplayName: b.DisplayName, ExternalID: b.ExternalID, FGAGroup: fga, Members: members}
	// The row first: its unique fga_group stops two IdP groups from sharing
	// one FGA group. Tuples second; on failure the row goes again.
	if err := h.Store.CreateGroup(ctx, req.Tenant, &g); err != nil {
		if errors.Is(err, db.ErrScimConflict) {
			return 0, nil, &Error{Status: http.StatusConflict, ScimType: "uniqueness", Detail: fmt.Sprintf("a group named %q already exists", fga)}
		}
		return 0, nil, err
	}
	if err := h.writeMembers(ctx, req.Tenant, fga, members, nil); err != nil {
		if derr := h.Store.DeleteGroup(ctx, req.Tenant, g.ID); derr != nil {
			h.Logger.Warn("scim: rollback of group create failed", zap.String("group", fga), zap.Error(derr))
		}
		return 0, nil, err
	}
	h.emit(ctx, req, "aikonos.broker.scim.group.created", "group:"+fga, map[string]any{"scim_id": g.ID.String(), "display_name": g.DisplayName, "members_added": len(members)})
	return http.StatusCreated, groupJSON(g, false), nil
}

// groupInUse reports whether group:<fga> already has a tuple, as an object
// (members, managers) or as a subject set in a grant: such a group was made
// by an admin, a JIT rule or a grant, and a SCIM client adopting it could add
// anyone to whatever it confers. A read failure counts as in use.
// ponytail: read-then-create, so an admin grant racing the create can slip in.
func (h *Handler) groupInUse(ctx context.Context, fga string) (bool, error) {
	rels, err := h.Tuples.ReadTuples(ctx, policy.ReadFilter{Object: "group:" + fga})
	if err != nil || len(rels) > 0 {
		return true, err
	}
	for _, typ := range groupSubjectTypes {
		rels, err := h.Tuples.ReadTuples(ctx, policy.ReadFilter{User: "group:" + fga + "#member", Object: typ + ":"})
		if err != nil || len(rels) > 0 {
			return true, err
		}
	}
	return false, nil
}

func (h *Handler) putGroup(ctx context.Context, req Request, id uuid.UUID) (int, any, error) {
	g, err := h.getGroup(ctx, req.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	var b groupBody
	if err := json.Unmarshal(req.Body, &b); err != nil {
		return 0, nil, badRequest("invalidSyntax", "body is not a SCIM Group")
	}
	members, err := parseMembers(b.Members)
	if err != nil {
		return 0, nil, err
	}
	next := g
	next.DisplayName, next.ExternalID, next.Members = b.DisplayName, b.ExternalID, members
	return h.saveGroup(ctx, req, g, next)
}

// memberFilterRe matches Entra's `members[value eq "<id>"]`.
var memberFilterRe = regexp.MustCompile(`(?i)^members\[value eq "([^"]+)"\]$`)

func (h *Handler) patchGroup(ctx context.Context, req Request, id uuid.UUID) (int, any, error) {
	g, err := h.getGroup(ctx, req.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	ops, err := decodePatch(req.Body)
	if err != nil {
		return 0, nil, err
	}
	next := g
	next.Members = append([]uuid.UUID(nil), g.Members...)
	for _, op := range ops {
		if err := patchGroupOp(&next, op); err != nil {
			return 0, nil, err
		}
	}
	return h.saveGroup(ctx, req, g, next)
}

func patchGroupOp(g *db.ScimGroup, op patchOp) error {
	refs := func() ([]uuid.UUID, error) {
		var rs []memberRef
		if len(op.Value) == 0 {
			return nil, nil
		}
		if err := json.Unmarshal(op.Value, &rs); err != nil {
			return nil, badRequest("invalidValue", "members must be a list of {value}")
		}
		return parseMembers(rs)
	}
	without := func(drop []uuid.UUID) {
		kept := g.Members[:0]
		for _, m := range g.Members {
			keep := true
			for _, d := range drop {
				keep = keep && m != d
			}
			if keep {
				kept = append(kept, m)
			}
		}
		g.Members = kept
	}
	lp := strings.ToLower(op.Path)
	switch {
	case lp == "":
		var attrs map[string]json.RawMessage
		if err := json.Unmarshal(op.Value, &attrs); err != nil {
			return badRequest("invalidValue", "a patch without path needs an object value")
		}
		for k, v := range attrs {
			if err := patchGroupOp(g, patchOp{Op: op.Op, Path: k, Value: v}); err != nil {
				return err
			}
		}
	case lp == "displayname":
		if op.Op == "remove" {
			return badRequest("mutability", "displayName cannot be removed")
		}
		if err := json.Unmarshal(op.Value, &g.DisplayName); err != nil {
			return badRequest("invalidValue", "displayName must be a string")
		}
	case lp == "externalid":
		g.ExternalID = ""
		if op.Op != "remove" {
			if err := json.Unmarshal(op.Value, &g.ExternalID); err != nil {
				return badRequest("invalidValue", "externalId must be a string")
			}
		}
	case lp == "members":
		ids, err := refs()
		if err != nil {
			return err
		}
		switch op.Op {
		case "add":
			added, _ := diff(g.Members, ids)
			g.Members = append(g.Members, added...)
		case "replace":
			g.Members = ids
		case "remove":
			if ids == nil { // no value: remove every member
				g.Members = nil
			} else {
				without(ids)
			}
		}
	case memberFilterRe.MatchString(op.Path):
		if op.Op != "remove" {
			return badRequest("invalidPath", "only remove is supported on a members filter")
		}
		id, err := uuid.Parse(memberFilterRe.FindStringSubmatch(op.Path)[1])
		if err != nil {
			return badRequest("invalidValue", "member is not a user id")
		}
		without([]uuid.UUID{id})
	case lp == "id":
		// Entra echoes id in path-less replaces; it cannot change.
	default:
		return badRequest("invalidPath", "unsupported path %q", op.Path)
	}
	return nil
}

// saveGroup writes the membership change to FGA, then persists. The FGA
// group name is fixed at creation: a rename changes only displayName.
func (h *Handler) saveGroup(ctx context.Context, req Request, prev, next db.ScimGroup) (int, any, error) {
	if strings.TrimSpace(next.DisplayName) == "" || len(next.DisplayName) > 256 {
		return 0, nil, badRequest("invalidValue", "displayName is required (at most 256 bytes)")
	}
	added, removed := diff(prev.Members, next.Members)
	if err := h.writeMembers(ctx, req.Tenant, next.FGAGroup, added, removed); err != nil {
		return 0, nil, err
	}
	if err := h.Store.UpdateGroup(ctx, req.Tenant, &next); err != nil {
		return 0, nil, err
	}
	h.emit(ctx, req, "aikonos.broker.scim.group.updated", "group:"+next.FGAGroup, map[string]any{
		"scim_id": next.ID.String(), "display_name": next.DisplayName,
		"members_added": idList(added), "members_removed": idList(removed),
	})
	return http.StatusOK, groupJSON(next, false), nil
}

func (h *Handler) deleteGroup(ctx context.Context, req Request, id uuid.UUID) (int, any, error) {
	g, err := h.getGroup(ctx, req.Tenant, id)
	if err != nil {
		return 0, nil, err
	}
	if err := h.writeMembers(ctx, req.Tenant, g.FGAGroup, nil, g.Members); err != nil {
		return 0, nil, err
	}
	if err := h.Store.DeleteGroup(ctx, req.Tenant, id); err != nil && !errors.Is(err, db.ErrNotFound) {
		return 0, nil, err
	}
	h.emit(ctx, req, "aikonos.broker.scim.group.deleted", "group:"+g.FGAGroup, map[string]any{"scim_id": id.String(), "display_name": g.DisplayName, "members_removed": len(g.Members)})
	return http.StatusNoContent, nil, nil
}

func idList(ids []uuid.UUID) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = id.String()
	}
	return strings.Join(s, ",")
}

// ── audit ────────────────────────────────────────────────────────────────────

func (h *Handler) emit(ctx context.Context, req Request, eventType, resource string, fields map[string]any) {
	if h.Audit == nil {
		return
	}
	fields["method"] = strings.ToUpper(req.Method)
	c, _ := structpb.NewStruct(fields)
	if err := h.Audit.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    req.Tenant,
		OccurredAt:  timestamppb.Now(),
		ActorUserId: req.Actor,
		EventType:   eventType,
		ResourceRef: resource,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     c,
	}); err != nil {
		audit.RecordEmitFailure(ctx, h.Logger, err, eventType)
	}
}
