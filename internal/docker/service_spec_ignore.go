package docker

import (
	"strconv"
	"strings"
)

const (
	// ServiceSpecFieldReplicas identifies replica count drift in service spec comparisons.
	ServiceSpecFieldReplicas = "replicas"
	// ServiceSpecFieldMode identifies deploy mode drift in service spec comparisons.
	ServiceSpecFieldMode = "mode"
)

var supportedServiceSpecFields = map[string]struct{}{
	ServiceSpecFieldReplicas: {},
	ServiceSpecFieldMode:     {},
}

func normalizeServiceSpecField(field string) string {
	return strings.ToLower(strings.TrimSpace(field))
}

func isSupportedServiceSpecField(field string) bool {
	_, ok := supportedServiceSpecFields[normalizeServiceSpecField(field)]
	return ok
}

func allSupportedServiceSpecFieldsSet() map[string]struct{} {
	ret := make(map[string]struct{}, len(supportedServiceSpecFields))
	for field := range supportedServiceSpecFields {
		ret[field] = struct{}{}
	}

	return ret
}

func hasServiceSpecIgnoreByLabels(labels map[string]string) bool {
	return len(serviceSpecIgnoreFieldsByLabels(labels)) > 0
}

// HasServiceSpecIgnoreByLabels reports whether a service label set opts out of any supported spec drift checks.
func HasServiceSpecIgnoreByLabels(labels map[string]string) bool {
	return hasServiceSpecIgnoreByLabels(labels)
}

// IsServiceSpecFieldIgnoredByLabels reports whether a specific supported spec field should be ignored for drift checks.
func IsServiceSpecFieldIgnoredByLabels(labels map[string]string, field string) bool {
	field = normalizeServiceSpecField(field)
	if !isSupportedServiceSpecField(field) {
		return false
	}

	_, ok := serviceSpecIgnoreFieldsByLabels(labels)[field]

	return ok
}

func serviceSpecIgnoreFieldsByLabels(labels map[string]string) map[string]struct{} {
	if len(labels) == 0 {
		return nil
	}

	if externallyManaged, ok := labels[DocoCDLabels.Service.ExternallyManaged]; ok {
		if parsed, err := strconv.ParseBool(strings.TrimSpace(externallyManaged)); err == nil && parsed {
			return allSupportedServiceSpecFieldsSet()
		}
	}

	rawIgnoreCfg, ok := labels[DocoCDLabels.Deployment.RecreateIgnore]
	if !ok {
		return nil
	}

	ignoreCfg, err := parseRecreateIgnore(strings.TrimSpace(rawIgnoreCfg))
	if err != nil {
		return nil
	}

	specFields, ok := ignoreCfg[changeScopeSpec]
	if !ok {
		return nil
	}

	if len(specFields) == 0 {
		return allSupportedServiceSpecFieldsSet()
	}

	ret := make(map[string]struct{}, len(specFields))
	for _, field := range specFields {
		normalized := normalizeServiceSpecField(field)
		if isSupportedServiceSpecField(normalized) {
			ret[normalized] = struct{}{}
		}
	}

	return ret
}
