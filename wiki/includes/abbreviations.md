[This is the global docs glossary]: <> (https://zensical.org/docs/authoring/tooltips/?h=glos#add-a-glossary)

*[AWS]: Amazon Web Services
*[ARN]: Amazon Resource Name: A unique identifier for a resource within Amazon Web Services (AWS).
*[TTL]: Time To Live: The period for which a cached item remains valid before doco-cd refreshes or retrieves it again from the source.
*[UUID]: Universally Unique Identifier
*[artifact]: A read-only copy of a Git repository or OCI artifact at a specific revision, such as a Git commit or OCI digest. doco-cd stores artifacts under the data directory and uses them to serve deployments.
*[OCI]: Open Container Initiative: A set of open standards for container formats and runtimes, ensuring interoperability between different container technologies.
*[OCI artifact]: A content package, such as a container image or deployment configurations, stored and distributed according to OCI specifications. Doco-CD can fetch OCI artifacts from an OCI-compliant registry and use them in deployments.
*[deployment]: A configured doco-cd resource that serves an artifact to a target environment.
*[revision]: The immutable version of an artifact used by doco-cd, represented by a Git commit or an OCI digest.
*[digest]: A content-addressable identifier, such as an OCI image digest, that uniquely identifies the exact contents of an artifact.