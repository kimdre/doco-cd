package webhook

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"text/template"

	"github.com/creasty/defaults"
)

const StoreVersionV1 = "v1"

var remoteRefFieldPattern = regexp.MustCompile(`\.remote_ref\.([A-Za-z0-9_]+)`) // e.g. {{ .remote_ref.key }}

// Store defines one reusable webhook secret-store entry.
type Store struct {
	Name     string            `yaml:"name"`
	Version  string            `yaml:"version"  default:"v1"`
	URL      string            `yaml:"url"`
	Method   string            `yaml:"method"   default:"GET"`
	Headers  map[string]string `yaml:"headers"`
	Body     string            `yaml:"body"`
	JSONPath string            `yaml:"json_path"`

	urlTemplate      *template.Template            `yaml:"-"`
	bodyTemplate     *template.Template            `yaml:"-"`
	headerTemplates  map[string]*template.Template `yaml:"-"`
	jsonPathTemplate *template.Template            `yaml:"-"`
	requiredFields   map[string]struct{}           `yaml:"-"`
}


func (s *Store) validateAndPrepare(funcMap template.FuncMap) error {
	if err := defaults.Set(s); err != nil {
		return fmt.Errorf("store %q: failed to apply defaults: %w", s.Name, err)
	}

	if s.Name == "" {
		return errors.New("store name is required")
	}

	if s.Version != StoreVersionV1 {
		return fmt.Errorf("store %q: unsupported version %q", s.Name, s.Version)
	}

	if s.URL == "" {
		return fmt.Errorf("store %q: url is required", s.Name)
	}

	if s.JSONPath == "" {
		return fmt.Errorf("store %q: json_path is required", s.Name)
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
		return fmt.Errorf("store %q: failed to parse json_path template: %w", s.Name, err)
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
