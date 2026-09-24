// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package harbor

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

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

	// maxPages bounds one robot listing (maxPages*pageSize robots). A
	// Harbor or proxy that ignores the page parameter would otherwise
	// make the walk loop forever and grow memory without bound.
	maxPages = 1000

	// DefaultCallTimeout bounds one Harbor API call. The generated SDK
	// params set a request timeout of 0 (none), and the reconcile and
	// janitor contexts carry no deadline, so without it one Harbor that
	// accepts the connection and never answers blocks the only reconcile
	// worker: rotation, revocation and deletion stop for every
	// HarborAccess, and DeletionBlocked is never reported.
	DefaultCallTimeout = 30 * time.Second

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
// the response of a freshly Created robot.
type Robot struct {
	ID int64

	// Name is the bridge-internal robot name: what RobotName returns and
	// what Create sends. The client strips Harbor's configured robot name
	// prefix (default "robot$", ADR-0014) on every read path, so callers
	// compare internal names only and never handle the prefix themselves.
	Name string

	// WireName is the name exactly as Harbor reports it
	// (<robot prefix><Name>). It is the Basic Auth username registry
	// clients must present, and Harbor requires it verbatim on update
	// (PUT /robots/{id} rejects any name that differs from the stored
	// on-wire name with 400 "cannot update the level or name of robot").
	WireName string

	Description string

	// Disabled mirrors Harbor's "disable" flag. Update echoes it back
	// unchanged: Harbor's PUT overwrites the flag with whatever the body
	// carries, so omitting it would silently re-enable a robot an
	// administrator disabled.
	Disabled bool

	// ExpiresAt is Harbor's expiry timestamp (unix seconds); -1 means the
	// robot never expires, which is what the bridge always requests.
	ExpiresAt int64

	// Permissions is the robot's project/repository grant set, normalized
	// by the client (one entry per project, canonical action order).
	Permissions []ProjectPermission

	// ForeignAccess reports that Harbor returned at least one grant the
	// bridge never writes (another resource, kind, or a deny effect). A
	// robot with foreign access never matches a desired permission set,
	// so the reconciler rewrites it back to exactly what the CR declares.
	ForeignAccess bool

	Secret string
}

// ErrRobotNotFound is returned by GetByName when no matching robot
// exists. Callers use errors.Is to distinguish from transport errors.
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
	GetByName(ctx context.Context, name string) (*Robot, error)
	RefreshSecret(ctx context.Context, id int64) (string, error)
	// Update rewrites the description and permission set of an existing
	// robot. current must come from a read path (List/GetByName): its
	// WireName and Disabled flag are echoed back as Harbor requires.
	Update(ctx context.Context, current *Robot, description string, perms []ProjectPermission) error
}

// Option configures NewClient.
type Option func(*goClient)

// WithRobotPrefix sets the robot name prefix the Harbor instance is
// configured with (Harbor's robot_name_prefix setting, default "robot$").
// Harbor stores robot names without it and prepends it on every read
// path; the client strips it again so callers only see internal names.
func WithRobotPrefix(prefix string) Option {
	return func(c *goClient) { c.robotPrefix = prefix }
}

// WithCallTimeout overrides DefaultCallTimeout (tests).
func WithCallTimeout(d time.Duration) Option {
	return func(c *goClient) { c.callTimeout = d }
}

// goClient is the production Client implementation, backed by
// github.com/goharbor/go-client.
type goClient struct {
	api         *v2client.HarborAPI
	robotPrefix string
	callTimeout time.Duration
}

// call derives the context for one Harbor API call.
func (c *goClient) call(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.callTimeout)
}

// defaultTransport is http.DefaultTransport with the timeouts it lacks
// (a response that never starts would otherwise hold the call until the
// per-call deadline) and TLS 1.2 as the floor.
func defaultTransport() http.RoundTripper {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		base = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	t := base.Clone()
	t.TLSHandshakeTimeout = 10 * time.Second
	t.ResponseHeaderTimeout = DefaultCallTimeout
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	return t
}

// noLogger discards the SDK runtime's debug output.
type noLogger struct{}

func (noLogger) Printf(string, ...any) {}
func (noLogger) Debugf(string, ...any) {}

// NewClient builds a Client connected to harborURL using HTTP Basic Auth.
// transport is optional; pass non-nil to override the default (httptest
// servers, custom TLS, mTLS, instrumented round-trippers, etc.). nil
// selects defaultTransport.
func NewClient(harborURL *url.URL, username, password string, transport http.RoundTripper, opts ...Option) (Client, error) {
	if harborURL == nil {
		return nil, errors.New("harborURL is nil")
	}
	u := *harborURL
	// The SDK derives its base path from u.Path. If the operator passed
	// just "https://harbor.example.com", we need to append /api/v2.0.
	// Trailing slashes are trimmed first so ".../api/v2.0/" does not
	// defeat the suffix check and double the base path.
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, harborBasePath) {
		u.Path += harborBasePath
	}

	if transport == nil {
		transport = defaultTransport()
	}
	cfg := v2client.Config{
		URL:       &u,
		Transport: transport,
		AuthInfo:  httptransport.BasicAuth(username, password),
	}
	api := v2client.New(cfg)
	// The go-openapi runtime turns on full request/response dumps when
	// DEBUG or SWAGGER_DEBUG is set in the environment: every call's
	// Authorization header (the Harbor admin credentials) and every
	// create/refresh response (robot passwords) would go to stdout.
	// Those variable names are generic enough to be set for unrelated
	// reasons, so the dumps are switched off unconditionally.
	rt, ok := api.Transport.(*httptransport.Runtime)
	if !ok {
		return nil, fmt.Errorf("harbor SDK transport is %T, want *client.Runtime (wire dumps could not be disabled)", api.Transport)
	}
	rt.SetDebug(false)
	rt.SetLogger(noLogger{})

	c := &goClient{api: api, robotPrefix: HarborRobotPrefix, callTimeout: DefaultCallTimeout}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

func (c *goClient) Create(ctx context.Context, name, description string, perms []ProjectPermission) (*Robot, error) {
	if err := validatePermissions(perms); err != nil {
		return nil, fmt.Errorf("create robot %q: %w", name, err)
	}
	body := &models.RobotCreate{
		Name:        name,
		Description: description,
		Level:       robotLevelSystem,
		Duration:    robotDurationNeverExpires,
		Permissions: toHarborPermissions(perms),
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	params := sdkrobot.NewCreateRobotParamsWithContext(ctx).WithRobot(body)
	resp, err := c.api.Robot.CreateRobot(ctx, params)
	if err != nil {
		return nil, wrapHarborOp(fmt.Sprintf("create robot %q", name), err)
	}
	if resp.Payload == nil || resp.Payload.Secret == "" {
		// Storing an empty password would fail every pull until the
		// next scheduled rotation, 24h later.
		return nil, fmt.Errorf("create robot %q: Harbor returned no secret", name)
	}
	wire := resp.Payload.Name
	if wire == "" {
		wire = c.robotPrefix + name
	}
	return &Robot{
		ID:          resp.Payload.ID,
		Name:        name,
		WireName:    wire,
		Description: description,
		ExpiresAt:   robotDurationNeverExpires,
		Permissions: normalizePermissions(perms),
		Secret:      resp.Payload.Secret,
	}, nil
}

func (c *goClient) Delete(ctx context.Context, id int64) error {
	ctx, cancel := c.call(ctx)
	defer cancel()
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

// List returns every system-level robot (Harbor's GET /robots without a
// Level filter lists exactly those).
func (c *goClient) List(ctx context.Context) ([]Robot, error) {
	return c.list(ctx, nil)
}

func (c *goClient) list(ctx context.Context, q *string) ([]Robot, error) {
	page := int64(1)
	size := pageSize
	var out []Robot
	for ; page <= maxPages; page++ {
		resp, err := c.listPage(ctx, page, size, q)
		if err != nil {
			return nil, wrapHarborOp(fmt.Sprintf("list robots (page %d)", page), err)
		}
		for _, r := range resp.Payload {
			if r == nil {
				continue
			}
			out = append(out, c.fromHarborRobot(r))
		}
		if int64(len(resp.Payload)) < size {
			return out, nil
		}
	}
	return nil, fmt.Errorf("list robots: more than %d pages of %d; refusing to continue (does the Harbor endpoint ignore the page parameter?)", maxPages, size)
}

func (c *goClient) listPage(ctx context.Context, page, size int64, q *string) (*sdkrobot.ListRobotOK, error) {
	ctx, cancel := c.call(ctx)
	defer cancel()
	params := sdkrobot.NewListRobotParamsWithContext(ctx).
		WithPage(&page).
		WithPageSize(&size).
		WithQ(q)
	return c.api.Robot.ListRobot(ctx, params)
}

// GetByName looks the robot up by its internal name. It asks Harbor for
// an exact name match first (q=name=<name>; Harbor stores names without
// the robot prefix, so the filter is prefix-agnostic) instead of paging
// through every robot on every reconcile. If the filtered query returns
// no match it falls back to a full scan, so a Harbor build whose query
// filter behaves differently degrades to the old O(robots) cost instead
// of a create/409 loop.
func (c *goClient) GetByName(ctx context.Context, name string) (*Robot, error) {
	q := "name=" + name
	robots, err := c.list(ctx, &q)
	if err != nil {
		return nil, err
	}
	if r := matchName(robots, name); r != nil {
		return r, nil
	}
	robots, err = c.List(ctx)
	if err != nil {
		return nil, err
	}
	if r := matchName(robots, name); r != nil {
		return r, nil
	}
	return nil, ErrRobotNotFound
}

func matchName(robots []Robot, name string) *Robot {
	for i := range robots {
		if robots[i].Name == name {
			return &robots[i]
		}
	}
	return nil
}

func (c *goClient) RefreshSecret(ctx context.Context, id int64) (string, error) {
	ctx, cancel := c.call(ctx)
	defer cancel()
	params := sdkrobot.NewRefreshSecParamsWithContext(ctx).
		WithRobotID(id).
		WithRobotSec(&models.RobotSec{})
	resp, err := c.api.Robot.RefreshSec(ctx, params)
	if err != nil {
		return "", wrapHarborOp(fmt.Sprintf("refresh secret for robot %d", id), err)
	}
	if resp.Payload == nil || resp.Payload.Secret == "" {
		return "", fmt.Errorf("refresh secret for robot %d: Harbor returned no secret", id)
	}
	return resp.Payload.Secret, nil
}

func (c *goClient) Update(ctx context.Context, current *Robot, description string, perms []ProjectPermission) error {
	if current == nil || current.WireName == "" {
		return errors.New("update robot: current robot with its on-wire name is required")
	}
	if err := validatePermissions(perms); err != nil {
		return fmt.Errorf("update robot %d: %w", current.ID, err)
	}
	// models.Robot.Duration is *int64 (x-nullable in swagger); take address.
	duration := robotDurationNeverExpires
	body := &models.Robot{
		ID:          current.ID,
		Name:        current.WireName,
		Description: description,
		Level:       robotLevelSystem,
		Duration:    &duration,
		Disable:     current.Disabled,
		Permissions: toHarborPermissions(perms),
	}
	ctx, cancel := c.call(ctx)
	defer cancel()
	params := sdkrobot.NewUpdateRobotParamsWithContext(ctx).
		WithRobotID(current.ID).
		WithRobot(body)
	if _, err := c.api.Robot.UpdateRobot(ctx, params); err != nil {
		return wrapHarborOp(fmt.Sprintf("update robot %d", current.ID), err)
	}
	return nil
}

// PermissionsMatch reports whether the robot's grants are exactly the
// desired set (order- and duplicate-insensitive). A robot carrying any
// grant the bridge never writes does not match.
func PermissionsMatch(r *Robot, desired []ProjectPermission) bool {
	if r.ForeignAccess {
		return false
	}
	a, b := normalizePermissions(r.Permissions), normalizePermissions(desired)
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// normalizePermissions merges entries per project, dedupes actions, and
// sorts both, rendering each project's actions in canonical order
// ("pull" before "push", then anything else alphabetically) joined by
// ",". The result is a stable form for comparison and for writing.
func normalizePermissions(perms []ProjectPermission) []ProjectPermission {
	byProject := map[string]map[string]struct{}{}
	for _, p := range perms {
		set := byProject[p.Project]
		if set == nil {
			set = map[string]struct{}{}
			byProject[p.Project] = set
		}
		for _, action := range strings.Split(p.Action, ",") {
			if action = strings.TrimSpace(action); action != "" {
				set[action] = struct{}{}
			}
		}
	}
	projects := make([]string, 0, len(byProject))
	for p := range byProject {
		projects = append(projects, p)
	}
	sort.Strings(projects)
	out := make([]ProjectPermission, 0, len(projects))
	for _, p := range projects {
		actions := make([]string, 0, len(byProject[p]))
		for a := range byProject[p] {
			actions = append(actions, a)
		}
		sort.Slice(actions, func(i, j int) bool { return actionRank(actions[i], actions[j]) })
		out = append(out, ProjectPermission{Project: p, Action: strings.Join(actions, ",")})
	}
	return out
}

func actionRank(a, b string) bool {
	order := map[string]int{"pull": 0, "push": 1}
	ra, oka := order[a]
	rb, okb := order[b]
	switch {
	case oka && okb:
		return ra < rb
	case oka:
		return true
	case okb:
		return false
	default:
		return a < b
	}
}

// toHarborPermissions translates our compact ProjectPermission into
// Harbor's two-level (RobotPermission + Access) shape. Entries are merged
// per project first (Harbor stores one policy row per access, so a
// project listed twice would otherwise produce duplicate rows), and the
// "pull,push" shorthand becomes two Access entries.
func toHarborPermissions(perms []ProjectPermission) []*models.RobotPermission {
	norm := normalizePermissions(perms)
	out := make([]*models.RobotPermission, 0, len(norm))
	for _, p := range norm {
		accesses := []*models.Access{}
		for _, action := range strings.Split(p.Action, ",") {
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

// fromHarborRobot converts a Harbor read-path robot. The prefix is
// stripped only when present, so legacy (v1, non-editable) robots whose
// names Harbor returns raw keep their names.
func (c *goClient) fromHarborRobot(r *models.Robot) Robot {
	out := Robot{
		ID:          r.ID,
		Name:        strings.TrimPrefix(r.Name, c.robotPrefix),
		WireName:    r.Name,
		Description: r.Description,
		Disabled:    r.Disable,
		ExpiresAt:   r.ExpiresAt,
		Secret:      r.Secret,
	}
	var perms []ProjectPermission
	for _, p := range r.Permissions {
		if p == nil {
			continue
		}
		if p.Kind != robotPermissionKindProject {
			out.ForeignAccess = true
			continue
		}
		for _, a := range p.Access {
			if a == nil {
				continue
			}
			if a.Resource != robotResourceRepository || (a.Effect != "" && a.Effect != "allow") {
				out.ForeignAccess = true
				continue
			}
			perms = append(perms, ProjectPermission{Project: p.Namespace, Action: a.Action})
		}
	}
	out.Permissions = normalizePermissions(perms)
	return out
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
	// GetByName semantics; the reconciler derives the
	// operator-action-required branch from the readable message above.
	return out
}
