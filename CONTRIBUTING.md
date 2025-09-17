# Contributing to doco-cd

Thank you for your support. Help is always appreciated!

## Have an issue, idea or question?

- Ask questions or discuss ideas on [GitHub Discussions](https://github.com/kimdre/doco-cd/discussions)
- Report bugs or suggest features by [opening an issue](https://github.com/kimdre/doco-cd/issues/new)

## Want to contribute your code?

### Required tools

The following tools need to be installed:

- A IDE/Editor of your choice
- [Git](https://git-scm.com/)
- Golang (see the [`go.mod`](https://github.com/kimdre/doco-cd/blob/main/go.mod#L3) file for the currently used version.
- [Make](https://www.gnu.org/software/make/)

### Additional steps

Follow the installation instructions for the following dependencies:

- [Bitwarden Go SDK](https://github.com/bitwarden/sdk-go/tree/main?tab=readme-ov-file#installation)

### Getting started

1. Set up [Git Commit Signing](https://docs.github.com/en/authentication/managing-commit-signature-verification/signing-commits) if you haven't already done so. Please always sign your commits!
2. Clone the repository.
3. In the cloned repo, execute `make tools` on your command line to download/install the Go tool binaries (like linters and formatters).

### Coding style

Please follow these guidelines when contributing code:

- Write comments and documentation where necessary.
- Follow Go best practices and conventions. Refer to the [Effective Go](https://golang.org/doc/effective_go) guide for more information.
- Write unit tests for your code that cover various scenarios and edge cases including error cases if applicable.
- Write clear, concise, and descriptive commit messages and use [conventional commits](https://www.conventionalcommits.org/en/v1.0.0/).
- Always sign your commits. See [Signing commits](https://docs.github.com/en/authentication/managing-commit-signature-verification/signing-commits) for more information.

### Testing

After you wrote your code, run `make fmt` to format and lint your code. Adjust your code accordingly to the proposals of the linter output. 
The CI pipeline will fail, if the code is not formatted correctly.

Always add unit tests to verify your code. 
Run the tests with `make test` or `make test-verbose` and run specific tests with `make test-run <testName>` (Verbose with `make test-run "-v <testName>"`).

#### With registry credentials

You can provide container registry credentials to avoid rate limiting issues when running tests.

Follow the [Accessing private container registries](https://github.com/kimdre/doco-cd/wiki/Tips-and-Tricks#accessing-private-container-registries) Guide in the Wiki for more information.

### Building from Source

Create a `.env` file and add any environment variables required for your development environment.

For example

```dotenv
GIT_ACCESS_TOKEN=xxx
WEBHOOK_SECRET=test_secret
SOPS_AGE_KEY=AGE-SECRET-KEY-xxx
APPRISE_NOTIFY_URLS=xxx
API_SECRET=xxx
```

Run the following command to build and run the doco-cd dev container:

```bash
docker compose -f dev.compose.yaml up --build
```

### Submitting your code

1. Commit your changes with a descriptive commit message, following the [conventional commits](https://www.conventionalcommits.org/en/v1.0.0/) specification.
2. Push your changes to your fork/branch.
3. Open a pull request against the `main` branch of this repository and provide a clear description of your changes.
