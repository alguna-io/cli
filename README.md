# alguna CLI

`alguna` is the command-line client for the [Alguna API](https://alguna.com/docs). Each API operation is one command.

## Install

```sh
npx @alguna/cli --help                                 # npm, no install
npm install --global @alguna/cli                       # npm
go install github.com/alguna-io/cli/cmd/alguna@latest  # Go
```

Or download an archive for your system from the [releases page](https://github.com/alguna-io/cli/releases).

## Log in

Create an API key in the Alguna dashboard under **Settings → Connections → Developers → API keys**. A key looks like `<key_id>.<key_secret>`.

`alguna login` asks for the key and saves it in `alguna/config.json` in your user config directory, with mode `0600`. On a terminal, the key is not shown while you type. In a script, use `alguna login --api-key "$KEY"`, or send the key on stdin. `alguna logout` removes the saved key.

A flag wins over an environment variable, and an environment variable wins over the saved key:

| Setting     | Flag            | Environment variable | Default                   |
| ----------- | --------------- | -------------------- | ------------------------- |
| API key     | `--api-key`     | `ALGUNA_API_KEY`     | the key saved by login    |
| Base URL    | `--base-url`    | `ALGUNA_BASE_URL`    | `https://api.alguna.io`   |
| API version | `--api-version` | `ALGUNA_API_VERSION` | shown by `alguna version` |

To use the sandbox, set `ALGUNA_BASE_URL=https://api.sandbox.alguna.io`.

## Use

```sh
alguna customers list --limit 10
alguna customers list --all -o ndjson > customers.ndjson
alguna customers get cus_123
alguna customers create --name "Acme" --currency USD
alguna customers create --body @customer.json --name "Acme Ltd"
alguna invoices generate-pdf inv_123 --out invoice.pdf
alguna invoices void inv_123 --yes
```

- Path parameters are arguments. Query parameters and the top-level text, number and true/false fields of the request body are flags. `alguna <resource> <operation> --help` shows them.
- `--body` sets the request body as JSON: inline, `@file`, or `-` for stdin. A field flag replaces the same field. Give object and list fields in `--body`.
- The CLI writes indented JSON to stdout. `-o ndjson` writes one line for each item of a list.
- `--all` gets every page. With `-o json` it writes one array after the last page; with `-o ndjson` it writes each page as it arrives. With `--all`, `--limit` is the most items in total.
- `--dry-run` writes the request and sends nothing. The API key shows as `[REDACTED]`.
- A command that deletes data, moves money or cannot be reversed asks you to confirm. `--yes` skips the question. When stdin is not a terminal, `--yes` is necessary.

## Idempotency and retries

Every command that is not a GET sends an `Idempotency-Key`: a new random key (UUID v4) on each run, or the key that you set with `--idempotency-key`. Every retry in that run sends the same key, so the API returns the first result and does not do the action again. Set your own key when you run a write again yourself, for example after a script stopped halfway.

The exceptions are `alguna events ingest`, `alguna credits track` and `alguna credits check`: they send no key unless you set one, because the API identifies their work by the request body. It removes duplicate events by `unique_id`, and `credits track` draws credit once for the same `unique_id`s. A new key on each run would replace that identity on `credits track`, so a run repeated after a failure would draw credit twice. The CLI still retries these commands. `alguna credits check` with `reserve` needs `--idempotency-key`, because the key identifies the hold.

The CLI retries after status 408, 429, 502, 503 or 504, and after a network error: a refused or reset connection, a connection closed before the response, or a timeout. It also retries a 409 that has a `Retry-After` header. The API sends that header only while a request with the same idempotency key is still running; a 409 without it is a real conflict.

By default the CLI retries 2 times (3 attempts). `--max-retries` sets the number, and `--max-retries 0` turns retries off. Between attempts it waits 1 second, then 2, then 4, up to 60 seconds, each with ±10% jitter. When the response has a `Retry-After` header (seconds or an HTTP date), it waits for that time instead, up to 60 seconds.

```sh
alguna customers create --name "Acme" --currency USD --idempotency-key signup-acme
alguna invoices list --all --max-retries 5
```

## Timeouts

Each attempt has 60 seconds to get a response. A retry gets the full time again. `--timeout` sets a different time:

```sh
alguna invoices generate-pdf inv_123 --out invoice.pdf --timeout 2m
```

## Errors and exit codes

On a terminal, an error is one line on stderr, such as `Error: customer not found (HTTP 404, request req_123)`. The text is the API's `detail`, else its `title`, else its `message`, else the HTTP status text. Otherwise, or with `--json-errors`, the error is one line of JSON with the status, the request ID and the response.

| Exit code | Meaning                                                       |
| --------- | ------------------------------------------------------------- |
| 0         | Success                                                       |
| 1         | Any other error, such as a wrong command or flag              |
| 4         | The API returned a 4xx status                                 |
| 5         | The API returned a 5xx status, or the request got no response |

## About this repo

This repo is generated from the Alguna API spec in a private repo. Open issues here; Alguna ports code changes to the private repo, so pull requests here are not merged. `cmd/alguna/manifest.json` describes each command, and the binary embeds it. Run the tests with `go test ./...`.
