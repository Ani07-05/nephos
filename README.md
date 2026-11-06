# nephos

A cloud for small software. Deploy a folder, invite people by email, they sign in and use it.

```
nephos serve                      run the server (dashboard link is printed)
nephos deploy ./folder [name]     publish a folder that has an index.html
nephos share <name> <email>       invite someone
nephos access <name> invited|org|public
nephos env <name> KEY=value       settings and secrets, read by handlers as env.KEY
nephos rollback <name>            switch back to the previous deploy
nephos tui                        manage apps from the terminal
nephos mcp                        let an AI agent deploy and share (see below)
```

An app can also ship a `handler.js` that defines `handle(req)`. It runs in a sandbox
(QuickJS on wazero) for requests to `/api/*`, with `kv.get/set/delete/list` for storage.

## Let an agent build and share it

`nephos mcp` is an MCP server on stdin/stdout. Point your agent at it and it can write an app,
deploy it, set a secret, open it to your organization and invite a colleague, all in one chat:

```
claude mcp add nephos -e NEPHOS_URL=https://nephos.example.com -e NEPHOS_TOKEN=... -- nephos mcp
```

Tools: `list_apps`, `deploy_app`, `share_app`, `set_access`, `set_env`, `rollback_app`.
There is deliberately no delete, and settings can be written but never read back.
`examples/flow.sh` runs this whole story against a real server and checks every step.

## Who can open an app

- **Only people I invite** (the default).
- **Anyone at your organization**: start the server with `-org-domain acme.com`. Anyone with a
  matching email can sign in, no invite needed. Add `-org-only` to refuse invitations to other domains.
- **Anyone with the address**.

Narrowing access ends the sessions that no longer qualify.

## Try it locally

```
go build -o nephos . && ./nephos serve
./nephos deploy examples/counter
./nephos share counter you@example.com
```

Without SMTP configured, sign-in emails are printed in the server log instead of sent.
Local app addresses look like `counter.localhost:8080` (Chrome and Firefox resolve them).

## Put it on the internet

You need a server with ports 80 and 443 open, and a domain.

1. Add two DNS records pointing at the server: `nephos.example.com` and `*.nephos.example.com`.
2. `cp .env.example .env` and fill it in. Use any SMTP provider (Resend, Postmark, SES, Mailgun).
3. `docker compose up -d`
4. `docker compose logs nephos` prints the dashboard login link.
5. Deploy from your laptop:
   ```
   export NEPHOS_URL=https://nephos.example.com
   export NEPHOS_TOKEN=<token from data/admin_token or the log link>
   nephos deploy ./my-app
   ```

Caddy gets a certificate for each app the first time it is visited, and only for apps that
exist (`/_internal/tls-ask` answers Caddy and nobody else).

## How sharing works

- Inviting an email sends that person a one-time link. They click Continue and stay signed in
  on that device for 30 days.
- A link works once and expires: 7 days for invites, 15 minutes for ones requested from the
  sign-in page. Opening a link only shows a confirm page, so mail scanners can't use it up.
- Removing someone signs them out everywhere at once.
- The sign-in page says the same thing whether or not an address has access, and is rate limited.
- Only hashes of links and sessions are stored. Over https, cookies are `__Host-` prefixed and Secure.
- Every app has its own origin, so one app can't read another's session.

## Limits

- One owner per server. There are no teams or accounts yet, and the agent token can do everything
  the MCP tools allow (no per-agent scopes).
- Handlers can't call other services yet.
- Handlers have no network access, and a handler can briefly use a lot of memory before its
  2 second limit stops it.
- Sign-in links are only as safe as the invited person's mailbox.
