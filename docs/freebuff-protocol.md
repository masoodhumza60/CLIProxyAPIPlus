# Freebuff

This provider exposes Freebuff's models through the proxy's OpenAI-compatible
endpoints. It signs in with the device-code flow, discovers which models the
account may actually use, and streams answers from the web application's chat
route.

## Getting started

Run `freebuff login`. It opens a browser, you sign in, and the credential is
stored under `auths/` like every other provider here. Nothing is pasted by hand
and no token is typed into a config file.

After that, `GET /v1/models` lists the models your account can run, and the
usual OpenAI and Anthropic routes serve them.

## Configuration

```yaml
freebuff:
  enabled: true
  # Where login and chat live.
  base-url: "https://freebuff.com"
  # Optional. Only needed for a self-hosted deployment; see "Two origins" below.
  catalog-base-url: "https://www.codebuff.com"
  freebuff-api-key:
    - name: my-account
      token: <not needed: run `freebuff login` instead>
      models:
        - MiMo 2.6 Flash
        - GLM 5.3 Flash
```

`models:` is an allow-list and is optional. It can only narrow the discovered
list — it can never add a model the account cannot run, because the service is
the authority on what exists.

A static token can be supplied instead of logging in, but `freebuff login` is
preferred: the token is a session credential with a finite life, and the error
when it expires says so.

## Two origins

The provider talks to two different services and they are not interchangeable:

| Purpose | Origin | Auth |
|---|---|---|
| Sign-in, chat, conversation history | `base-url` (`freebuff.com`) | session cookie |
| Model catalogue | `catalog-base-url` (`www.codebuff.com`) | bearer **and** `x-codebuff-api-key` |

The catalogue origin is never inferred from `base-url`. Deriving one from the
other would send catalogue traffic somewhere nobody chose, and the two services
do not answer on each other's paths.

Both auth headers are required on the catalogue. Sending only `Authorization`
is rejected with a 401.

## Model names

A model may be named three ways, and all three are accepted:

- the display name, e.g. `MiMo 2.6 Flash`
- the catalogue key, e.g. `m-00032eaeec`
- the recommended row, when nothing else matches

The list is **discovered, never hardcoded**, because it is per-account and moves.
One account may see models another cannot, and rows disappear as the access tier
changes. A fixed list would advertise models that fail on first use.

Rows the account may not run — those marked as needing a paid plan — are left
off the list entirely.

## Expired sessions

The credential is a session token with a finite life. When it ages out, requests
fail with a 401 and a message telling you to run `freebuff login` again. That is
deliberate: retrying an expired session cannot succeed, so the useful advice is
to authenticate again.

The failure is reported as credential-scoped, so the proxy stops routing to a
token that cannot work rather than retrying it across a request.

## What this client does not do

- It does not impersonate the official CLI. The official CLI sends extra markers
  and a device signature; this client sends neither, and the requests it makes
  are its own.
- It does not randomise its `User-Agent`. It sends a constant
  `cli-proxy-api-freebuff/1.0`.
- It does not work around access gates. Rate limits, spend limits, country
  blocks and bans are reported as the service states them.
- It does not call the public Codebuff chat API, which refuses third-party
  clients.

## Notes for contributors

The chat route takes a single prompt string rather than a message array, and
has no tool-call channel. The executor therefore flattens a translated request
into a role-prefixed prompt. Tool definitions are not forwarded, because the
route would treat them as part of the prompt.

Streaming reads Server-Sent Events. An event type this client does not model is
skipped rather than failing the request, so a new event upstream cannot break a
healthy answer.

The fixture used by the tests lives behind a build tag and enforces the same
gates the live service does: the session cookie on chat, both auth headers on
the catalogue, and a model that must come from the catalogue. Run it with:

```bash
go test -tags freebuffmock ./internal/runtime/executor/... -run Freebuff
```

A permissive fixture would let a real defect through, which is why it is strict.