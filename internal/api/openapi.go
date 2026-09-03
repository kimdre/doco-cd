package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3gen"

	"github.com/kimdre/doco-cd/internal/config/app"
	"github.com/kimdre/doco-cd/internal/config/poll"
	"github.com/kimdre/doco-cd/internal/controlplane"
	"github.com/kimdre/doco-cd/internal/scheduler"
	"github.com/kimdre/doco-cd/internal/webhook"
)

const (
	apiKeySecurityScheme = "ApiKey"
	openAPIMediaType     = "application/vnd.oai.openapi+json"
)

type successEnvelope[T any] struct {
	Content T      `json:"content,omitempty"`
	JobID   string `json:"job_id,omitempty"`
}

type errorEnvelope struct {
	Error   string `json:"error"`
	Content any    `json:"content,omitempty"`
	JobID   string `json:"job_id,omitempty"`
}

type composeProject struct {
	ID          string `json:"ID,omitempty"`
	Name        string `json:"Name"`
	Status      string `json:"Status"`
	ConfigFiles string `json:"ConfigFiles,omitempty"`
	Reason      string `json:"Reason,omitempty"`
}

type composeContainer struct {
	ID       string            `json:"ID"`
	Name     string            `json:"Name"`
	Image    string            `json:"Image"`
	Project  string            `json:"Project"`
	Service  string            `json:"Service"`
	State    string            `json:"State"`
	Status   string            `json:"Status"`
	Health   string            `json:"Health,omitempty"`
	ExitCode int               `json:"ExitCode"`
	Labels   map[string]string `json:"Labels,omitempty"`
	Networks []string          `json:"Networks,omitempty"`
}

type swarmService struct {
	ID   string `json:"ID"`
	Spec struct {
		Name   string            `json:"Name"`
		Labels map[string]string `json:"Labels,omitempty"`
	} `json:"Spec"`
}

type schemaBuilder struct {
	components *openapi3.Components
}

func newSchemaBuilder() *schemaBuilder {
	components := openapi3.NewComponents()
	components.Schemas = make(openapi3.Schemas)
	components.SecuritySchemes = make(openapi3.SecuritySchemes)

	return &schemaBuilder{
		components: &components,
	}
}

func schemaFor[T any](builder *schemaBuilder, name string) (*openapi3.SchemaRef, error) {
	if existing := builder.components.Schemas[name]; existing != nil {
		return openapi3.NewSchemaRef("#/components/schemas/"+name, existing.Value), nil
	}

	generator := openapi3gen.NewGenerator(
		openapi3gen.CreateComponentSchemas(openapi3gen.ExportComponentSchemasOptions{
			ExportComponentSchemas: true,
			ExportTopLevelSchema:   true,
			ExportGenerics:         true,
		}),
		openapi3gen.CreateTypeNameGenerator(componentTypeName),
	)

	schema, err := generator.GenerateSchemaRef(reflect.TypeFor[T]())
	if err != nil {
		return nil, fmt.Errorf("generate %s schema: %w", name, err)
	}

	for _, generatedSchema := range generator.Types {
		generatedName := strings.TrimPrefix(generatedSchema.Ref, "#/components/schemas/")
		if generatedName == generatedSchema.Ref {
			generatedSchema.Ref = ""
		} else if generatedSchema.Value != nil {
			builder.components.Schemas[generatedName] = &openapi3.SchemaRef{Value: generatedSchema.Value}
		}
	}

	if generatedName, ok := strings.CutPrefix(schema.Ref, "#/components/schemas/"); ok {
		generatedSchema := builder.components.Schemas[generatedName]
		if generatedSchema == nil || generatedSchema.Value == nil {
			return nil, fmt.Errorf("generated %s component schema is missing", name)
		}

		delete(builder.components.Schemas, generatedName)
		builder.components.Schemas[name] = generatedSchema
	} else {
		builder.components.Schemas[name] = schema
	}

	return openapi3.NewSchemaRef("#/components/schemas/"+name, builder.components.Schemas[name].Value), nil
}

func componentTypeName(t reflect.Type) string {
	name := t.Name()
	if name == "" {
		return ""
	}

	if packagePath := t.PkgPath(); packagePath != "" {
		name = packagePath + "." + name
	}

	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			return r
		default:
			return '_'
		}
	}, name)
}

func jsonResponseFor[T any](builder *schemaBuilder, name, description string) (*openapi3.ResponseRef, error) {
	schema, err := schemaFor[T](builder, name)
	if err != nil {
		return nil, err
	}

	return &openapi3.ResponseRef{
		Value: openapi3.NewResponse().
			WithDescription(description).
			WithJSONSchemaRef(schema),
	}, nil
}

func jsonRequestFor[T any](builder *schemaBuilder, name, description string) (*openapi3.RequestBodyRef, error) {
	schema, err := schemaFor[T](builder, name)
	if err != nil {
		return nil, err
	}

	return &openapi3.RequestBodyRef{
		Value: openapi3.NewRequestBody().
			WithDescription(description).
			WithRequired(true).
			WithJSONSchemaRef(schema),
	}, nil
}

func buildOpenAPIDocument(routes []Route, components *openapi3.Components) (*openapi3.T, []byte, error) {
	documentComponents := openapi3.NewComponents()
	documentComponents.Schemas = maps.Clone(components.Schemas)
	documentComponents.SecuritySchemes = make(openapi3.SecuritySchemes)
	addReferencedSecuritySchemes(&documentComponents, routes)

	document := &openapi3.T{
		OpenAPI: "3.2.0",
		Info: &openapi3.Info{
			Title:       "Doco-CD API Documentation",
			Version:     app.Version,
			Description: openAPIDescription(routes),
		},
		Paths:      openapi3.NewPaths(),
		Components: &documentComponents,
	}

	operationIDs := make(map[string]string)
	referencedTags := make(map[string]bool)

	for _, route := range routes {
		for i := range route.Operations {
			metadata := &route.Operations[i]
			if previous, ok := operationIDs[metadata.Operation.OperationID]; ok {
				return nil, nil, fmt.Errorf(
					"duplicate OpenAPI operation ID %q for %s and %s",
					metadata.Operation.OperationID,
					previous,
					route.Pattern,
				)
			}

			operationIDs[metadata.Operation.OperationID] = route.Pattern
			document.AddOperation(route.Pattern, metadata.Method, metadata.Operation)
			for _, tag := range metadata.Operation.Tags {
				referencedTags[tag] = true
			}
		}
	}

	for _, tag := range []string{"Health", "Runs", "Scheduled jobs", "Projects", "Stacks", "Polling", "Webhooks"} {
		if referencedTags[tag] {
			document.Tags = append(document.Tags, &openapi3.Tag{Name: tag})
		}
	}

	if err := document.Validate(context.Background(), openapi3.IsOpenAPI32OrLater()); err != nil {
		return nil, nil, fmt.Errorf("validate OpenAPI document: %w", err)
	}

	data, err := document.MarshalJSON()
	if err != nil {
		return nil, nil, fmt.Errorf("marshal OpenAPI document: %w", err)
	}

	return document, data, nil
}

func openAPIDescription(routes []Route) string {
	var hasREST, hasWebhooks bool
	for _, route := range routes {
		switch route.Root {
		case HealthPath, APIPath:
			hasREST = true
		case WebhookPath:
			hasWebhooks = true
		}
	}

	switch {
	case hasREST && !hasWebhooks:
		return `
## REST API

Authenticated routes require ` + "`API_SECRET` or `API_SECRET_FILE`" + ` being set in the doco-cd environment.

All supported routes are documented even when currently disabled.

[Open webhook API documentation](` + DocsWebhookPath + `).`
	case hasWebhooks && !hasREST:
		return `
## Webhook API

Authenticated routes require ` + "`WEBHOOK_SECRET` or `WEBHOOK_SECRET_FILE`" + ` being set in the doco-cd environment.

All supported routes are documented even when currently disabled.

[Open REST API documentation](` + DocsPath + `).`
	default:
		return `
## API

Authenticated REST routes require ` + "`API_SECRET` or `API_SECRET_FILE`" + ` being set in the doco-cd environment.

Webhook routes require ` + "`WEBHOOK_SECRET` or `WEBHOOK_SECRET_FILE`" + ` being set in the doco-cd environment.

All supported routes are documented even when disabled in the current process.`
	}
}

func addReferencedSecuritySchemes(components *openapi3.Components, routes []Route) {
	referenced := make(map[string]bool)
	for _, route := range routes {
		for _, metadata := range route.Operations {
			if metadata.Operation.Security == nil {
				continue
			}

			for _, requirement := range *metadata.Operation.Security {
				for name := range requirement {
					referenced[name] = true
				}
			}
		}
	}

	if referenced[apiKeySecurityScheme] {
		components.SecuritySchemes[apiKeySecurityScheme] = &openapi3.SecuritySchemeRef{
			Value: openapi3.NewSecurityScheme().
				WithType("apiKey").
				WithIn("header").
				WithName("x-api-key").
				WithDescription("REST API secret configured with API_SECRET or API_SECRET_FILE."),
		}
	}

	for name, header := range map[string]string{ // #nosec G101 -- these are HTTP header names, not credentials.
		"GitHubSignature":  "X-Hub-Signature-256",
		"GitLabToken":      "X-Gitlab-Token",
		"GiteaSignature":   "X-Gitea-Signature",
		"GogsSignature":    "X-Gogs-Signature",
		"ForgejoSignature": "X-Forgejo-Signature",
		"OCISignature":     "X-Doco-OCI-Signature-256",
	} {
		if !referenced[name] {
			continue
		}

		components.SecuritySchemes[name] = &openapi3.SecuritySchemeRef{
			Value: openapi3.NewSecurityScheme().
				WithType("apiKey").
				WithIn("header").
				WithName(header).
				WithDescription("Provider-specific webhook secret."),
		}
	}
}

func operation(method, id, summary string, tags []string, parameters openapi3.Parameters, request *openapi3.RequestBodyRef, responses *openapi3.Responses, security *openapi3.SecurityRequirements) Operation {
	return Operation{
		Method: method,
		Operation: &openapi3.Operation{
			OperationID: id,
			Summary:     summary,
			Description: summary + ".",
			Tags:        tags,
			Parameters:  parameters,
			RequestBody: request,
			Responses:   responses,
			Security:    security,
		},
	}
}

func responses(entries map[int]*openapi3.ResponseRef) *openapi3.Responses {
	result := openapi3.NewResponsesWithCapacity(len(entries))
	for status, response := range entries {
		result.Set(strconv.Itoa(status), response)
	}

	return result
}

func standardResponses(builder *schemaBuilder, success map[int]*openapi3.ResponseRef, statuses ...int) (map[int]*openapi3.ResponseRef, error) {
	result := make(map[int]*openapi3.ResponseRef, len(success)+len(statuses))
	maps.Copy(result, success)

	errorResponse, err := jsonResponseFor[errorEnvelope](builder, "ErrorResponse", "Error response.")
	if err != nil {
		return nil, err
	}

	for _, status := range statuses {
		result[status] = errorResponse
	}

	return result, nil
}

func apiSecurity() *openapi3.SecurityRequirements {
	security := openapi3.SecurityRequirements{
		openapi3.SecurityRequirement{apiKeySecurityScheme: {}},
	}

	return &security
}

func webhookSecurity() *openapi3.SecurityRequirements {
	security := openapi3.SecurityRequirements{
		{"GitHubSignature": {}},
		{"GitLabToken": {}},
		{"GiteaSignature": {}},
		{"GogsSignature": {}},
		{"ForgejoSignature": {}},
		{"OCISignature": {}},
	}

	return &security
}

func pathParameter(name, description string, values ...any) *openapi3.ParameterRef {
	schema := openapi3.NewStringSchema()
	if len(values) > 0 {
		schema.WithEnum(values...)
	}

	return &openapi3.ParameterRef{Value: openapi3.NewPathParameter(name).WithDescription(description).WithSchema(schema)}
}

func queryStringParameter(name, description string, values ...any) *openapi3.ParameterRef {
	schema := openapi3.NewStringSchema()
	if len(values) > 0 {
		schema.WithEnum(values...)
	}

	return &openapi3.ParameterRef{Value: openapi3.NewQueryParameter(name).WithDescription(description).WithSchema(schema)}
}

func queryBoolParameter(name, description string, defaultValue bool) *openapi3.ParameterRef {
	return &openapi3.ParameterRef{
		Value: openapi3.NewQueryParameter(name).
			WithDescription(description).
			WithSchema(openapi3.NewBoolSchema().WithDefault(defaultValue)),
	}
}

func queryIntParameter(name, description string, defaultValue int, minimum, maximum float64) *openapi3.ParameterRef {
	schema := openapi3.NewIntegerSchema().WithDefault(defaultValue).WithMin(minimum)
	if maximum > 0 {
		schema.WithMax(maximum)
	}

	return &openapi3.ParameterRef{
		Value: openapi3.NewQueryParameter(name).
			WithDescription(description).
			WithSchema(schema),
	}
}

func queryOptionalIntParameter(name, description string, minimum float64) *openapi3.ParameterRef {
	return &openapi3.ParameterRef{
		Value: openapi3.NewQueryParameter(name).
			WithDescription(description).
			WithSchema(openapi3.NewIntegerSchema().WithMin(minimum)),
	}
}

func contextParameter() *openapi3.ParameterRef {
	return queryStringParameter("context", "Docker context name. Defaults to the default Docker context.")
}

func webhookParameters(includeTarget bool) openapi3.Parameters {
	parameters := openapi3.Parameters{
		queryBoolParameter("wait", "Wait for deployment completion. Invalid values are treated as false.", false),
	}
	if includeTarget {
		parameters = append(parameters, pathParameter("customTarget", "Custom deployment configuration target."))
	}

	for _, header := range []string{
		"X-GitHub-Event",
		"X-Gitlab-Event",
		"X-Gitea-Event",
		"X-Gogs-Event",
		"X-Forgejo-Event",
		"X-Doco-OCI-Event",
	} {
		parameters = append(parameters, &openapi3.ParameterRef{
			Value: openapi3.NewHeaderParameter(header).
				WithDescription("Provider-specific webhook event header.").
				WithSchema(openapi3.NewStringSchema()),
		})
	}

	return parameters
}

func createRouteCatalog(h *Handler, mounts Mounts, builder *schemaBuilder) ([]Route, error) {
	stringResponse, err := jsonResponseFor[successEnvelope[string]](builder, "StringResponse", "Successful operation.")
	if err != nil {
		return nil, err
	}

	runResponse, err := jsonResponseFor[successEnvelope[controlplane.Run]](builder, "RunResponse", "Tracked deployment run.")
	if err != nil {
		return nil, err
	}

	runsResponse, err := jsonResponseFor[successEnvelope[[]controlplane.Run]](builder, "RunsResponse", "Tracked deployment runs.")
	if err != nil {
		return nil, err
	}

	jobsResponse, err := jsonResponseFor[successEnvelope[[]scheduler.JobInfo]](builder, "ScheduledJobsResponse", "Scheduled jobs.")
	if err != nil {
		return nil, err
	}

	projectsResponse, err := jsonResponseFor[successEnvelope[[]composeProject]](builder, "ComposeProjectsResponse", "Compose projects.")
	if err != nil {
		return nil, err
	}

	containersResponse, err := jsonResponseFor[successEnvelope[[]composeContainer]](builder, "ComposeContainersResponse", "Compose project containers.")
	if err != nil {
		return nil, err
	}

	stacksResponse, err := jsonResponseFor[successEnvelope[map[string][]swarmService]](builder, "SwarmStacksResponse", "Swarm stacks and services.")
	if err != nil {
		return nil, err
	}

	servicesResponse, err := jsonResponseFor[successEnvelope[[]swarmService]](builder, "SwarmServicesResponse", "Swarm stack services.")
	if err != nil {
		return nil, err
	}

	pollRequest, err := jsonRequestFor[[]poll.Config](builder, "PollConfigs", "Poll configurations to run.")
	if err != nil {
		return nil, err
	}

	if err := customizePollSchema(builder.components.Schemas[componentTypeName(reflect.TypeFor[poll.Config]())]); err != nil {
		return nil, err
	}

	if err := customizeRunSchema(builder.components.Schemas[componentTypeName(reflect.TypeFor[controlplane.Run]())]); err != nil {
		return nil, err
	}

	githubPayload, err := schemaFor[webhook.GithubPushPayload](builder, "GitHubPushPayload")
	if err != nil {
		return nil, err
	}

	gitlabPayload, err := schemaFor[webhook.GitlabPushPayload](builder, "GitLabPushPayload")
	if err != nil {
		return nil, err
	}

	ociPayload, err := schemaFor[webhook.OCIArtifactPayload](builder, "OCIArtifactPayload")
	if err != nil {
		return nil, err
	}

	webhookPayload := &openapi3.SchemaRef{
		Value: &openapi3.Schema{
			OneOf: openapi3.SchemaRefs{githubPayload, gitlabPayload, ociPayload},
		},
	}
	builder.components.Schemas["WebhookPayload"] = webhookPayload
	webhookRequest := &openapi3.RequestBodyRef{
		Value: openapi3.NewRequestBody().
			WithDescription("Git provider push payload or OCI artifact payload.").
			WithRequired(true).
			WithJSONSchemaRef(openapi3.NewSchemaRef("#/components/schemas/WebhookPayload", webhookPayload.Value)),
	}

	restEnabled := func(config *app.Config) bool { return config.ApiSecret != "" }
	webhookEnabled := func(config *app.Config) bool { return config.WebhookSecret != "" }
	mcpEnabled := func(config *app.Config) bool { return config.McpEnabled && config.ApiSecret != "" }
	alwaysEnabled := func(*app.Config) bool { return true }

	healthResponses, err := standardResponses(builder, map[int]*openapi3.ResponseRef{http.StatusOK: stringResponse}, http.StatusServiceUnavailable)
	if err != nil {
		return nil, err
	}

	listRunsResponses, err := standardResponses(builder, map[int]*openapi3.ResponseRef{http.StatusOK: runsResponse}, http.StatusBadRequest, http.StatusUnauthorized, http.StatusMethodNotAllowed)
	if err != nil {
		return nil, err
	}

	getRunResponses, err := standardResponses(builder, map[int]*openapi3.ResponseRef{http.StatusOK: runResponse}, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusMethodNotAllowed)
	if err != nil {
		return nil, err
	}

	listJobsResponses, err := standardResponses(builder, map[int]*openapi3.ResponseRef{http.StatusOK: jobsResponse}, http.StatusBadRequest, http.StatusUnauthorized, http.StatusInternalServerError, http.StatusMethodNotAllowed)
	if err != nil {
		return nil, err
	}

	triggerResponses, err := standardResponses(builder, map[int]*openapi3.ResponseRef{
		http.StatusOK:       stringResponse,
		http.StatusAccepted: stringResponse,
	}, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusConflict, http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusMethodNotAllowed)
	if err != nil {
		return nil, err
	}

	listProjectsResponses, err := standardResponses(builder, map[int]*openapi3.ResponseRef{http.StatusOK: projectsResponse}, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError, http.StatusMethodNotAllowed)
	if err != nil {
		return nil, err
	}

	getProjectResponses, err := standardResponses(builder, map[int]*openapi3.ResponseRef{http.StatusOK: containersResponse}, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError, http.StatusMethodNotAllowed)
	if err != nil {
		return nil, err
	}

	mutationResponses, err := standardResponses(builder, map[int]*openapi3.ResponseRef{http.StatusOK: stringResponse}, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError, http.StatusMethodNotAllowed)
	if err != nil {
		return nil, err
	}

	listStacksResponses, err := standardResponses(builder, map[int]*openapi3.ResponseRef{http.StatusOK: stacksResponse}, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError, http.StatusMethodNotAllowed)
	if err != nil {
		return nil, err
	}

	getStackResponses, err := standardResponses(builder, map[int]*openapi3.ResponseRef{http.StatusOK: servicesResponse}, http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound, http.StatusInternalServerError, http.StatusMethodNotAllowed)
	if err != nil {
		return nil, err
	}

	pollResponses, err := standardResponses(builder, map[int]*openapi3.ResponseRef{
		http.StatusOK:       stringResponse,
		http.StatusAccepted: stringResponse,
	}, http.StatusBadRequest, http.StatusUnauthorized, http.StatusRequestEntityTooLarge, http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusMethodNotAllowed)
	if err != nil {
		return nil, err
	}

	webhookResponses, err := standardResponses(builder, map[int]*openapi3.ResponseRef{
		http.StatusCreated:  stringResponse,
		http.StatusAccepted: stringResponse,
	}, http.StatusBadRequest, http.StatusUnauthorized, http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusMethodNotAllowed)
	if err != nil {
		return nil, err
	}

	apiAuth := apiSecurity()
	webhookAuth := webhookSecurity()
	timeout := queryIntParameter("timeout", "Operation timeout in seconds.", controlplane.DefaultProjectActionTimeout, 1, float64(controlplane.MaxProjectActionTimeout))

	return []Route{
		{
			Pattern: HealthPath,
			Handler: http.HandlerFunc(h.HealthCheckHandler),
			Enabled: alwaysEnabled,
			Root:    HealthPath,
			Operations: []Operation{
				operation(http.MethodGet, "getHealth", "Check application health", []string{"Health"}, nil, nil, responses(healthResponses), nil),
			},
		},
		{
			Pattern: APIPath + "/runs",
			Handler: http.HandlerFunc(h.GetDeploymentRunsHandler),
			Enabled: restEnabled,
			Root:    APIPath,
			Operations: []Operation{
				operation(http.MethodGet, "listDeploymentRuns", "List deployment runs", []string{"Runs"}, openapi3.Parameters{
					queryIntParameter("limit", "Maximum number of runs.", 50, 1, 200),
					queryStringParameter("status", "Run status filter.", "accepted", "running", "succeeded", "failed", "skipped"),
					queryStringParameter("trigger", "Run trigger filter.", "webhook", "poll", "scheduled_job"),
				}, nil, responses(listRunsResponses), apiAuth),
			},
		},
		{
			Pattern: APIPath + "/run/{jobID}",
			Handler: http.HandlerFunc(h.GetDeploymentRunHandler),
			Enabled: restEnabled,
			Root:    APIPath,
			Operations: []Operation{
				operation(http.MethodGet, "getDeploymentRun", "Get a deployment run", []string{"Runs"}, openapi3.Parameters{
					pathParameter("jobID", "Tracked job identifier."),
				}, nil, responses(getRunResponses), apiAuth),
			},
		},
		{
			Pattern: APIPath + "/jobs",
			Handler: http.HandlerFunc(h.GetScheduledJobsHandler),
			Enabled: restEnabled,
			Root:    APIPath,
			Operations: []Operation{
				operation(http.MethodGet, "listScheduledJobs", "List scheduled jobs", []string{"Scheduled jobs"}, openapi3.Parameters{
					contextParameter(),
					queryStringParameter("stack", "Optional stack or project filter."),
				}, nil, responses(listJobsResponses), apiAuth),
			},
		},
		{
			Pattern: APIPath + "/job/{jobName}/run",
			Handler: http.HandlerFunc(h.TriggerScheduledJobHandler),
			Enabled: restEnabled,
			Root:    APIPath,
			Operations: []Operation{
				operation(http.MethodPost, "triggerScheduledJob", "Trigger a scheduled job", []string{"Scheduled jobs"}, openapi3.Parameters{
					pathParameter("jobName", "Scheduled container or service name."),
					contextParameter(),
					queryStringParameter("stack", "Optional stack or project used to disambiguate the job."),
					queryBoolParameter("wait", "Wait for the job to finish.", true),
				}, nil, responses(triggerResponses), apiAuth),
			},
		},
		{
			Pattern: APIPath + "/projects",
			Handler: http.HandlerFunc(h.GetProjectsApiHandler),
			Enabled: restEnabled,
			Root:    APIPath,
			Operations: []Operation{
				operation(http.MethodGet, "listComposeProjects", "List Compose projects", []string{"Projects"}, openapi3.Parameters{
					contextParameter(),
					queryBoolParameter("all", "Include inactive projects.", false),
				}, nil, responses(listProjectsResponses), apiAuth),
			},
		},
		{
			Pattern: APIPath + "/project/{projectName}",
			Handler: http.HandlerFunc(h.ProjectApiHandler),
			Enabled: restEnabled,
			Root:    APIPath,
			Operations: []Operation{
				operation(http.MethodGet, "getComposeProject", "Get a Compose project", []string{"Projects"}, openapi3.Parameters{
					pathParameter("projectName", "Compose project name."),
					contextParameter(),
				}, nil, responses(getProjectResponses), apiAuth),
				operation(http.MethodDelete, "deleteComposeProject", "Remove a Compose project", []string{"Projects"}, openapi3.Parameters{
					pathParameter("projectName", "Compose project name."),
					contextParameter(),
					timeout,
					queryBoolParameter("volumes", "Remove associated volumes.", true),
					queryBoolParameter("images", "Remove associated images.", true),
				}, nil, responses(mutationResponses), apiAuth),
			},
		},
		{
			Pattern: APIPath + "/project/{projectName}/{action}",
			Handler: http.HandlerFunc(h.ProjectActionApiHandler),
			Enabled: restEnabled,
			Root:    APIPath,
			Operations: []Operation{
				operation(http.MethodPost, "runComposeProjectAction", "Run a Compose project action", []string{"Projects"}, openapi3.Parameters{
					pathParameter("projectName", "Compose project name."),
					pathParameter("action", "Lifecycle action.", "start", "stop", "restart"),
					contextParameter(),
					timeout,
				}, nil, responses(mutationResponses), apiAuth),
			},
		},
		{
			Pattern: APIPath + "/stacks",
			Handler: http.HandlerFunc(h.GetStacksApiHandler),
			Enabled: restEnabled,
			Root:    APIPath,
			Operations: []Operation{
				operation(http.MethodGet, "listSwarmStacks", "List Swarm stacks", []string{"Stacks"}, openapi3.Parameters{
					contextParameter(),
				}, nil, responses(listStacksResponses), apiAuth),
			},
		},
		{
			Pattern: APIPath + "/stack/{stackName}",
			Handler: http.HandlerFunc(h.StackApiHandler),
			Enabled: restEnabled,
			Root:    APIPath,
			Operations: []Operation{
				operation(http.MethodGet, "getSwarmStack", "Get a Swarm stack", []string{"Stacks"}, openapi3.Parameters{
					pathParameter("stackName", "Swarm stack name."),
					contextParameter(),
				}, nil, responses(getStackResponses), apiAuth),
				operation(http.MethodDelete, "deleteSwarmStack", "Remove a Swarm stack", []string{"Stacks"}, openapi3.Parameters{
					pathParameter("stackName", "Swarm stack name."),
					contextParameter(),
				}, nil, responses(mutationResponses), apiAuth),
			},
		},
		{
			Pattern: APIPath + "/stack/{stackName}/{action}",
			Handler: http.HandlerFunc(h.StackActionApiHandler),
			Enabled: restEnabled,
			Root:    APIPath,
			Operations: []Operation{
				operation(http.MethodPost, "runSwarmStackAction", "Run a Swarm stack action", []string{"Stacks"}, openapi3.Parameters{
					pathParameter("stackName", "Swarm stack name."),
					pathParameter("action", "Stack action.", "scale", "restart", "run"),
					contextParameter(),
					queryStringParameter("service", "Optional service name."),
					queryOptionalIntParameter("replicas", "Non-negative replica count. Required for the scale action.", 0),
					queryBoolParameter("wait", "Wait for services to become ready.", true),
				}, nil, responses(mutationResponses), apiAuth),
			},
		},
		{
			Pattern: APIPath + "/poll/run",
			Handler: http.HandlerFunc(h.TriggerPollHandler),
			Enabled: restEnabled,
			Root:    APIPath,
			Operations: []Operation{
				operation(http.MethodPost, "triggerPoll", "Trigger poll configurations", []string{"Polling"}, openapi3.Parameters{
					queryBoolParameter("wait", "Wait for polling deployments to finish.", true),
				}, pollRequest, responses(pollResponses), apiAuth),
			},
		},
		{
			Pattern:    "POST " + MCPPath,
			Handler:    mounts.MCP,
			Enabled:    mcpEnabled,
			Root:       MCPPath,
			Operations: nil,
		},
		{
			Pattern: WebhookPath,
			Handler: mounts.Webhook,
			Enabled: webhookEnabled,
			Root:    WebhookPath,
			Operations: []Operation{
				operation(http.MethodPost, "receiveWebhook", "Receive a deployment webhook", []string{"Webhooks"}, webhookParameters(false), webhookRequest, responses(webhookResponses), webhookAuth),
			},
		},
		{
			Pattern: WebhookPath + "/{customTarget}",
			Handler: mounts.Webhook,
			Enabled: webhookEnabled,
			Root:    WebhookPath,
			Operations: []Operation{
				operation(http.MethodPost, "receiveTargetedWebhook", "Receive a targeted deployment webhook", []string{"Webhooks"}, webhookParameters(true), webhookRequest, responses(webhookResponses), webhookAuth),
			},
		},
	}, nil
}

func customizePollSchema(schema *openapi3.SchemaRef) error {
	if schema == nil || schema.Value == nil {
		return errors.New("generated poll configuration schema is missing")
	}

	schema.Value.Required = append(schema.Value.Required, "url")
	sourceSchema := openapi3.NewStringSchema().WithEnum("git", "oci").WithDefault("git")
	sourceSchema.Description = "Source backend."
	schema.Value.Properties["source"] = &openapi3.SchemaRef{Value: sourceSchema}

	schema.Value.Properties["interval"] = &openapi3.SchemaRef{
		Value: &openapi3.Schema{
			Description: "Polling interval as whole seconds or a Go duration string; ignored for API-triggered runs.",
			OneOf: openapi3.SchemaRefs{
				{Value: openapi3.NewIntegerSchema().WithMin(0)},
				{Value: openapi3.NewStringSchema()},
			},
			Default: "180s",
		},
	}
	for name, defaultValue := range map[string]bool{"run_once": false, "watch": true} {
		property := schema.Value.Properties[name]
		if property == nil || property.Value == nil {
			return fmt.Errorf("generated poll configuration property %q is missing", name)
		}

		property.Value.Default = defaultValue
	}

	return nil
}

func customizeRunSchema(schema *openapi3.SchemaRef) error {
	if schema == nil || schema.Value == nil {
		return errors.New("generated deployment run schema is missing")
	}

	schema.Value.Properties["trigger"] = &openapi3.SchemaRef{
		Value: openapi3.NewStringSchema().WithEnum("webhook", "poll", "scheduled_job"),
	}
	schema.Value.Properties["status"] = &openapi3.SchemaRef{
		Value: openapi3.NewStringSchema().WithEnum("accepted", "running", "succeeded", "failed", "skipped"),
	}

	return nil
}
