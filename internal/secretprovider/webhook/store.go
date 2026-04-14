package webhook

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"text/template"
)

const StoreVersionV1 = "v1"

var remoteRefFieldPattern = regexp.MustCompile(`\.remoteRef\.([A-Za-z0-9_]+)`) // e.g. {{ .remoteRef.key }}

// Store defines one reusable webhook secret-store entry.
type Store struct {
	Name     string            `yaml:"name"`
	Version  string            `yaml:"version"`
	URL      string            `yaml:"url"`
	Method   string            `yaml:"method"`
	Headers  map[string]string `yaml:"headers"`
	Body     string            `yaml:"body"`
	JSONPath string            `yaml:"jsonPath"`

	urlTemplate      *template.Template            `yaml:"-"`
	bodyTemplate     *template.Template            `yaml:"-"`
	headerTemplates  map[string]*template.Template `yaml:"-"`
	jsonPathTemplate *template.Template            `yaml:"-"`
	requiredFields   map[string]struct{}           `yaml:"-"`
}

func (s *Store) validateAndPrepare(funcMap template.FuncMap) error {
	if s.Name == "" {
		return fmt.Errorf("store name is required")
	}

	if s.Version == "" {
		return fmt.Errorf("store %q: version is required", s.Name)
	}

	if s.Version != StoreVersionV1 {
		return fmt.Errorf("store %q: unsupported version %q", s.Name, s.Version)
	}

	if s.URL == "" {
		return fmt.Errorf("store %q: url is required", s.Name)
	}

	if s.JSONPath == "" {
		return fmt.Errorf("store %q: jsonPath is required", s.Name)
	}

	if s.Method == "" {
		s.Method = "GET"
	}

	s.Method = strings.ToUpper(s.Method)

	switch s.Method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
		// valid
	default:
		return fmt.Errorf("store %q: unsupported http method %q", s.Name, s.Method)
	}

	if (s.Method == "GET" || s.Method == "DELETE") && s.Body != "" {
		return fmt.Errorf("store %q: http method %q must not define body", s.Name, s.Method)
	}

	s.requiredFields = make(map[string]struct{})
	s.headerTemplates = make(map[string]*template.Template, len(s.Headers))

	var err error

	s.urlTemplate, err = template.New(s.Name + "-url").Funcs(funcMap).Option("missingkey=error").Parse(s.URL)
	if err != nil {
		return fmt.Errorf("store %q: failed to parse url template: %w", s.Name, err)
	}

	s.collectRequiredFields(s.URL)

	if s.Body != "" {
		s.bodyTemplate, err = template.New(s.Name + "-body").Funcs(funcMap).Option("missingkey=error").Parse(s.Body)
		if err != nil {
			return fmt.Errorf("store %q: failed to parse body template: %w", s.Name, err)
		}

		s.collectRequiredFields(s.Body)
	}

	for key, value := range s.Headers {
		tpl, tplErr := template.New(s.Name + "-header-" + key).Funcs(funcMap).Option("missingkey=error").Parse(value)
		if tplErr != nil {
			return fmt.Errorf("store %q: failed to parse header template for %q: %w", s.Name, key, tplErr)
		}

		s.headerTemplates[key] = tpl
		s.collectRequiredFields(value)
	}

	s.jsonPathTemplate, err = template.New(s.Name + "-jsonpath").Funcs(funcMap).Option("missingkey=error").Parse(s.JSONPath)
	if err != nil {
		return fmt.Errorf("store %q: failed to parse jsonPath template: %w", s.Name, err)
	}

	s.collectRequiredFields(s.JSONPath)

	return nil
}

func (s *Store) collectRequiredFields(input string) {
	for _, match := range remoteRefFieldPattern.FindAllStringSubmatch(input, -1) {
		if len(match) == 2 {
			s.requiredFields[match[1]] = struct{}{}
		}
	}
}

func (s *Store) requiredRemoteRefFields() []string {
	result := make([]string, 0, len(s.requiredFields))
	for field := range s.requiredFields {
		result = append(result, field)
	}

	sort.Strings(result)

	return result
}
