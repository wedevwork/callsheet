package client

import (
	"context"
	"net/http"

	"github.com/wedevwork/callsheet/internal/adapter"
	"github.com/wedevwork/callsheet/internal/contract"
)

// The five role operations (iteration 04). They take typed inputs and
// return domain results independent of formatting, so the CLI and a
// future MCP server share them. Responses are decoded strictly: version
// header and envelope, schema, expected ID, count, ordering, unique IDs
// and orders, and the known adapter metadata; integers exactly. Nothing is
// cached and no operation is retried.

// roleLookup is the coordinator's adapter metadata.
var roleLookup = adapter.Lookup()

func encodeBody(v any) ([]byte, error) {
	b, err := contract.Encode(v)
	if err != nil {
		return nil, contract.Wrap(contract.CodeInternal, "cannot encode the request", err)
	}
	return b, nil
}

func invalidResponse(msg string) error {
	return contract.New(contract.CodeInvalidArgument, "the plane's response is invalid: "+msg)
}

// roleResult decodes a single-role response for id.
func roleResult(b []byte, id string) (contract.RoleView, error) {
	v, err := contract.ParseRoleResponse(b, roleLookup)
	if err != nil {
		return contract.RoleView{}, err
	}
	if v.ID != id {
		return contract.RoleView{}, invalidResponse("it names another role")
	}
	return v, nil
}

// AddRole registers c (timeout optional) and returns its view. A duplicate
// ID always conflicts, including an identical retry: after an ambiguous
// result read ShowRole.
func (c *Client) AddRole(ctx context.Context, rc contract.RoleConfig) (contract.RoleView, error) {
	if err := contract.ValidateRoleConfig(rc, roleLookup); err != nil {
		return contract.RoleView{}, err
	}
	body, err := encodeBody(rc)
	if err != nil {
		return contract.RoleView{}, err
	}
	status, b, err := c.request(ctx, http.MethodPost, contract.PathRoles, body, contract.MaxRoleResponseBytes)
	if err != nil {
		return contract.RoleView{}, err
	}
	if status != http.StatusCreated {
		return contract.RoleView{}, invalidResponse("unexpected status for a role add")
	}
	v, err := roleResult(b, rc.ID)
	if err != nil {
		return contract.RoleView{}, err
	}
	if v.Node != rc.Node {
		return contract.RoleView{}, invalidResponse("it names another node")
	}
	return v, nil
}

// SetRole applies a nonempty patch of mutable fields to role id.
func (c *Client) SetRole(ctx context.Context, id string, p contract.RolePatch) (contract.RoleView, error) {
	if !contract.ValidSlug(id) {
		return contract.RoleView{}, contract.New(contract.CodeInvalidArgument, "invalid role ID; want a 1-63 character slug of lowercase letters, digits and internal hyphens")
	}
	if err := contract.ValidatePatch(p); err != nil {
		return contract.RoleView{}, err
	}
	body, err := encodeBody(p)
	if err != nil {
		return contract.RoleView{}, err
	}
	status, b, err := c.request(ctx, http.MethodPatch, contract.PathRoles+"/"+id, body, contract.MaxRoleResponseBytes)
	if err != nil {
		return contract.RoleView{}, err
	}
	if status != http.StatusOK {
		return contract.RoleView{}, invalidResponse("unexpected status for a role change")
	}
	return roleResult(b, id)
}

// ListRoles returns every role, sorted by name, then registration order.
func (c *Client) ListRoles(ctx context.Context) ([]contract.RoleView, error) {
	status, b, err := c.request(ctx, http.MethodGet, contract.PathRoles, nil, contract.MaxRoleListBytes)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, invalidResponse("unexpected status for a role list")
	}
	return contract.ParseRoleListResponse(b, roleLookup)
}

// ShowRole returns role id.
func (c *Client) ShowRole(ctx context.Context, id string) (contract.RoleView, error) {
	if !contract.ValidSlug(id) {
		return contract.RoleView{}, contract.New(contract.CodeInvalidArgument, "invalid role ID; want a 1-63 character slug of lowercase letters, digits and internal hyphens")
	}
	status, b, err := c.request(ctx, http.MethodGet, contract.PathRoles+"/"+id, nil, contract.MaxRoleResponseBytes)
	if err != nil {
		return contract.RoleView{}, err
	}
	if status != http.StatusOK {
		return contract.RoleView{}, invalidResponse("unexpected status for a role show")
	}
	return roleResult(b, id)
}

// RemoveRole deletes role id. force is accepted for the future task
// semantics; in this iteration both values remove identically.
func (c *Client) RemoveRole(ctx context.Context, id string, force bool) error {
	if !contract.ValidSlug(id) {
		return contract.New(contract.CodeInvalidArgument, "invalid role ID; want a 1-63 character slug of lowercase letters, digits and internal hyphens")
	}
	body, err := encodeBody(contract.RoleRemoveRequest{Force: force})
	if err != nil {
		return err
	}
	status, b, err := c.request(ctx, http.MethodDelete, contract.PathRoles+"/"+id, body, contract.MaxRoleResponseBytes)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return invalidResponse("unexpected status for a role removal")
	}
	removed, err := contract.ParseRoleRemoveResponse(b)
	if err != nil {
		return err
	}
	if removed != id {
		return invalidResponse("it names another role")
	}
	return nil
}
