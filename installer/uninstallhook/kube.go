// Copyright 2026 The Aetherize Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// serviceAccountDir is where kubelet mounts the Job's ServiceAccount token
// and the cluster CA.
const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// maxResponseBytes bounds every API response the hook reads (a pod list of
// a large cluster stays well below it).
const maxResponseBytes = 64 << 20

var (
	errNotFound = errors.New("not found")
	errRedirect = errors.New("the API server answered with a redirect, which the hook does not follow: the ServiceAccount token goes to the API server only")
)

// apiClient is the hook's minimal Kubernetes API client.
type apiClient struct {
	base      string // https://<host>:<port>
	http      *http.Client
	tokenPath string
}

// newInClusterClient is the client of the API server the pod runs against:
// KUBERNETES_SERVICE_HOST and _PORT, verified against the ServiceAccount
// CA, without a proxy (a proxy would see the token's use) and without
// following redirects.
func newInClusterClient(getenv func(string) string) (*apiClient, error) {
	host, port := getenv("KUBERNETES_SERVICE_HOST"), getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("KUBERNETES_SERVICE_HOST and KUBERNETES_SERVICE_PORT are not set: the hook runs only in a pod")
	}
	ca, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read the ServiceAccount CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("the ServiceAccount CA holds no certificate")
	}
	transport := &http.Transport{
		Proxy:               nil,
		TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return newAPIClient("https://"+net.JoinHostPort(host, port), &http.Client{Transport: transport}, serviceAccountDir+"/token"), nil
}

// newAPIClient is the client of the API server at base, with the token in
// the file tokenPath. It refuses every redirect and bounds every request.
func newAPIClient(base string, client *http.Client, tokenPath string) *apiClient {
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirect }
	c.Timeout = 30 * time.Second
	return &apiClient{base: base, http: &c, tokenPath: tokenPath}
}

// do sends one request and decodes a JSON answer into out (when not nil).
// It reads the token for every request: kubelet rotates a projected token.
func (c *apiClient) do(ctx context.Context, method, path, contentType string, body []byte, out any) error {
	token, err := os.ReadFile(c.tokenPath)
	if err != nil {
		return fmt.Errorf("read the ServiceAccount token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return fmt.Errorf("%s %s: read the answer: %w", method, path, err)
	}
	if len(raw) > maxResponseBytes {
		return fmt.Errorf("%s %s: the answer is larger than %d bytes", method, path, maxResponseBytes)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return errNotFound
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		var status struct {
			Message string `json:"message"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &status) == nil && status.Message != "" {
			msg = status.Message
		}
		if len(msg) > 500 {
			msg = msg[:500] + "..."
		}
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, msg)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s %s: decode the answer: %w", method, path, err)
	}
	return nil
}

// fieldManager names the hook's changes in the DaemonSet's managed fields.
const fieldManager = "harbor-bridge-uninstall-hook"

func daemonSetPath(namespace, name string) string {
	return "/apis/apps/v1/namespaces/" + url.PathEscape(namespace) + "/daemonsets/" + url.PathEscape(name)
}

func (c *apiClient) getDaemonSet(ctx context.Context, namespace, name string) (*daemonSet, error) {
	var ds daemonSet
	if err := c.do(ctx, http.MethodGet, daemonSetPath(namespace, name), "", nil, &ds); err != nil {
		return nil, err
	}
	return &ds, nil
}

// patchDaemonSet applies a strategic merge patch to the DaemonSet.
func (c *apiClient) patchDaemonSet(ctx context.Context, namespace, name string, patch []byte) error {
	path := daemonSetPath(namespace, name) + "?fieldManager=" + url.QueryEscape(fieldManager)
	return c.do(ctx, http.MethodPatch, path, "application/strategic-merge-patch+json", patch, nil)
}

// listPods lists the pods in namespace that match every label of selector.
func (c *apiClient) listPods(ctx context.Context, namespace string, selector map[string]string) ([]pod, error) {
	var terms []string
	for k, v := range selector {
		terms = append(terms, k+"="+v)
	}
	path := "/api/v1/namespaces/" + url.PathEscape(namespace) + "/pods?labelSelector=" + url.QueryEscape(strings.Join(terms, ","))
	var list struct {
		Items []pod `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, path, "", nil, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// The parts of the API objects the hook reads.

type daemonSet struct {
	Metadata struct {
		Generation  int64             `json:"generation"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Selector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration     int64 `json:"observedGeneration"`
		DesiredNumberScheduled int   `json:"desiredNumberScheduled"`
		UpdatedNumberScheduled int   `json:"updatedNumberScheduled"`
		NumberAvailable        int   `json:"numberAvailable"`
		NumberUnavailable      int   `json:"numberUnavailable"`
	} `json:"status"`
}

type pod struct {
	Metadata struct {
		Name              string  `json:"name"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		NodeName       string      `json:"nodeName"`
		InitContainers []container `json:"initContainers"`
	} `json:"spec"`
	Status struct {
		Phase                 string            `json:"phase"`
		InitContainerStatuses []containerStatus `json:"initContainerStatuses"`
	} `json:"status"`
}

type container struct {
	Name string `json:"name"`
	Env  []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"env"`
}

type containerStatus struct {
	Name      string         `json:"name"`
	State     containerState `json:"state"`
	LastState containerState `json:"lastState"`
}

type containerState struct {
	Waiting *struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"waiting"`
	Running    *struct{} `json:"running"`
	Terminated *struct {
		ExitCode int    `json:"exitCode"`
		Reason   string `json:"reason"`
		Message  string `json:"message"`
	} `json:"terminated"`
}
