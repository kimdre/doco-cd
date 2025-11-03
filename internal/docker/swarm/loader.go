package swarm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/distribution/reference"
	"github.com/docker/cli/cli/command"
	"github.com/docker/cli/cli/compose/loader"
	"github.com/docker/cli/cli/compose/schema"
	"github.com/docker/cli/cli/compose/types"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"

	"github.com/kimdre/doco-cd/internal/docker/options"
)

// LoadComposefile parse the composefile specified in the cli and returns its Config and version.
func LoadComposefile(dockerCli command.Cli, opts options.Deploy, resolvedSecrets secrettypes.ResolvedSecrets, workingDir string) (*types.Config, error) {
	configDetails, err := GetConfigDetails(opts.Composefiles, dockerCli.In())
	if err != nil {
		return nil, err
	}

	optsFunc := func(opts *loader.Options) {
		opts.SkipInterpolation = false
	}

	configDetails.WorkingDir = workingDir

	if configDetails.Environment == nil {
		configDetails.Environment = map[string]string{}
	}

	// Set any environment variables passed in via .env file
	for k, v := range opts.Environment {
		configDetails.Environment[k] = v
	}

	// Inject external secrets into the environment for variable interpolation
	for k, v := range resolvedSecrets {
		configDetails.Environment[k] = v
	}

	dicts := getDictsFrom(configDetails.ConfigFiles)

	config, err := loader.Load(configDetails, optsFunc)
	if err != nil {
		var fpe *loader.ForbiddenPropertiesError
		if errors.As(err, &fpe) {
			// this error is intentionally formatted multi-line
			return nil, fmt.Errorf("compose file contains unsupported options:\n\n%s", propertyWarnings(fpe.Properties))
		}

		return nil, err
	}

	unsupportedProperties := loader.GetUnsupportedProperties(dicts...)
	if len(unsupportedProperties) > 0 {
		_, _ = fmt.Fprintf(dockerCli.Err(), "Ignoring unsupported options: %s\n\n",
			strings.Join(unsupportedProperties, ", "))
	}

	deprecatedProperties := loader.GetDeprecatedProperties(dicts...)
	if len(deprecatedProperties) > 0 {
		_, _ = fmt.Fprintf(dockerCli.Err(), "Ignoring deprecated options:\n\n%s\n\n",
			propertyWarnings(deprecatedProperties))
	}

	// Validate if each service has a valid image-reference.
	for _, svc := range config.Services {
		if svc.Image == "" {
			return nil, fmt.Errorf("invalid image reference for service %s: no image specified", svc.Name)
		}

		if _, err = reference.ParseAnyReference(svc.Image); err != nil {
			return nil, fmt.Errorf("%w: invalid image reference for service %s", err, svc.Name)
		}
	}

	return config, nil
}

func getDictsFrom(configFiles []types.ConfigFile) []map[string]any {
	var dicts []map[string]any

	for _, configFile := range configFiles {
		dicts = append(dicts, configFile.Config)
	}

	return dicts
}

func propertyWarnings(properties map[string]string) string {
	msgs := make([]string, 0, len(properties))
	for name, description := range properties {
		msgs = append(msgs, fmt.Sprintf("%s: %s", name, description))
	}

	sort.Strings(msgs)

	return strings.Join(msgs, "\n\n")
}

// GetConfigDetails parse the composefiles specified in the cli and returns their ConfigDetails.
func GetConfigDetails(composefiles []string, stdin io.Reader) (types.ConfigDetails, error) {
	var details types.ConfigDetails

	if len(composefiles) == 0 {
		return details, errors.New("no compose file specified")
	}

	if composefiles[0] == "-" && len(composefiles) == 1 {
		workingDir, err := os.Getwd()
		if err != nil {
			return details, err
		}

		details.WorkingDir = workingDir
	} else {
		absPath, err := filepath.Abs(composefiles[0])
		if err != nil {
			return details, err
		}

		details.WorkingDir = filepath.Dir(absPath)
	}

	var err error

	details.ConfigFiles, err = loadConfigFiles(composefiles, stdin)
	if err != nil {
		return details, err
	}
	// Take the first file version (2 files can't have different version)
	details.Version = schema.Version(details.ConfigFiles[0].Config)
	details.Environment, err = buildEnvironment(os.Environ())

	return details, err
}

func buildEnvironment(env []string) (map[string]string, error) {
	result := make(map[string]string, len(env))
	for _, s := range env {
		if runtime.GOOS == "windows" && len(s) > 0 {
			// cmd.exe can have special environment variables which names start with "=".
			// They are only there for MS-DOS compatibility and we should ignore them.
			// See TestBuildEnvironment for examples.
			//
			// https://ss64.com/nt/syntax-variables.html
			// https://devblogs.microsoft.com/oldnewthing/20100506-00/?p=14133
			// https://github.com/docker/cli/issues/4078
			if s[0] == '=' {
				continue
			}
		}

		k, v, ok := strings.Cut(s, "=")
		if !ok || k == "" {
			return result, fmt.Errorf("unexpected environment variable '%s'", s)
		}
		// value may be set, but empty if "s" is like "K=", not "K".
		result[k] = v
	}

	return result, nil
}

func loadConfigFiles(filenames []string, stdin io.Reader) ([]types.ConfigFile, error) {
	configFiles := make([]types.ConfigFile, 0, len(filenames))

	for _, filename := range filenames {
		configFile, err := loadConfigFile(filename, stdin)
		if err != nil {
			return configFiles, err
		}

		configFiles = append(configFiles, *configFile)
	}

	return configFiles, nil
}

func loadConfigFile(filename string, stdin io.Reader) (*types.ConfigFile, error) {
	var (
		bytes []byte
		err   error
	)

	if filename == "-" {
		bytes, err = io.ReadAll(stdin)
	} else {
		bytes, err = os.ReadFile(filename) // #nosec G304
	}

	if err != nil {
		return nil, err
	}

	config, err := loader.ParseYAML(bytes)
	if err != nil {
		return nil, err
	}

	return &types.ConfigFile{
		Filename: filename,
		Config:   config,
	}, nil
}
