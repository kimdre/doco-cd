# AWS Secrets Manager

## Environment Variables

To use AWS Secrets Manager, you need to set the following environment variables:

!!! tip
    Create an access token via IAM, see https://repost.aws/knowledge-center/create-access-key

| Key                                      | Value                                                                    |
|------------------------------------------|--------------------------------------------------------------------------|
| `SECRET_PROVIDER`                        | `aws_sm`                                                                 |
| `SECRET_PROVIDER_REGION`                 | AWS Region to use, e.g. `eu-west-1`                                      |
| `SECRET_PROVIDER_ACCESS_KEY_ID`          | Access key ID of an IAM user with access to AWS Secrets Manager          |
| `SECRET_PROVIDER_SECRET_ACCESS_KEY`      | Secret access key of an IAM user with access to AWS Secrets Manager      |
| `SECRET_PROVIDER_SECRET_ACCESS_KEY_FILE` | Path to the file containing the secret access token inside the container |

## Deployment configuration

Add a mapping/reference between the environment variable you want to set in the docker compose project/stack and the ARN of the secret in AWS Secrets Manager.

Secrets can be retrieved in two ways:

- As clear text/plain string: 
  ```
  arn:aws:secretsmanager:region:account-id:secret:secret-name
  ```
- With a path to the value (Used if the secret contains a JSON):
  ```
  arn:aws:secretsmanager:region:account-id:secret:secret-name/item
  ```

For example in your account `1234567890`, the secret `myapp` in the region `eu-west-1` contains this JSON value: `{"username":"foo","password":"bar"}`

If you want to get the secret value of the `password` field, use the ARN in addition with a slash (`/`) and the field name/key as the path:
```
arn:aws:secretsmanager:eu-west-1:1234567890:secret:myapp/password
```

!!! note
    Without specifying a path, the entire JSON gets returned as a single string (see example below).

### Example

For example in your `.doco-cd.yml`:

```yaml title=".doco-cd.yml"
external_secrets:
  JSON_STRING: "arn:aws:secretsmanager:eu-west-1:1234567890:secret:myapp"  # '{"username":"foo","password":"bar"}'
  APP_PASSWORD: "arn:aws:secretsmanager:eu-west-1:1234567890:secret:myapp/password" # bar
```
