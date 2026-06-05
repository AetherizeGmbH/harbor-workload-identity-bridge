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

// ErrRobotAlreadyExists is returned by Create when Harbor responds 409
// because a robot with the same name is already present. The reconciler
// recovers from this by re-fetching the existing robot and rotating its
// password so the per-CR Secret in the bridge namespace stays in sync.
// See ADR-0003 for the persistent-robot lifecycle.
var ErrRobotAlreadyExists = errors.New("robot already exists")

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
		return nil, wrapHarborOp(fmt.Sprintf("create robot %q", name), err)
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
		return wrapHarborOp(fmt.Sprintf("delete robot %d", id), err)
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
			return nil, wrapHarborOp(fmt.Sprintf("list robots (page %d)", page), err)
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
		return nil, wrapHarborOp(fmt.Sprintf("get robot %d", id), err)
	}
	r := fromHarborRobot(resp.Payload)
	return &r, nil
}

func (c *goClient) GetByName(ctx context.Context, name string) (*Robot, error) {
	robots, err := c.List(ctx)
	if err != nil {
		return nil, err
	}
	// Harbor reads back system-level robot names with the literal
	// "robot$" prefix even though POST /robots accepts (and we send)
	// the un-prefixed name. Callers pass us the un-prefixed form; we
	// match either form so a future Harbor version that drops the
	// asymmetry (or a project-scope robot path that may not add the
	// prefix) doesn't silently break.
	for i := range robots {
		if robots[i].Name == name || robots[i].Name == HarborRobotPrefix+name {
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
		return "", wrapHarborOp(fmt.Sprintf("refresh secret for robot %d", id), err)
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
		return wrapHarborOp(fmt.Sprintf("update robot %d", id), err)
	}
	return nil
}

// FilterOwned returns the subset of robots whose names start with the
// cluster's ownership prefix. The ADR-0009 safety invariant lives at the
// reconciler and janitor call sites; this helper centralises the filter
// so all callers use one implementation. Delegates to OwnsRobot so the
// "robot$" normalization happens in exactly one place.
func FilterOwned(robots []Robot, cluster string) []Robot {
	out := make([]Robot, 0, len(robots))
	for _, r := range robots {
		if OwnsRobot(cluster, r.Name) {
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
// runtime.APIError (returned by the SDK for status codes not enumerated in
// the swagger spec — Harbor's 409 on POST /robots, for example) implements
// IsCode too, so the same interface covers both typed and fallback errors.
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

func isConflict(err error) bool {
	var hse harborStatusErr
	if errors.As(err, &hse) {
		return hse.IsCode(http.StatusConflict)
	}
	return false
}

// harborPayloadErr is the interface every generated typed error response
// implements via its GetPayload() method. We use it to lift Harbor's
// structured error payload (codes + messages) out of an SDK error so the
// status condition message tells the operator what's actually wrong
// instead of "&{Errors:[0x71c4c2a8cfe0]}".
type harborPayloadErr interface {
	error
	GetPayload() *models.Errors
}

// formatHarborMessage returns a human-readable rendering of err. When the
// underlying SDK error carries a models.Errors payload (typed 4xx
// responses) it formats as "CODE: message; CODE: message"; otherwise it
// falls through to err.Error(), which for runtime.APIError (untyped
// fallback) already includes the status code and raw body.
func formatHarborMessage(err error) string {
	if err == nil {
		return ""
	}
	var hpe harborPayloadErr
	if errors.As(err, &hpe) {
		payload := hpe.GetPayload()
		if payload != nil && len(payload.Errors) > 0 {
			parts := make([]string, 0, len(payload.Errors))
			for _, e := range payload.Errors {
				code := e.Code
				if code == "" {
					code = "UNKNOWN"
				}
				if e.Message != "" {
					parts = append(parts, code+": "+e.Message)
				} else {
					parts = append(parts, code)
				}
			}
			return strings.Join(parts, "; ")
		}
	}
	return err.Error()
}

// hbErr wraps an SDK error so .Error() renders a clean message while
// preserving the original chain for errors.Is/errors.As (callers can
// still pattern-match against ErrRobotAlreadyExists, ErrRobotNotFound,
// or the typed harborStatusErr interface). aliases is the slice of
// sentinel errors hbErr should report a match for from errors.Is, so
// the reconciler can branch on harbor.ErrRobotAlreadyExists without
// knowing about the SDK's status-code-to-type mapping.
type hbErr struct {
	msg     string
	cause   error
	aliases []error
}

func (e *hbErr) Error() string { return e.msg }
func (e *hbErr) Unwrap() error { return e.cause }
func (e *hbErr) Is(target error) bool {
	for _, a := range e.aliases {
		if a == target {
			return true
		}
	}
	return false
}

// wrapHarborOp wraps an SDK error returned by op. Returns nil when err is
// nil so the call site stays a single line. Detects 404/409 to attach
// the relevant sentinel; the reconciler keys off those via errors.Is.
func wrapHarborOp(op string, err error) error {
	if err == nil {
		return nil
	}
	out := &hbErr{
		msg:   op + ": " + formatHarborMessage(err),
		cause: err,
	}
	if isConflict(err) {
		out.aliases = append(out.aliases, ErrRobotAlreadyExists)
	}
	// Note: a 404 from POST /robots means "referenced project not found",
	// not "this robot does not exist", so we deliberately do not alias it
	// to ErrRobotNotFound. ErrRobotNotFound stays scoped to the
	// GetByID/GetByName semantics; the reconciler derives the
	// operator-action-required branch from the readable message above.
	return out
}
