package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/logger"
)

func TestOpenAPIScheduledJobsLastRunSchema(t *testing.T) {
	t.Parallel()

	h := &Handler{appConfig: &app.Config{}, log: logger.New(logger.LevelCritical)}
	builder := newSchemaBuilder()

	routes, err := createRouteCatalog(h, Mounts{}, builder)
	if err != nil {
		t.Fatal(err)
	}

	_, data, err := buildOpenAPIDocument(routes, builder.components)
	if err != nil {
		t.Fatal(err)
	}

	document, err := openapi3.NewLoader().LoadFromData(data)
	if err != nil {
		t.Fatal(err)
	}

	job := document.Components.Schemas["ScheduledJobsResponse"].Value.Properties["content"].Value.Items.Value
	for _, name := range []string{"name", "context", "stack", "mode", "enabled", "valid", "last_run_at", "next_run_at", "last_run"} {
		if job.Properties[name] == nil {
			t.Errorf("flat job schema missing %q", name)
		}
	}

	lastRun := job.Properties["last_run"]
	runRef := "#/components/schemas/" + componentTypeName(reflect.TypeFor[controlplane.Run]())

	if lastRun == nil || lastRun.Value == nil {
		t.Fatal("last_run schema is missing")
	}

	if lastRun.Value.Nullable {
		t.Fatal("last_run must not use the OpenAPI 3.0 nullable keyword in an OpenAPI 3.2 document")
	}

	if len(lastRun.Value.OneOf) != 2 || lastRun.Value.OneOf[0].Ref != runRef ||
		lastRun.Value.OneOf[1].Value == nil || !lastRun.Value.OneOf[1].Value.Type.Is(openapi3.TypeNull) {
		t.Fatalf("last_run must be one of the shared run reference %q or null: %#v", runRef, lastRun)
	}

	sharedRun := lastRun.Value.OneOf[0]

	detail := document.Components.Schemas["RunResponse"].Value.Properties["content"]
	if detail.Ref != runRef || !reflect.DeepEqual(sharedRun.Value.Properties, detail.Value.Properties) {
		t.Fatal("last_run must share the complete run-detail schema")
	}

	if !slices.Contains(job.Required, "last_run") {
		t.Fatal("last_run must be required, including when null")
	}

	if err := lastRun.Value.VisitJSON(nil); err != nil {
		t.Fatalf("last_run schema must accept null: %v", err)
	}

	if err := lastRun.Value.VisitJSON(map[string]any{
		"job_id":     "run-1",
		"trigger":    "scheduled_job",
		"status":     "succeeded",
		"created_at": "2026-10-10T12:00:00Z",
		"updated_at": "2026-10-10T12:00:10Z",
	}); err != nil {
		t.Fatalf("last_run schema must accept a run object: %v", err)
	}

	if !slices.Equal(sharedRun.Value.Properties["status"].Value.Enum, []any{"accepted", "running", "succeeded", "failed", "skipped"}) {
		t.Fatalf("run status enum = %#v", sharedRun.Value.Properties["status"].Value.Enum)
	}

	if !slices.Equal(sharedRun.Value.Properties["trigger"].Value.Enum, []any{"webhook", "poll", "scheduled_job", "mirror_compaction"}) {
		t.Fatalf("run trigger enum = %#v", sharedRun.Value.Properties["trigger"].Value.Enum)
	}

	for _, hidden := range []string{"LatestRunID", "latest_run_id", "last_run_id", "JobInfo"} {
		if job.Properties[hidden] != nil {
			t.Errorf("job schema leaked internal or redundant field %q", hidden)
		}
	}
}

func TestOpenAPIDocumentMatchesRouteCatalog(t *testing.T) {
	t.Parallel()

	handler := &Handler{
		appConfig: &app.Config{},
		log:       logger.New(logger.LevelCritical),
	}
	builder := newSchemaBuilder()

	routes, err := createRouteCatalog(handler, Mounts{}, builder)
	if err != nil {
		t.Fatalf("create route catalog: %v", err)
	}

	document, data, err := buildOpenAPIDocument(routes, builder.components)
	if err != nil {
		t.Fatalf("build OpenAPI document: %v", err)
	}

	if err := document.Validate(context.Background(), openapi3.IsOpenAPI32OrLater()); err != nil {
		t.Fatalf("validate OpenAPI document: %v", err)
	}

	if document.OpenAPI != "3.2.0" {
		t.Fatalf("OpenAPI version = %q, want 3.2.0", document.OpenAPI)
	}

	loaded, err := openapi3.NewLoader().LoadFromData(data)
	if err != nil {
		t.Fatalf("load marshaled OpenAPI document: %v", err)
	}

	if loaded.OpenAPI != "3.2.0" {
		t.Fatalf("marshaled OpenAPI version = %q, want 3.2.0", loaded.OpenAPI)
	}

	wantOperations := 0
	operationIDs := make(map[string]bool)

	for _, route := range routes {
		wantOperations += len(route.Operations)
		for _, metadata := range route.Operations {
			path := loaded.Paths.Find(route.Pattern)
			if path == nil {
				t.Errorf("OpenAPI path %q is missing", route.Pattern)
				continue
			}

			got := path.GetOperation(metadata.Method)
			if got == nil {
				t.Errorf("OpenAPI operation %s %s is missing", metadata.Method, route.Pattern)
				continue
			}

			if operationIDs[got.OperationID] {
				t.Errorf("operation ID %q is duplicated", got.OperationID)
			}

			operationIDs[got.OperationID] = true

			for _, name := range got.Tags {
				if tag := loaded.Tags.Get(name); tag == nil || tag.Description == "" {
					t.Errorf("tag %q of operation %q is not declared with a description", name, got.OperationID)
				}
			}
		}
	}

	gotOperations := 0
	for _, path := range loaded.Paths.Map() {
		gotOperations += len(path.Operations())
	}

	if gotOperations != wantOperations {
		t.Fatalf("OpenAPI operations = %d, want %d", gotOperations, wantOperations)
	}

	if loaded.Paths.Find(MCPPath) != nil {
		t.Fatal("MCP route must not be present in OpenAPI")
	}
}

func TestDocumentationVersion(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		version string
		want    string
	}{
		{version: "v0.115.0", want: "v0.115"},
		{version: "0.115.0", want: "0.115"},
		{version: "v1.2.3", want: "v1.2"},
		{version: "1.2.3", want: "1.2"},
		{version: "dev", want: "next"},
		{version: "dev-build", want: "next"},
	} {
		t.Run(testCase.version, func(t *testing.T) {
			t.Parallel()

			if got := documentationVersion(testCase.version); got != testCase.want {
				t.Fatalf("documentation version = %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestOpenAPIDocumentDescribesSecurityParametersAndSchemas(t *testing.T) {
	t.Parallel()

	handler := &Handler{
		appConfig: &app.Config{},
		log:       logger.New(logger.LevelCritical),
	}
	builder := newSchemaBuilder()

	routes, err := createRouteCatalog(handler, Mounts{}, builder)
	if err != nil {
		t.Fatalf("create route catalog: %v", err)
	}

	document, _, err := buildOpenAPIDocument(routes, builder.components)
	if err != nil {
		t.Fatalf("build OpenAPI document: %v", err)
	}

	runs := document.Paths.Find(APIPath + "/runs").Get
	if runs.Security == nil || len(*runs.Security) != 1 || (*runs.Security)[0][apiKeySecurityScheme] == nil {
		t.Fatal("deployment runs operation does not require the API key")
	}

	if parameterByName(runs.Parameters, "limit") == nil || parameterByName(runs.Parameters, "status") == nil {
		t.Fatal("deployment runs operation is missing filter parameters")
	}

	pollOperation := document.Paths.Find(APIPath + "/poll/run").Post
	if pollOperation.RequestBody == nil || pollOperation.RequestBody.Value == nil || pollOperation.Responses.Status(http.StatusRequestEntityTooLarge) == nil {
		t.Fatal("poll operation is missing its request body or payload-too-large response")
	}

	compactOperation := document.Paths.Find(APIPath + "/storage/compact").Post
	if compactOperation == nil || compactOperation.Responses.Status(http.StatusConflict) == nil {
		t.Fatal("storage compaction operation is missing or has no conflict response")
	}

	for _, name := range []string{"repository", "mode", "max_size", "wait"} {
		if parameterByName(compactOperation.Parameters, name) == nil {
			t.Errorf("storage compaction operation is missing parameter %q", name)
		}
	}

	if document.Tags.Get("Storage") == nil {
		t.Fatal("Storage tag is not declared")
	}

	targetedWebhook := document.Paths.Find(WebhookPath + "/{customTarget}").Post
	if targetedWebhook.RequestBody == nil || parameterByName(targetedWebhook.Parameters, "customTarget") == nil {
		t.Fatal("targeted webhook is missing its request body or path parameter")
	}

	if targetedWebhook.Security == nil || len(*targetedWebhook.Security) != 6 {
		t.Fatal("webhook security alternatives are missing")
	}

	for _, requirement := range *targetedWebhook.Security {
		if len(requirement) != 1 {
			t.Fatal("each webhook security alternative must contain exactly one authentication scheme")
		}
	}

	for _, header := range []string{
		"X-Hub-Signature-256",
		"X-Gitlab-Token",
		"X-Gitea-Signature",
		"X-Gogs-Signature",
		"X-Forgejo-Signature",
		"X-Doco-OCI-Signature-256",
	} {
		if parameterByName(targetedWebhook.Parameters, header) != nil {
			t.Errorf("webhook authentication header %q must be represented only as a security scheme", header)
		}
	}

	if parameterByName(targetedWebhook.Parameters, "X-GitHub-Event") == nil {
		t.Fatal("webhook operation is missing provider event headers")
	}

	webhookPayload := document.Components.Schemas["WebhookPayload"]
	if webhookPayload == nil || webhookPayload.Value == nil || len(webhookPayload.Value.OneOf) != 3 {
		t.Fatal("webhook payload must contain three provider payload alternatives")
	}

	for _, component := range []string{"ErrorResponse", "RunResponse", "ScheduledJobsResponse", "PollConfigs"} {
		if document.Components.Schemas[component] == nil {
			t.Errorf("component schema %q is missing", component)
		}
	}

	pollConfig := document.Components.Schemas[componentTypeName(reflect.TypeFor[poll.Config]())]
	if pollConfig == nil || pollConfig.Value == nil || !slices.Contains(pollConfig.Value.Required, "url") {
		t.Fatal("poll configuration schema must require url")
	}

	if interval := pollConfig.Value.Properties["interval"]; interval == nil || interval.Value == nil || len(interval.Value.OneOf) != 2 {
		t.Fatal("poll interval must support numeric seconds and duration strings")
	}
}

func TestRegisterRoutesServesPublicOpenAPIAndSwaggerUI(t *testing.T) {
	t.Parallel()

	handler := &Handler{
		appConfig: &app.Config{OpenAPIEnabled: true},
		log:       logger.New(logger.LevelCritical),
	}

	mux := http.NewServeMux()
	if _, err := RegisterRoutes(mux, handler, Mounts{}); err != nil {
		t.Fatalf("register routes: %v", err)
	}

	for _, testCase := range []struct {
		name                    string
		path                    string
		includedPath            string
		excludedPath            string
		wantSecuritySchemeCount int
		excludedSecurityScheme  string
		includedTag             string
		excludedTag             string
		expectedExternalDocsURL string
	}{
		{
			name:                    "REST",
			path:                    OpenAPIRestPath,
			includedPath:            APIPath + "/projects",
			excludedPath:            WebhookPath,
			wantSecuritySchemeCount: 1,
			excludedSecurityScheme:  "GitHubSignature",
			includedTag:             "Projects",
			excludedTag:             "Webhooks",
			expectedExternalDocsURL: DocsWebhookPath,
		},
		{
			name:                    "webhooks",
			path:                    OpenAPIWebhookPath,
			includedPath:            WebhookPath,
			excludedPath:            APIPath + "/projects",
			wantSecuritySchemeCount: 6,
			excludedSecurityScheme:  apiKeySecurityScheme,
			includedTag:             "Webhooks",
			excludedTag:             "Projects",
			expectedExternalDocsURL: DocsPath,
		},
	} {
		t.Run(testCase.name+" specification", func(t *testing.T) {
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, testCase.path, nil))

			if response.Code != http.StatusOK {
				t.Fatalf("OpenAPI status = %d, want %d", response.Code, http.StatusOK)
			}

			if got := response.Header().Get("Content-Type"); got != openAPIMediaType {
				t.Fatalf("OpenAPI content type = %q, want %q", got, openAPIMediaType)
			}

			document, err := openapi3.NewLoader().LoadFromData(response.Body.Bytes())
			if err != nil {
				t.Fatalf("parse served OpenAPI document: %v", err)
			}

			if document.Paths.Find(testCase.includedPath) == nil {
				t.Fatalf("OpenAPI document is missing %q", testCase.includedPath)
			}

			if document.Paths.Find(testCase.excludedPath) != nil {
				t.Fatalf("OpenAPI document must not include %q", testCase.excludedPath)
			}

			if len(document.Components.SecuritySchemes) != testCase.wantSecuritySchemeCount {
				t.Fatalf("security schemes = %d, want %d", len(document.Components.SecuritySchemes), testCase.wantSecuritySchemeCount)
			}

			if document.Components.SecuritySchemes[testCase.excludedSecurityScheme] != nil {
				t.Fatalf("OpenAPI document must not include security scheme %q", testCase.excludedSecurityScheme)
			}

			if !hasOpenAPITag(document.Tags, testCase.includedTag) {
				t.Fatalf("OpenAPI document is missing tag %q", testCase.includedTag)
			}

			if hasOpenAPITag(document.Tags, testCase.excludedTag) {
				t.Fatalf("OpenAPI document must not include tag %q", testCase.excludedTag)
			}

			if testCase.expectedExternalDocsURL != "" {
				if document.ExternalDocs == nil {
					t.Fatalf("OpenAPI document is missing ExternalDocs")
				}

				if document.ExternalDocs.URL != testCase.expectedExternalDocsURL {
					t.Fatalf("OpenAPI ExternalDocs URL = %q, want %q", document.ExternalDocs.URL, testCase.expectedExternalDocsURL)
				}
			}
		})
	}

	docsResponse := httptest.NewRecorder()
	mux.ServeHTTP(docsResponse, httptest.NewRequest(http.MethodGet, DocsPath, nil))

	if docsResponse.Code != http.StatusOK {
		t.Fatalf("documentation index status = %d, want %d", docsResponse.Code, http.StatusOK)
	}

	if !strings.Contains(docsResponse.Body.String(), OpenAPIRestPath) {
		t.Fatalf("REST Swagger UI does not reference %q", OpenAPIRestPath)
	}

	for uiPath, documentPath := range map[string]string{
		DocsPath:        OpenAPIRestPath,
		DocsWebhookPath: OpenAPIWebhookPath,
	} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, uiPath, nil))

		if response.Code != http.StatusOK {
			t.Errorf("%s status = %d, want %d", uiPath, response.Code, http.StatusOK)
		}

		if !strings.Contains(response.Body.String(), documentPath) {
			t.Errorf("%s does not reference %q", uiPath, documentPath)
		}
	}

	for _, documentPath := range []string{OpenAPIRestPath, OpenAPIWebhookPath} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodPost, documentPath, nil))

		if response.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST %s status = %d, want %d", documentPath, response.Code, http.StatusMethodNotAllowed)
		}
	}
}

func TestRegisterRoutesDoesNotServeOpenAPIWhenDisabled(t *testing.T) {
	t.Parallel()

	handler := &Handler{
		appConfig: &app.Config{},
		log:       logger.New(logger.LevelCritical),
	}

	mux := http.NewServeMux()
	if _, err := RegisterRoutes(mux, handler, Mounts{}); err != nil {
		t.Fatalf("register routes: %v", err)
	}

	for _, endpoint := range []string{OpenAPIRestPath, OpenAPIWebhookPath, DocsPath, DocsWebhookPath} {
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, endpoint, nil))

		if response.Code != http.StatusNotFound {
			t.Errorf("%s status = %d, want %d", endpoint, response.Code, http.StatusNotFound)
		}
	}
}

func TestRegisterRoutesReturnsMissingMountErrors(t *testing.T) {
	t.Parallel()

	for _, testCase := range []struct {
		name   string
		config app.Config
	}{
		{
			name:   "webhook",
			config: app.Config{WebhookSecret: "secret"}, // #nosec G101 -- test fixture.
		},
		{
			name: "MCP",
			config: app.Config{
				ApiSecret:  "secret", // #nosec G101 -- test fixture.
				McpEnabled: true,
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			handler := &Handler{
				appConfig: &testCase.config,
				log:       logger.New(logger.LevelCritical),
			}
			if _, err := RegisterRoutes(http.NewServeMux(), handler, Mounts{}); err == nil {
				t.Fatal("register routes succeeded without required mount")
			}
		})
	}
}

func parameterByName(parameters openapi3.Parameters, name string) *openapi3.Parameter {
	for _, parameter := range parameters {
		if parameter.Value != nil && parameter.Value.Name == name {
			return parameter.Value
		}
	}

	return nil
}

func hasOpenAPITag(tags openapi3.Tags, name string) bool {
	for _, tag := range tags {
		if tag.Name == name {
			return true
		}
	}

	return false
}
