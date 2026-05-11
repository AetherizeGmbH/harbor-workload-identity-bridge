// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	httptransport "github.com/go-openapi/runtime/client"
	v2client "github.com/goharbor/go-client/pkg/sdk/v2.0/client"
	sdkrobot "github.com/goharbor/go-client/pkg/sdk/v2.0/client/robot"
	"github.com/goharbor/go-client/pkg/sdk/v2.0/models"
)

const (
	// harborBasePath is the v2 API root Harbor exposes. The SDK requires
	// this on the URL passed to v2client.New.
	harborBasePath = "/api/v2.0"

	// pageSize is the page size the wrapper uses when walking the paginated
	// robot list. Harbor's default is small; 100 keeps round-trip count low
	// without hammering memory on huge fleets.
	pageSize int64 = 100

	// robotLevelSystem is the value Harbor expects in RobotCreate.Level for
	// system-scope robots (which can hold permissions across multiple
	// projects). See goharbor/harbor src/common/rbac for the constants.
	robotLevelSystem = "system"

	// robotResourceRepository is the resource string for image push/pull
	// access on a Harbor project.
	robotResourceRepository = "repository"

	// robotPermissionKindProject is the value Harbor expects in
	// RobotPermission.Kind when scoping access to a specific project.
	robotPermissionKindProject = "project"

	// robotDurationNeverExpires tells Harbor not to expire the robot
	// itself. The bridge rotates the password on its own schedule
	// (see ADR-0003); robot lifetime is governed by the HarborAccess CR.
	robotDurationNeverExpires int64 = -1
)

// ProjectPermission is a project-scoped action expressed in the bridge's
// terms. Action is one of "pull", "push", "pull,push" — the wrapper
// expands the comma-form into the two Access entries Harbor expects.
type ProjectPermission struct {
	Project string
	Action  string
}

// Robot is the bridge's view of a Harbor robot. Only the fields the
// control plane actually needs are exposed. Secret is non-empty only on
// the response of a freshly Created or just-Refreshed robot.
type Robot struct {
	ID          int64
	Name        string
	Description string
	Secret      string
}

// ErrRobotNotFound is returned by GetByID and GetByName when no matching
// robot exists. Callers use errors.Is to distinguish from transport errors.
var ErrRobotNotFound = errors.New("robot not found")

// Client is the small surface the reconciler and janitor need against
// Harbor. The bridge's ownership-prefix safety invariant (ADR-0009) is the
// caller's responsibility; Client is intentionally cluster-agnostic so its
// methods can be reused (e.g. by future tooling) without re-encoding the
// prefix rule.
type Client interface {
	Create(ctx context.Context, name, description string, perms []ProjectPermission) (*Robot, error)
	Delete(ctx context.Context, id int64) error
	List(ctx context.Context) ([]Robot, error)
	GetByID(ctx context.Context, id int64) (*Robot, error)
	GetByName(ctx context.Context, name string) (*Robot, error)
	RefreshSecret(ctx context.Context, id int64) (string, error)
	UpdatePermissions(ctx context.Context, id int64, description string, perms []ProjectPermission) error
}

// goClient is the production Client implementation, backed by
// github.com/goharbor/go-client.
type goClient struct {
	api *v2client.HarborAPI
}

// NewClient builds a Client connected to harborURL using HTTP Basic Auth.
// transport is optional; pass non-nil to override the default (httptest
// servers, custom TLS, mTLS, instrumented round-trippers, etc.).
func NewClient(harborURL *url.URL, username, password string, transport http.RoundTripper) (Client, error) {
	if harborURL == nil {
		return nil, errors.New("harborURL is nil")
	}
	u := *harborURL
	// The SDK derives its base path from u.Path. If the operator passed
	// just "https://harbor.example.com", we need to append /api/v2.0.
	switch {
	case u.Path == "" || u.Path == "/":
		u.Path = harborBasePath
	case !strings.HasSuffix(u.Path, harborBasePath):
		u.Path = strings.TrimRight(u.Path, "/") + harborBasePath
	}

	cfg := v2client.Config{
		URL:       &u,
		Transport: transport,
		AuthInfo:  httptransport.BasicAuth(username, password),
	}
	return &goClient{api: v2client.New(cfg)}, nil
}

func (c *goClient) Create(ctx context.Context, name, description string, perms []ProjectPermission) (*Robot, error) {
	body := &models.RobotCreate{
		Name:        name,
		Description: description,
		Level:       robotLevelSystem,
		Duration:    robotDurationNeverExpires,
		Permissions: toHarborPermissions(perms),
	}
	params := sdkrobot.NewCreateRobotParamsWithContext(ctx).WithRobot(body)
	resp, err := c.api.Robot.CreateRobot(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("create robot %q: %w", name, err)
	}
	return &Robot{
		ID:          resp.Payload.ID,
		Name:        resp.Payload.Name,
		Description: description,
		Secret:      resp.Payload.Secret,
	}, nil
}

func (c *goClient) Delete(ctx context.Context, id int64) error {
	params := sdkrobot.NewDeleteRobotParamsWithContext(ctx).WithRobotID(id)
	if _, err := c.api.Robot.DeleteRobot(ctx, params); err != nil {
		if isNotFound(err) {
			// Delete is idempotent at the bridge level.
			return nil
		}
		return fmt.Errorf("delete robot %d: %w", id, err)
	}
	return nil
}

func (c *goClient) List(ctx context.Context) ([]Robot, error) {
	page := int64(1)
	size := pageSize
	var out []Robot
	for {
		params := sdkrobot.NewListRobotParamsWithContext(ctx).
			WithPage(&page).
			WithPageSize(&size)
		resp, err := c.api.Robot.ListRobot(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("list robots (page %d): %w", page, err)
		}
		for _, r := range resp.Payload {
			out = append(out, fromHarborRobot(r))
		}
		if int64(len(resp.Payload)) < size {
			return out, nil
		}
		page++
	}
}

func (c *goClient) GetByID(ctx context.Context, id int64) (*Robot, error) {
	params := sdkrobot.NewGetRobotByIDParamsWithContext(ctx).WithRobotID(id)
	resp, err := c.api.Robot.GetRobotByID(ctx, params)
	if err != nil {
		if isNotFound(err) {
			return nil, ErrRobotNotFound
		}
		return nil, fmt.Errorf("get robot %d: %w", id, err)
	}
	r := fromHarborRobot(resp.Payload)
	return &r, nil
}

func (c *goClient) GetByName(ctx context.Context, name string) (*Robot, error) {
	robots, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	for i := range robots {
		if robots[i].Name == name {
			return &robots[i], nil
		}
	}
	return nil, ErrRobotNotFound
}

func (c *goClient) RefreshSecret(ctx context.Context, id int64) (string, error) {
	params := sdkrobot.NewRefreshSecParamsWithContext(ctx).
		WithRobotID(id).
		WithRobotSec(&models.RobotSec{})
	resp, err := c.api.Robot.RefreshSec(ctx, params)
	if err != nil {
		return "", fmt.Errorf("refresh secret for robot %d: %w", id, err)
	}
	return resp.Payload.Secret, nil
}

func (c *goClient) UpdatePermissions(ctx context.Context, id int64, description string, perms []ProjectPermission) error {
	// models.Robot.Duration is *int64 (x-nullable in swagger); take address.
	duration := robotDurationNeverExpires
	body := &models.Robot{
		ID:          id,
		Description: description,
		Level:       robotLevelSystem,
		Duration:    &duration,
		Permissions: toHarborPermissions(perms),
	}
	params := sdkrobot.NewUpdateRobotParamsWithContext(ctx).
		WithRobotID(id).
		WithRobot(body)
	if _, err := c.api.Robot.UpdateRobot(ctx, params); err != nil {
		return fmt.Errorf("update robot %d: %w", id, err)
	}
	return nil
}

// FilterOwned returns the subset of robots whose names start with the
// cluster's ownership prefix. The ADR-0009 safety invariant lives at the
// reconciler and janitor call sites; this helper centralises the filter
// so all callers use one implementation.
func FilterOwned(robots []Robot, cluster string) []Robot {
	if cluster == "" {
		return nil
	}
	prefix := ClusterPrefix(cluster)
	out := make([]Robot, 0, len(robots))
	for _, r := range robots {
		if strings.HasPrefix(r.Name, prefix) {
			out = append(out, r)
		}
	}
	return out
}

// toHarborPermissions translates our compact ProjectPermission into
// Harbor's two-level (RobotPermission + Access) shape. The "pull,push"
// shorthand becomes two Access entries; whitespace around commas is
// tolerated.
func toHarborPermissions(perms []ProjectPermission) []*models.RobotPermission {
	out := make([]*models.RobotPermission, 0, len(perms))
	for _, p := range perms {
		accesses := []*models.Access{}
		for _, action := range strings.Split(p.Action, ",") {
			action = strings.TrimSpace(action)
			if action == "" {
				continue
			}
			accesses = append(accesses, &models.Access{
				Resource: robotResourceRepository,
				Action:   action,
			})
		}
		out = append(out, &models.RobotPermission{
			Kind:      robotPermissionKindProject,
			Namespace: p.Project,
			Access:    accesses,
		})
	}
	return out
}

func fromHarborRobot(r *models.Robot) Robot {
	return Robot{
		ID:          r.ID,
		Name:        r.Name,
		Description: r.Description,
		Secret:      r.Secret,
	}
}

// harborStatusErr is the interface every generated go-client error response
// implements (IsCode, IsClientError, IsServerError). We use it via errors.As
// to detect 404s without depending on the specific operation's error type.
type harborStatusErr interface {
	error
	IsCode(int) bool
}

func isNotFound(err error) bool {
	var hse harborStatusErr
	if errors.As(err, &hse) {
		return hse.IsCode(http.StatusNotFound)
	}
	return false
}
