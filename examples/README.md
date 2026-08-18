# Examples

Full working setups for common doco-cd scenarios. Each example replicates every repo it needs:

- `app-repo/` or `deployments-repo/` — what you commit to Git.
- `server/` — what you put on the VM next to Docker (doco-cd itself).

| Example | Pattern | Pick it when |
|---|---|---|
| [single-repo-single-env](single-repo-single-env/) | App repo carries its own compose + `.doco-cd.yml`. One VM polls it. | You have one app and one server. Start here. |
| [single-repo-two-envs](single-repo-two-envs/) | Same repo, two deploy configs. Each VM selects its own via poll `target:`. Adds notifications. | You have staging and prod of the same app. |
| [deployments-repo](deployments-repo/) | One central repo deploys many apps to many VMs. Version pins per VM, compose files pulled from app repos, secrets encrypted with SOPS. | You have a fleet of apps/VMs and want one place that states what runs where. |

Notes that apply to all examples:

- All examples poll. Polling needs no inbound port, so the VM firewall stays closed. Webhooks work the same way — set `WEBHOOK_SECRET` and publish the port.
- Examples use the `latest` image tag to stay copy-pasteable. In real use pin the doco-cd image by tag + digest.
- The `server/` compose is the one thing doco-cd cannot GitOps — it deploys stacks, not itself. Copy it to the VM by hand (or see [Self-Updating](https://doco.cd/latest/Advanced/Self-Updating/)).
