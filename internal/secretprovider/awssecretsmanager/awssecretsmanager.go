package awssecretsmanager

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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
	// Deduplicate Secret ARNs to minimize API calls
	var uniqueSecretARNS []string

	for _, id := range ids {
		arn, _ := getPathFromARN(id)
		if !slices.Contains(uniqueSecretARNS, arn) {
			uniqueSecretARNS = append(uniqueSecretARNS, arn)
		}
	}

	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)

	errCh := make(chan error, len(ids))

	// Fetch all unique Secret ARNs concurrently and store secret values
	secretValues := make(map[string]string, len(uniqueSecretARNS))
	for _, id := range uniqueSecretARNS {
		wg.Add(1)

		go func(secretID string) {
			defer wg.Done()

			val, err := p.GetSecret(ctx, id)
			if err != nil {
				errCh <- err
				return
			}

			mu.Lock()

			secretValues[secretID] = val

			mu.Unlock()
		}(id)
	}

	wg.Wait()
	close(errCh)

	if err, ok := <-errCh; ok {
		return nil, err
	}

	// Map back to original IDs, handling paths if necessary
	result := make(map[string]string, len(ids))
	for _, id := range ids {
		arn, path := getPathFromARN(id)

		val, ok := secretValues[arn]
		if !ok {
			return nil, errors.New("missing secret for ARN: " + arn)
		}

		if path == "" {
			result[id] = val
			continue
		}

		// If a path is specified, assume the secret value is a JSON object and extract the value at the path
		// Example: if path is "db_password", expect the secret value to be {"db_password": "actual_password"}
		var secretMap map[string]string
		if err := json.Unmarshal([]byte(val), &secretMap); err != nil {
			return nil, err
		}

		v, ok := secretMap[path]
		if !ok {
			return nil, errors.New("secret path not found in JSON: " + path)
		}

		result[id] = v
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
