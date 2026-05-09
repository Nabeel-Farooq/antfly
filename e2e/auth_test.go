// Copyright 2025 Antfly, Inc.
//
// Licensed under the Elastic License 2.0 (ELv2); you may not use this file
// except in compliance with the Elastic License 2.0. You may obtain a copy of
// the Elastic License 2.0 at
//
//     https://www.antfly.io/licensing/ELv2-license
//
// Unless required by applicable law or agreed to in writing, software distributed
// under the Elastic License 2.0 is distributed on an "AS IS" BASIS, WITHOUT
// WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the
// Elastic License 2.0 for the specific language governing permissions and
// limitations.

package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	adminUsername = "admin"
	adminPassword = "admin"
	apiBasePath   = "/api/v1"
	testTimeout   = 2 * time.Minute
)

// permission mirrors the API permission schema.
type permission struct {
	Resource     string `json:"resource"`
	ResourceType string `json:"resource_type"`
	Type         string `json:"type"`
}

// apiKeyResponse mirrors ApiKeyWithSecret from the API.
type apiKeyResponse struct {
	KeyID       string       `json:"key_id"`
	KeySecret   string       `json:"key_secret"`
	Encoded     string       `json:"encoded"`
	Name        string       `json:"name"`
	Username    string       `json:"username"`
	CreatedAt   string       `json:"created_at"`
	ExpiresAt   string       `json:"expires_at,omitempty"`
	Permissions []permission `json:"permissions,omitempty"`
}

// apiKeyListEntry mirrors ApiKey (no secret) from the API.
type apiKeyListEntry struct {
	KeyID       string       `json:"key_id"`
	Name        string       `json:"name"`
	Username    string       `json:"username"`
	CreatedAt   string       `json:"created_at"`
	ExpiresAt   string       `json:"expires_at,omitempty"`
	Permissions []permission `json:"permissions,omitempty"`
}

// TestE2E_Auth verifies that authentication and authorization work correctly
// when EnableAuth is true.
func TestE2E_Auth(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping e2e test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	t.Log("Starting Antfly swarm with auth enabled...")

	swarm := startAntflySwarmWithOptions(t, ctx, SwarmOptions{
		DisableTermite: true,
		EnableAuth:     true,
	})
	defer swarm.Cleanup()

	baseURL := swarm.MetadataAPIURL + apiBasePath
	adminAuth := basicAuth(adminUsername, adminPassword)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		auth   http.Header
		status int
	}{
		{
			name:   "Unauthenticated GET /secrets",
			method: http.MethodGet,
			path:   "/secrets",
			status: http.StatusUnauthorized,
		},
		{
			name:   "Bad credentials GET /secrets",
			method: http.MethodGet,
			path:   "/secrets",
			auth:   basicAuth("wrong", "creds"),
			status: http.StatusUnauthorized,
		},
		{
			name:   "Admin GET /secrets",
			method: http.MethodGet,
			path:   "/secrets",
			auth:   adminAuth,
			status: http.StatusOK,
		},
		{
			name:   "Unauthenticated PUT /secrets/test.key",
			method: http.MethodPut,
			path:   "/secrets/test.key",
			body:   `{"value":"secret-value"}`,
			status: http.StatusUnauthorized,
		},
		{
			name:   "Admin PUT /secrets/test.key",
			method: http.MethodPut,
			path:   "/secrets/test.key",
			body:   `{"value":"secret-value"}`,
			auth:   adminAuth,
			status: http.StatusOK,
		},
		{
			name:   "Unauthenticated DELETE /secrets/test.key",
			method: http.MethodDelete,
			path:   "/secrets/test.key",
			status: http.StatusUnauthorized,
		},
		{
			name:   "Admin DELETE /secrets/test.key",
			method: http.MethodDelete,
			path:   "/secrets/test.key",
			auth:   adminAuth,
			status: http.StatusNoContent,
		},
		{
			name:   "Unauthenticated GET /tables",
			method: http.MethodGet,
			path:   "/tables",
			status: http.StatusUnauthorized,
		},
		{
			name:   "Admin GET /tables",
			method: http.MethodGet,
			path:   "/tables",
			auth:   adminAuth,
			status: http.StatusOK,
		},
		{
			name:   "Unauthenticated GET /status",
			method: http.MethodGet,
			path:   "/status",
			status: http.StatusUnauthorized,
		},
		{
			name:   "Admin GET /status",
			method: http.MethodGet,
			path:   "/status",
			auth:   adminAuth,
			status: http.StatusOK,
		},
	}

	for _, tc := range tests {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			doRequestExpectStatus(
				t,
				ctx,
				tc.method,
				baseURL+tc.path,
				tc.body,
				tc.auth,
				tc.status,
			)
		})
	}
}

// TestE2E_ApiKeys tests the full API key lifecycle.
func TestE2E_ApiKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping e2e test in short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), testTimeout)
	defer cancel()

	t.Log("Starting Antfly swarm with auth enabled...")

	swarm := startAntflySwarmWithOptions(t, ctx, SwarmOptions{
		DisableTermite: true,
		EnableAuth:     true,
	})
	defer swarm.Cleanup()

	baseURL := swarm.MetadataAPIURL + apiBasePath
	adminAuth := basicAuth(adminUsername, adminPassword)

	t.Log("Creating test user 'alice'")
	createUser(t, ctx, baseURL, "alice", "password123", adminAuth)

	grantPermission(t, ctx, baseURL, "alice", "*", "table", "read", adminAuth)
	grantPermission(t, ctx, baseURL, "alice", "orders", "table", "write", adminAuth)

	t.Log("Creating unscoped API key")
	key1 := createApiKey(t, ctx, baseURL, "alice", "full-access key", nil, adminAuth)

	require.NotEmpty(t, key1.KeyID)
	require.NotEmpty(t, key1.KeySecret)
	require.NotEmpty(t, key1.Encoded)

	assert.Equal(t, "alice", key1.Username)

	expectedEncoded := base64.StdEncoding.EncodeToString(
		[]byte(key1.KeyID + ":" + key1.KeySecret),
	)

	assert.Equal(t, expectedEncoded, key1.Encoded)

	t.Log("Authenticating with API key")
	doRequestExpectStatus(
		t,
		ctx,
		http.MethodGet,
		baseURL+"/status",
		"",
		apiKeyAuth(key1.Encoded),
		http.StatusOK,
	)

	t.Log("Authenticating with bearer token")
	doRequestExpectStatus(
		t,
		ctx,
		http.MethodGet,
		baseURL+"/status",
		"",
		bearerAuth(key1.Encoded),
		http.StatusOK,
	)

	keys := listApiKeys(t, ctx, baseURL, "alice", adminAuth)

	require.Len(t, keys, 1)

	assert.Equal(t, key1.KeyID, keys[0].KeyID)
	assert.Equal(t, "full-access key", keys[0].Name)

	t.Log("Creating scoped API key")

	readOnlyPerms := []permission{
		{
			Resource:     "orders",
			ResourceType: "table",
			Type:         "read",
		},
	}

	key2 := createApiKey(
		t,
		ctx,
		baseURL,
		"alice",
		"read-only key",
		readOnlyPerms,
		adminAuth,
	)

	require.NotEmpty(t, key2.KeyID)
	assert.Len(t, key2.Permissions, 1)

	escalatedPerms := []permission{
		{
			Resource:     "*",
			ResourceType: "*",
			Type:         "admin",
		},
	}

	t.Log("Verifying privilege escalation prevention")

	createApiKeyExpectError(
		t,
		ctx,
		baseURL,
		"alice",
		"escalated key",
		escalatedPerms,
		adminAuth,
		http.StatusForbidden,
	)

	t.Log("Verifying invalid credentials rejection")

	fakeEncoded := base64.StdEncoding.EncodeToString(
		[]byte("fakeid:fakesecret"),
	)

	doRequestExpectStatus(
		t,
		ctx,
		http.MethodGet,
		baseURL+"/status",
		"",
		apiKeyAuth(fakeEncoded),
		http.StatusUnauthorized,
	)

	t.Log("Deleting API key")

	deleteApiKey(t, ctx, baseURL, "alice", key1.KeyID, adminAuth)

	doRequestExpectStatus(
		t,
		ctx,
		http.MethodGet,
		baseURL+"/status",
		"",
		apiKeyAuth(key1.Encoded),
		http.StatusUnauthorized,
	)

	doRequestExpectStatus(
		t,
		ctx,
		http.MethodGet,
		baseURL+"/status",
		"",
		apiKeyAuth(key2.Encoded),
		http.StatusOK,
	)

	keys = listApiKeys(t, ctx, baseURL, "alice", adminAuth)

	require.Len(t, keys, 1)
	assert.Equal(t, key2.KeyID, keys[0].KeyID)

	deleteApiKey(t, ctx, baseURL, "alice", key2.KeyID, adminAuth)
}

// ---------- helpers ----------

func basicAuth(username, password string) http.Header {
	return authorizationHeader(
		"Basic " + base64.StdEncoding.EncodeToString(
			[]byte(username+":"+password),
		),
	)
}

func apiKeyAuth(encoded string) http.Header {
	return authorizationHeader("ApiKey " + encoded)
}

func bearerAuth(encoded string) http.Header {
	return authorizationHeader("Bearer " + encoded)
}

func authorizationHeader(value string) http.Header {
	return http.Header{
		"Authorization": []string{value},
	}
}

func doRequestExpectStatus(
	t *testing.T,
	ctx context.Context,
	method,
	url,
	body string,
	headers http.Header,
	wantStatus int,
) {
	t.Helper()

	req, err := newRequest(ctx, method, url, body, headers)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(
		t,
		wantStatus,
		resp.StatusCode,
		"%s %s expected status %d, got %d: %s",
		method,
		url,
		wantStatus,
		resp.StatusCode,
		string(respBody),
	)
}

func newRequest(
	ctx context.Context,
	method,
	url,
	body string,
	headers http.Header,
) (*http.Request, error) {
	var reader io.Reader

	if body != "" {
		reader = bytes.NewBufferString(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}

	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	for key, values := range headers {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}

	return req, nil
}

// ---------- API key helpers ----------

func createUser(
	t *testing.T,
	ctx context.Context,
	baseURL,
	username,
	password string,
	auth http.Header,
) {
	t.Helper()

	body := fmt.Sprintf(`{"password":"%s"}`, password)

	doRequestExpectStatus(
		t,
		ctx,
		http.MethodPost,
		baseURL+"/users/"+username,
		body,
		auth,
		http.StatusCreated,
	)
}

func grantPermission(
	t *testing.T,
	ctx context.Context,
	baseURL,
	username,
	resource,
	resourceType,
	permType string,
	auth http.Header,
) {
	t.Helper()

	body := fmt.Sprintf(
		`{"resource":"%s","resource_type":"%s","type":"%s"}`,
		resource,
		resourceType,
		permType,
	)

	doRequestExpectStatus(
		t,
		ctx,
		http.MethodPost,
		baseURL+"/users/"+username+"/permissions",
		body,
		auth,
		http.StatusCreated,
	)
}

func createApiKey(
	t *testing.T,
	ctx context.Context,
	baseURL,
	username,
	name string,
	permissions []permission,
	auth http.Header,
) apiKeyResponse {
	t.Helper()

	reqBody := map[string]any{
		"name": name,
	}

	if permissions != nil {
		reqBody["permissions"] = permissions
	}

	bodyBytes, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req, err := newRequest(
		ctx,
		http.MethodPost,
		baseURL+"/users/"+username+"/api-keys",
		string(bodyBytes),
		auth,
	)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(
		t,
		http.StatusCreated,
		resp.StatusCode,
		"POST /users/%s/api-keys expected 201, got %d: %s",
		username,
		resp.StatusCode,
		string(respBody),
	)

	var result apiKeyResponse

	require.NoError(t, json.Unmarshal(respBody, &result))

	return result
}

func createApiKeyExpectError(
	t *testing.T,
	ctx context.Context,
	baseURL,
	username,
	name string,
	permissions []permission,
	auth http.Header,
	wantStatus int,
) {
	t.Helper()

	reqBody := map[string]any{
		"name": name,
	}

	if permissions != nil {
		reqBody["permissions"] = permissions
	}

	bodyBytes, err := json.Marshal(reqBody)
	require.NoError(t, err)

	req, err := newRequest(
		ctx,
		http.MethodPost,
		baseURL+"/users/"+username+"/api-keys",
		string(bodyBytes),
		auth,
	)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	assert.Equal(
		t,
		wantStatus,
		resp.StatusCode,
		"POST /users/%s/api-keys expected status %d, got %d: %s",
		username,
		wantStatus,
		resp.StatusCode,
		string(respBody),
	)
}

func listApiKeys(
	t *testing.T,
	ctx context.Context,
	baseURL,
	username string,
	auth http.Header,
) []apiKeyListEntry {
	t.Helper()

	req, err := newRequest(
		ctx,
		http.MethodGet,
		baseURL+"/users/"+username+"/api-keys",
		"",
		auth,
	)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(
		t,
		http.StatusOK,
		resp.StatusCode,
		"GET /users/%s/api-keys expected 200, got %d: %s",
		username,
		resp.StatusCode,
		string(respBody),
	)

	var result []apiKeyListEntry

	require.NoError(t, json.Unmarshal(respBody, &result))

	return result
}

func deleteApiKey(
	t *testing.T,
	ctx context.Context,
	baseURL,
	username,
	keyID string,
	auth http.Header,
) {
	t.Helper()

	doRequestExpectStatus(
		t,
		ctx,
		http.MethodDelete,
		baseURL+"/users/"+username+"/api-keys/"+keyID,
		"",
		auth,
		http.StatusNoContent,
	)
}
