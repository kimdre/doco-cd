package awssecretsmanager

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"

	secrettypes "github.com/kimdre/doco-cd/internal/secretprovider/types"
)

const Name = "aws_sm"

type Provider struct {
	client *secretsmanager.Client
}

func (p *Provider) Name() string {
	return Name
}

const (
	PathDelimiter = "/"
)

// NewProvider initializes a new AWS Secrets Manager provider with the given configuration.
func NewProvider(ctx context.Context, region, accessKeyID, secretAccessKey string) (*Provider, error) {
	cfg, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(
			aws.NewCredentialsCache(
				credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, ""),
			),
		),
	)
	if err != nil {
		return nil, err
	}

	return &Provider{client: secretsmanager.NewFromConfig(cfg)}, nil
}

// getPathFromARN extracts the path from the ARN if it exists and returns the base ARN and the path separately.
// Example ARN with path: arn:aws:secretsmanager:region:account-id:secret:secret-name/path.
func getPathFromARN(id string) (string, string) {
	// Find the last occurrence of ":secret:" to ensure we only split after the secret name
	secretPrefix := ":secret:"

	idx := strings.Index(id, secretPrefix)
	if idx == -1 {
		return id, ""
	}
	// Find the first "/" after ":secret:"
	slashIdx := strings.Index(id[idx+len(secretPrefix):], PathDelimiter)
	if slashIdx == -1 {
		return id, ""
	}
	// The base ARN is up to the slash, the path is after
	base := id[:idx+len(secretPrefix)+slashIdx]
	path := id[idx+len(secretPrefix)+slashIdx+1:]

	return base, path
}

// getSecretValueWithOptionalPath retrieves a secret value from AWS Secrets Manager.
// If the secret ID contains a path, it fetches the secret and extracts the value at the specified path.
func (p *Provider) getSecretValueWithOptionalPath(ctx context.Context, secretID string) (string, error) {
	arn, path := getPathFromARN(secretID)
	if path == "" {
		return p.GetSecret(ctx, secretID)
	}

	val, err := p.GetSecret(ctx, arn)
	if err != nil {
		return "", err
	}

	var secretMap map[string]string
	if err = json.Unmarshal([]byte(val), &secretMap); err != nil {
		return "", err
	}

	v, ok := secretMap[path]
	if !ok {
		return "", errors.New("secret path not found in JSON: " + path)
	}

	return v, nil
}

// GetSecret retrieves a secret value from AWS Secrets Manager using the provided ARN.
func (p *Provider) GetSecret(ctx context.Context, id string) (string, error) {
	result, err := p.client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{
		SecretId: aws.String(id),
	})
	if err != nil {
		return "", err
	}

	return aws.ToString(result.SecretString), nil
}

// GetSecrets retrieves multiple secrets from AWS Secrets Manager using the provided list of ARNs.
func (p *Provider) GetSecrets(ctx context.Context, ids []string) (map[string]string, error) {
	result := make(map[string]string)

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	errCh := make(chan error, len(ids))

	for _, id := range ids {
		wg.Add(1)

		go func(secretID string) {
			defer wg.Done()

			val, err := p.getSecretValueWithOptionalPath(ctx, secretID)
			if err != nil {
				errCh <- err
				return
			}

			if err != nil {
				errCh <- err
				return
			}

			mu.Lock()

			result[secretID] = val

			mu.Unlock()
		}(id)
	}

	wg.Wait()
	close(errCh)

	if err, ok := <-errCh; ok {
		return nil, err
	}

	return result, nil
}

// ResolveSecretReferences resolves the provided map of environment variable names to secret IDs
// by fetching the corresponding secret values from AWS Secrets Manager.
func (p *Provider) ResolveSecretReferences(ctx context.Context, secrets map[string]string) (secrettypes.ResolvedSecrets, error) {
	ids := make([]string, 0, len(secrets))
	for _, id := range secrets {
		ids = append(ids, id)
	}

	resolved, err := p.GetSecrets(ctx, ids)
	if err != nil {
		return nil, err
	}

	for envVar, secretID := range secrets {
		if val, ok := resolved[secretID]; ok {
			secrets[envVar] = val
		}
	}

	return secrets, nil
}

func (p *Provider) Close() {
	// No resources to close for AWS SDK v2 client
}
