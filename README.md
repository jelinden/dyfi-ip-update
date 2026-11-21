# dy.fi ip updater

See: [https://www.dy.fi/page/specification](https://www.dy.fi/page/specification)

## What does it do?

Runs as a daemon and keeps the A records of the hostnames in
`domainsToBeUpdated` (main.go) pointing to this machine's outbound IP:

- Checks every 30 seconds whether the outbound IP has changed; if it
  has, all hosts are updated via the dy.fi update API.
- Renews every host's reservation every 5 days + a per-host random
  jitter (dy.fi reservations expire after 7 days, and the spec asks
  for updates between 5x24h and 6x24h after the previous one).
- Parses the dy.fi reply (`good`, `nochg`, `nohost`, `badauth`,
  `dnserr`, `abuse`, ...) and retries transient failures with backoff.
- If a reservation has expired (`nic/update` answers `nohost`), the
  host is **re-registered automatically** by logging in to the dy.fi
  web UI and submitting the "add a new host" form (dy.fi has no API
  endpoint for adding hosts).
- All update requests are paced (3 s apart) through a single worker so
  the dy.fi abuse protection is not triggered.
- Exits cleanly on SIGINT/SIGTERM.

Note: dy.fi only accepts .fi/.dy.fi hostnames registered to your
account, and the outbound IP must be registered to a Finnish
organisation (`badip` otherwise).

## Build

`go build`

## Configuration

The domains to update live in a JSON file outside the code (default
`dyfi.json` next to the binary, or `-config /path/to/file.json`).
Copy `dyfi.example.json` to `dyfi.json` and edit:

```json
{
  "domains": [
    "jelinden.fi",
    "www.jelinden.fi",
    "uutispuro.fi"
  ]
}
```

If the file is missing, the built-in default domain list is used.
`dyfi.json` is gitignored; duplicates and surrounding whitespace are
ignored and domains are lowercased automatically.

## Running

dyUsername=USERNAME dyPassword=PASSWORD ./dyfi-ip-update [-config dyfi.json]

## Tests

`go test -race ./...`
