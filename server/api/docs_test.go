package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// The OpenAPI document is hand-written next to the router, so nothing but a
// test keeps the two honest. These check what a generated client (a ChatGPT
// Action above all) depends on: every route is described, nothing is
// described that does not exist, the document is well-formed, its $refs
// resolve, and its servers entry names the deployment the document was
// fetched from.

// openAPIPaths is the served document, decoded far enough to compare with
// the router.
type openAPIPaths struct {
	OpenAPI string `json:"openapi"`
	Servers []struct {
		URL string `json:"url"`
	} `json:"servers"`
	Paths map[string]map[string]struct {
		OperationID string `json:"operationId"`
		Summary     string `json:"summary"`
		// Absent means "inherit the document's security"; an empty array
		// means the operation is open.
		Security   *[]map[string][]string     `json:"security"`
		Parameters []openAPIParam             `json:"parameters"`
		Responses  map[string]json.RawMessage `json:"responses"`
	} `json:"paths"`
	Components struct {
		Schemas    map[string]json.RawMessage `json:"schemas"`
		Parameters map[string]openAPIParam    `json:"parameters"`
		Responses  map[string]json.RawMessage `json:"responses"`
	} `json:"components"`
}

// openAPIParam is one parameter of an operation: inline, or a $ref into
// components.parameters, which fetchOpenAPI resolves in place.
type openAPIParam struct {
	Ref      string `json:"$ref"`
	Name     string `json:"name"`
	In       string `json:"in"`
	Required bool   `json:"required"`
}

// fetchOpenAPI serves GET /openapi.json and decodes it.
func fetchOpenAPI(t *testing.T, srv *Server) (openAPIPaths, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	body := rec.Body.Bytes()
	var doc openAPIPaths
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("the served OpenAPI document is not valid JSON: %v", err)
	}
	// Shared parameters (user, start, end, ...) are $refs; resolve them so
	// every check below sees the parameter itself.
	for path, operations := range doc.Paths {
		for method, operation := range operations {
			for i, param := range operation.Parameters {
				if param.Ref == "" {
					continue
				}
				name, ok := strings.CutPrefix(param.Ref, "#/components/parameters/")
				resolved, found := doc.Components.Parameters[name]
				if !ok || !found {
					t.Fatalf("%s %s: parameter $ref %q does not resolve", method, path, param.Ref)
				}
				operation.Parameters[i] = resolved
			}
			operations[method] = operation
		}
	}
	return doc, body
}

func TestOpenAPIDescribesTheRouter(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{})
	doc, _ := fetchOpenAPI(t, srv)

	if !strings.HasPrefix(doc.OpenAPI, "3.1") {
		t.Errorf("openapi = %q, want a 3.1 document", doc.OpenAPI)
	}

	routes := srv.apiRoutes()
	described := make(map[string]bool, len(doc.Paths))
	for path := range doc.Paths {
		described[path] = false
	}

	for _, rt := range routes {
		operations, ok := doc.Paths[rt.path]
		if !ok {
			t.Errorf("the router serves %s but the OpenAPI document does not describe it", rt.path)
			continue
		}
		described[rt.path] = true

		// Every route is a GET, and nothing but GET is documented.
		operation, ok := operations["get"]
		if !ok {
			t.Errorf("%s: the document has no get operation", rt.path)
			continue
		}
		for method := range operations {
			if method != "get" {
				t.Errorf("%s: the document describes a %s operation the router does not serve", rt.path, method)
			}
		}

		// A generated client names its method after the operationId, so
		// every operation needs one and no two may collide.
		if operation.OperationID == "" {
			t.Errorf("%s: no operationId; a generated client has nothing to name the call", rt.path)
		}
		if operation.Summary == "" {
			t.Errorf("%s: no summary", rt.path)
		}

		// The token requirement in the document has to match the router's:
		// an open endpoint carries "security": [], an authenticated one
		// inherits the document's bearerAuth.
		switch {
		case rt.auth && operation.Security != nil:
			t.Errorf("%s: the router requires the bearer token but the document overrides security", rt.path)
		case !rt.auth && (operation.Security == nil || len(*operation.Security) != 0):
			t.Errorf("%s: the router serves this without a token but the document does not carry \"security\": []", rt.path)
		}

		// Every {placeholder} in the path is a declared, required path
		// parameter, and nothing else is declared as one.
		wanted := pathPlaceholders(rt.path)
		got := map[string]bool{}
		for _, param := range operation.Parameters {
			if param.In != "path" {
				continue
			}
			if !param.Required {
				t.Errorf("%s: path parameter %q must be required", rt.path, param.Name)
			}
			got[param.Name] = true
			if !wanted[param.Name] {
				t.Errorf("%s: declares path parameter %q that the route does not take", rt.path, param.Name)
			}
		}
		for name := range wanted {
			if !got[name] {
				t.Errorf("%s: path parameter %q is not declared in the document", rt.path, name)
			}
		}
	}

	var undescribed []string
	for path, matched := range described {
		if !matched {
			undescribed = append(undescribed, path)
		}
	}
	sort.Strings(undescribed)
	if len(undescribed) > 0 {
		t.Errorf("the OpenAPI document describes %v, which the router does not serve", undescribed)
	}
}

// pathPlaceholders collects the {name} segments of an OpenAPI path.
func pathPlaceholders(path string) map[string]bool {
	out := map[string]bool{}
	for _, segment := range strings.Split(path, "/") {
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "}") {
			out[segment[1:len(segment)-1]] = true
		}
	}
	return out
}

// A $ref that names a component the document does not define breaks every
// importer, and a schema, parameter or response nothing refers to is dead
// weight the next editor will trust.
func TestOpenAPIRefsResolve(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{})
	doc, body := fetchOpenAPI(t, srv)

	var tree any
	if err := json.Unmarshal(body, &tree); err != nil {
		t.Fatalf("decode: %v", err)
	}
	referenced := map[string]bool{}
	collectRefs(tree, referenced)

	defined := map[string]bool{}
	for name := range doc.Components.Schemas {
		defined["#/components/schemas/"+name] = true
	}
	for name := range doc.Components.Parameters {
		defined["#/components/parameters/"+name] = true
	}
	for name := range doc.Components.Responses {
		defined["#/components/responses/"+name] = true
	}

	for ref := range referenced {
		if !defined[ref] {
			t.Errorf("$ref %q names nothing the document's components define", ref)
		}
	}
	for ref := range defined {
		if !referenced[ref] {
			t.Errorf("%s is defined but nothing refers to it", strings.TrimPrefix(ref, "#/"))
		}
	}
}

// collectRefs walks a decoded JSON document and records every $ref string.
func collectRefs(node any, out map[string]bool) {
	switch v := node.(type) {
	case map[string]any:
		for key, value := range v {
			if key == "$ref" {
				if ref, ok := value.(string); ok {
					out[ref] = true
					continue
				}
			}
			collectRefs(value, out)
		}
	case []any:
		for _, item := range v {
			collectRefs(item, out)
		}
	}
}

// The HTML reference and the OpenAPI document are two views of the same API:
// the page must mention every route.
func TestDocsPageListsEveryRoute(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{})
	req := httptest.NewRequest(http.MethodGet, "/docs", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	page := rec.Body.String()
	for _, rt := range srv.apiRoutes() {
		if rt.path == "/" {
			continue // the index is the page's own base, not a listed route
		}
		if !strings.Contains(page, rt.path) {
			t.Errorf("the docs page does not mention %s", rt.path)
		}
	}
}

// servers[0].url must be a real, absolute URL: ChatGPT Actions and most
// generators refuse a document whose server is a placeholder, and the
// deployment cannot know its own public name until a request arrives.
func TestOpenAPIServerURLFollowsTheRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		host       string
		headers    map[string]string
		trustProxy bool
		want       string
	}{
		{"plain host", "puls.example.com", nil, false, "http://puls.example.com"},
		{
			"behind a trusted TLS proxy",
			"127.0.0.1:8081",
			map[string]string{"X-Forwarded-Host": "puls.example.com", "X-Forwarded-Proto": "https"},
			true,
			"https://puls.example.com",
		},
		{
			"a trusted proxy chain names the client's host first",
			"127.0.0.1:8081",
			map[string]string{"X-Forwarded-Host": "puls.example.com, inner", "X-Forwarded-Proto": "https, http"},
			true,
			"https://puls.example.com",
		},
		{
			"a host that is not an authority falls back to the relative server",
			"127.0.0.1:8081",
			map[string]string{"X-Forwarded-Host": `evil", "x": "`},
			true,
			"/",
		},
		// /openapi.json is unauthenticated, so without TRUST_PROXY_HEADERS any
		// caller could choose the host the document advertises — and docs/ai.md
		// tells people to hand that document to ChatGPT alongside the API
		// token. Untrusted, the header is ignored entirely.
		{
			"an untrusted X-Forwarded-Host is ignored",
			"127.0.0.1:8081",
			map[string]string{"X-Forwarded-Host": "attacker.example.com", "X-Forwarded-Proto": "https"},
			false,
			"http://127.0.0.1:8081",
		},
		{
			"an untrusted proxy cannot upgrade the scheme either",
			"puls.example.com",
			map[string]string{"X-Forwarded-Proto": "https"},
			false,
			"http://puls.example.com",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := testServer(t, &fakeStore{})
			srv.trustProxyHeaders = tc.trustProxy
			req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
			req.Host = tc.host
			for name, value := range tc.headers {
				req.Header.Set(name, value)
			}
			rec := httptest.NewRecorder()
			srv.routes().ServeHTTP(rec, req)

			var doc openAPIPaths
			if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
				t.Fatalf("the served document is not valid JSON: %v\n%s", err, rec.Body.String())
			}
			if len(doc.Servers) != 1 || doc.Servers[0].URL != tc.want {
				t.Fatalf("servers = %+v, want one entry with url %q", doc.Servers, tc.want)
			}
		})
	}

	// The placeholder never reaches a client.
	srv := testServer(t, &fakeStore{})
	req := httptest.NewRequest(http.MethodGet, "/openapi.json", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "{{origin}}") {
		t.Error("the served document still carries the {{origin}} placeholder")
	}
}

// The stored document has to keep exactly one placeholder for the
// substitution to be total; a second copy would silently stay unreplaced.
func TestStoredOpenAPIHasExactlyOneOriginPlaceholder(t *testing.T) {
	t.Parallel()

	if n := strings.Count(productAPIOpenAPIJSON, openAPIOriginPlaceholder); n != 1 {
		t.Fatalf("the stored document carries %d %s placeholders, want exactly 1", n, openAPIOriginPlaceholder)
	}
	if got := openAPIDocument("https://example.test"); !strings.Contains(got, `"url": "https://example.test"`) {
		t.Fatalf("openAPIDocument did not fill in the server url:\n%s", firstLines(got, 12))
	}
}

func firstLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return fmt.Sprint(strings.Join(lines, "\n"))
}

// Every authenticated operation can answer 401 (wrong token), 429 (too many
// wrong tokens) and 500, and every /v1 route can answer 400 and 403 (the
// user parameter); a reference that leaves them out tells a client those
// statuses never happen.
func TestOpenAPIDocumentsTheErrorResponses(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{})
	_, body := fetchOpenAPI(t, srv)
	var doc struct {
		Paths map[string]map[string]struct {
			Responses map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, rt := range srv.apiRoutes() {
		if !rt.auth {
			continue
		}
		responses := doc.Paths[rt.path]["get"].Responses
		for _, code := range []string{"200", "400", "401", "403", "429", "500"} {
			if _, ok := responses[code]; !ok {
				t.Errorf("%s: the document does not describe a %s response", rt.path, code)
			}
		}
	}
}

// Every status the middleware can answer an authenticated route with is in
// the document, so a generated client (a ChatGPT Action) knows a 429 means
// "wait", a 403 means "this user is not readable here", and a 504 means
// "narrow the range": auth's 401 and 429, scopeUser's 403 (?user= naming
// someone else with PULS_MULTI_USER off), and withTimeout's 504 on everything
// but the export. The open routes carry none of them.
func TestOpenAPIDocumentsTheMiddlewareStatuses(t *testing.T) {
	t.Parallel()

	srv := testServer(t, &fakeStore{})
	doc, _ := fetchOpenAPI(t, srv)
	for _, rt := range srv.apiRoutes() {
		responses := doc.Paths[rt.path]["get"].Responses
		if _, ok := responses["200"]; !ok {
			t.Errorf("%s: no 200 response", rt.path)
		}
		want := map[string]bool{"401": rt.auth, "403": rt.auth, "429": rt.auth, "504": rt.auth && rt.path != "/v1/export"}
		for status, documented := range want {
			if _, ok := responses[status]; ok != documented {
				t.Errorf("%s: documents %s = %v, want %v", rt.path, status, ok, documented)
			}
		}
	}

	// And the router really answers those statuses on an authenticated route.
	scoped := scopedServer(t, &fakeStore{}, defaultUserID)
	rec := serveAuthorized(t, scoped, http.MethodGet, "/v1/profile?user=7b1e4c2a-9d3f-4e5a-8b6c-0f1d2e3a4b5c", nil)
	if rec.Code != http.StatusForbidden {
		t.Errorf("?user= for someone else with the gate off = %d, want 403", rec.Code)
	}
	for i := 0; i <= authFailureBurst; i++ {
		req := httptest.NewRequest(http.MethodGet, "/v1/profile", nil)
		req.RemoteAddr = "192.0.2.7:1234"
		rec = httptest.NewRecorder()
		scoped.routes().ServeHTTP(rec, req)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("after %d failed authentications = %d, want 429", authFailureBurst+1, rec.Code)
	}
}
