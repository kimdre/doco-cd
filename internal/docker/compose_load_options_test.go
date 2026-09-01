package docker

import (
	"reflect"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"

	"github.com/kimdre/doco-cd/internal/config/app"
)

func TestNewComposeLoadOptions(t *testing.T) {
	t.Parallel()

	config := &app.Config{
		PassEnv:                    true,
		SkipTLSVerification:        true,
		HttpProxy:                   transport.ProxyOptions{URL: "https://proxy.example.com"},
		GitCloneSubmodules:         true,
		GitCloneDepth:              5,
		SSHPrivateKey:              "private-key",
		SSHPrivateKeyPassphrase:    "passphrase",
		GitAccessToken:             "access-token",
		OciInsecureRegistries:      []string{"registry.example.com"},
		DataHostPath:               "/host/data",
		DataMountPath:              "/data",
		DeployConfigBaseDir:        "/deploy",
		InterpolateExternalSecrets: true,
		DockerSwarmConfigRetention: 3,
		DockerSwarmSecretRetention: 4,
	}

	want := ComposeLoadOptions{
		PassEnv:                 true,
		SkipTLSVerify:           true,
		HttpProxy:               transport.ProxyOptions{URL: "https://proxy.example.com"},
		GitCloneSubmodules:      true,
		GitCloneDepth:           5,
		SSHPrivateKey:           "private-key",
		SSHPrivateKeyPassphrase: "passphrase",
		GitAccessToken:          "access-token",
		OciInsecureRegistries:   []string{"registry.example.com"},
		DataHostPath:            "/host/data",
		DataMountPath:           "/data",
	}

	if got := NewComposeLoadOptions(config); !reflect.DeepEqual(got, want) {
		t.Fatalf("NewComposeLoadOptions() = %+v, want %+v", got, want)
	}
}

func TestNewScheduledComposeOptions(t *testing.T) {
	t.Parallel()

	config := &app.Config{
		PassEnv:                    true,
		DeployConfigBaseDir:        "/deploy",
		InterpolateExternalSecrets: true,
	}

	want := ScheduledComposeOptions{
		ComposeLoad:                ComposeLoadOptions{PassEnv: true},
		DeployConfigBaseDir:        "/deploy",
		InterpolateExternalSecrets: true,
	}

	if got := NewScheduledComposeOptions(config); !reflect.DeepEqual(got, want) {
		t.Fatalf("NewScheduledComposeOptions() = %+v, want %+v", got, want)
	}
}

func TestNewCertificateRotationOptions(t *testing.T) {
	t.Parallel()

	config := &app.Config{
		DeployConfigBaseDir:        "/deploy",
		DockerSwarmConfigRetention: 3,
		DockerSwarmSecretRetention: 4,
	}

	want := CertificateRotationOptions{
		Scheduled: ScheduledComposeOptions{
			DeployConfigBaseDir: "/deploy",
		},
		DockerSwarmConfigRetention: 3,
		DockerSwarmSecretRetention: 4,
	}

	if got := NewCertificateRotationOptions(config); !reflect.DeepEqual(got, want) {
		t.Fatalf("NewCertificateRotationOptions() = %+v, want %+v", got, want)
	}
}

func TestRuntimeOptionsConstructorsAllowNilConfig(t *testing.T) {
	t.Parallel()

	if got := NewComposeLoadOptions(nil); !reflect.DeepEqual(got, ComposeLoadOptions{}) {
		t.Fatalf("NewComposeLoadOptions(nil) = %+v, want zero value", got)
	}

	if got := NewScheduledComposeOptions(nil); !reflect.DeepEqual(got, ScheduledComposeOptions{}) {
		t.Fatalf("NewScheduledComposeOptions(nil) = %+v, want zero value", got)
	}

	if got := NewCertificateRotationOptions(nil); !reflect.DeepEqual(got, CertificateRotationOptions{}) {
		t.Fatalf("NewCertificateRotationOptions(nil) = %+v, want zero value", got)
	}
}
