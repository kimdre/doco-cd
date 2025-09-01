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

### Next Steps

1. Set up [Git Commit Signing](https://docs.github.com/en/authentication/managing-commit-signature-verification/signing-commits) if you haven't already done so. Please always sign your commits!
2. Clone the repository.
3. In the cloned repo, execute `make tools` on your command line to download/install the Go tool binaries (like linters and formatters).

### Testing

After you wrote your code, run `make fmt` to format and lint your code. Adjust your code accordingly to the proposals of the linter output. 
The CI pipeline will fail, if the code is not formatted correctly.

Always add unit tests to verify your code. 
Run the tests with `make test` or `make test-verbose` and run specific tests with `make test-run <testName>` (Verbose with `make test-run "-v <testName>"`).

When you are done, [open a pull request](https://github.com/kimdre/doco-cd/pulls).
