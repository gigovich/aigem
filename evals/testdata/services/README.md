# services

Three independent HTTP services. Nothing is shared between them, and each one has its own
storage, its own config, and its own retry policy.

| Service | Domain | Package |
| --- | --- | --- |
| `alpha` | accounts | `services/alpha` |
| `beta` | billing | `services/beta` |
| `gamma` | notifications | `services/gamma` |

Every service follows the same layout: `api.go` for the HTTP surface, one file for its storage,
and `config.go` for its settings.
