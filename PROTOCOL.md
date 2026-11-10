# What the web app receives

This is the web app's side of scrap-king-smart-iot: what it must accept and
check. Using the app itself is covered in the [README](README.md).

The app sends three kinds of `POST` under its configured `endpoint`
(for example `https://your-app.example.com/api/iot`):

| Request | Body | When (settings from the app's `config.json`) |
| --- | --- | --- |
| `{endpoint}/scales` | JSON: the state of every scale and camera | `sendEveryMs` (250 ms by default) after any change, and every `heartbeatSeconds` (5 s by default) regardless |
| `{endpoint}/cameras/{cameraId}/live` | One JPEG | Every `liveIntervalSeconds` per camera (off by default; the example config sends one a second) |
| `{endpoint}/captures` | One JPEG | When a scale's weight settles at or above `minCaptureWeight`: one per settled weight per camera |

Answer any `2xx` for success; the body is ignored. On an error, the first
200 bytes of the body appear in the app's log, so a short
`{ "error": "..." }` helps whoever reads it.

The app does not follow redirects: it reports them as errors. Serve these
routes at exactly the configured address (no trailing-slash or http-to-https
redirect in between).

## Checking the signature

Every request carries these headers:

| Header | Value |
| --- | --- |
| `X-Agent-Id` | The app's `agentId` |
| `X-Timestamp` | Unix time in seconds, when the request was signed |
| `X-Nonce` | A random string, never reused |
| `X-Meta` | JSON describing an image (image requests only) |
| `X-Signature` | Base64 Ed25519 signature of the message below |
| `User-Agent` | `scrap-king-smart-iot/<version>` |

The signed message is these seven lines joined by `\n`, with no newline at the
end:

```
scrap-king-smart-iot-v1
<the request's URL path, e.g. /api/iot/scales>
<X-Agent-Id>
<X-Timestamp>
<X-Nonce>
<X-Meta, or an empty line when there is none>
<lowercase hex SHA-256 of the raw request body>
```

The query string, the method and `Content-Type` are not signed: ignore the
query string, and decide how to read the body from the route, not from
`Content-Type`.

Refuse the request with `401` unless all of these hold:

1. `X-Timestamp` is within 5 minutes of the server's clock.
2. `X-Nonce` has not been seen in the last 10 minutes. Keep the nonces in the
   database (a table with a unique nonce column) when more than one instance of
   the web app runs, or a request replayed to another instance would pass.
3. The signature verifies with this agent's public key.

Read the raw body before parsing it: the signature covers the exact bytes.
Refuse a body over 20 MB with `413` before reading it; the app never sends
one (and drops a photo answered with `413` instead of retrying it).
Send a `Date` header on the `401` (Node and most servers do by default); the
app compares it with its own clock and tells the yard if the PC's clock is off.

The public key is what `scrap-king-smart-iot keygen` printed: base64 of the
DER-encoded SubjectPublicKeyInfo (it always starts `MCowBQYDK2VwAyEA`). With
more than one yard, keep one public key per `agentId` and choose the key by
`X-Agent-Id`. Give the public key only to the deployment the app sends to: a
staging copy holding it too would accept a request captured from production
(nonces are not shared between deployments). Staging gets its own agent and key.

### In a Next.js route handler

Node's built-in `crypto` does all of it:

```ts
import { createHash, createPublicKey, verify } from "node:crypto";

// The agent's public key, as printed by `scrap-king-smart-iot keygen`.
const publicKey = createPublicKey({
  key: Buffer.from(process.env.IOT_AGENT_PUBLIC_KEY ?? "", "base64"),
  format: "der",
  type: "spki",
});

const MAX_SKEW_SECONDS = 300;

const MAX_BODY_BYTES = 20 * 1024 * 1024; // the app never sends more

// Nonces seen within the timestamp window. One process only, and forgotten on
// every restart: with several app instances, keep them in the database (a
// table with a unique nonce column).
const seenNonces = new Map<string, number>();

export type AgentRequest =
  | { ok: true; agentId: string; meta: string; body: Buffer }
  | { ok: false; status: 401 | 413 };

/** Checks that the agent signed this request; answer a failure with its status. */
export async function verifyAgentRequest(request: Request): Promise<AgentRequest> {
  const header = (name: string) => request.headers.get(name) ?? "";
  if (Number(header("content-length")) > MAX_BODY_BYTES) return { ok: false, status: 413 };
  const agentId = header("x-agent-id");
  const timestamp = header("x-timestamp");
  const nonce = header("x-nonce");
  const meta = header("x-meta");
  const signature = Buffer.from(header("x-signature"), "base64");
  const body = Buffer.from(await request.arrayBuffer());
  if (body.length > MAX_BODY_BYTES) return { ok: false, status: 413 };

  const refused = { ok: false, status: 401 } as const;
  const now = Date.now() / 1000;
  if (!/^\d{1,12}$/.test(timestamp) || Math.abs(now - Number(timestamp)) > MAX_SKEW_SECONDS) return refused;
  if (!nonce || seenNonces.has(nonce)) return refused;

  const message = [
    "scrap-king-smart-iot-v1",
    new URL(request.url).pathname,
    agentId,
    timestamp,
    nonce,
    meta,
    createHash("sha256").update(body).digest("hex"),
  ].join("\n");
  if (!verify(null, Buffer.from(message), publicKey, signature)) return refused;

  seenNonces.set(nonce, now + 2 * MAX_SKEW_SECONDS);
  for (const [n, expires] of seenNonces) if (expires < now) seenNonces.delete(n);
  return { ok: true, agentId, meta, body };
}
```

This exact code was run against the app: it accepted every request, refused a
request signed by another key, and refused a request replayed byte for byte.

## `POST {endpoint}/scales`

`Content-Type: application/json`. Each report is the complete current state, so
keep only the latest; a lost report is corrected by the next one. "Latest"
means the greatest `sentAt`, not the last to arrive: a slow request can land
after a newer one, so drop a report older than the one you have. If none has
arrived for about three heartbeats (15 s), treat the yard as offline.

```json
{
  "agentId": "amr-north",
  "version": "0.1.0",
  "sentAt": "2026-11-10T20:58:51.012Z",
  "scales": [
    {
      "id": "inbound-1",
      "name": "Inbound Scale 1",
      "online": true,
      "weight": 2840,
      "unit": "lb",
      "mode": "gross",
      "status": "ok",
      "steady": true,
      "steadySince": "2026-11-10T20:58:49.924Z",
      "readAt": "2026-11-10T20:58:50.998Z"
    },
    {
      "id": "outbound-2",
      "name": "Outbound Scale 2",
      "online": false,
      "error": "192.168.1.12:10002 refused the connection; another program may be holding this scale's line",
      "weight": null,
      "steady": false
    }
  ],
  "cameras": [
    { "id": "scale-camera", "name": "Scale camera", "online": true, "lastImageAt": "2026-11-10T20:58:50.501Z" }
  ]
}
```

| Scale field | Meaning |
| --- | --- |
| `online` | A weight arrived within the scale's `staleSeconds` (3 s by default). |
| `error` | Why the scale cannot be read, when `online` is false. It is written for people: show it as is. |
| `weight` | The latest weight, signed; `null` while offline. |
| `unit` | `lb`, `kg`, `t`, `g` or `oz`, as the indicator sends it. |
| `mode` | `gross`, `net` or `tare`. |
| `status` | The indicator's own flag: `ok`, `zero` (at the centre of zero), `motion`, `overload` (out of range) or `invalid`. |
| `steady` | The app's own judgement, stricter than `status`: still for `settleSeconds` (1 s by default) within `tolerance` (20 by default). Only accept a weight for a ticket while this is true. |
| `steadySince` | When the current settled weight settled. Matches `steadySince` on its photos. |
| `readAt` | When the latest weight arrived. |

A camera's `online` is `null` until the app has asked it for a picture, and
turns `false` only after 3 failed pictures in a row.
Fields that do not apply are left out, except `weight`, which is `null`.

## `POST {endpoint}/cameras/{cameraId}/live`

`Content-Type: image/jpeg`, one picture as the body. `X-Meta`:

```json
{ "cameraId": "scale-camera", "capturedAt": "2026-11-10T20:58:50.501Z" }
```

A slow camera or web app makes the app skip pictures rather than queue them,
so keep only the newest. At 640x480 a picture is about 30 KB.

## `POST {endpoint}/captures`

`Content-Type: image/jpeg`, the photo as the body. `X-Meta`:

```json
{
  "captureId": "U5NQAARUWDRJUUWZYFODAJQBP5",
  "scaleId": "inbound-1",
  "cameraId": "scale-camera",
  "weight": 2840,
  "unit": "lb",
  "mode": "gross",
  "steadySince": "2026-11-10T20:59:09.815Z",
  "capturedAt": "2026-11-10T20:59:10.815Z"
}
```

Store it. A photo that was not accepted is sent again with the same
`captureId` (and a fresh timestamp, nonce and signature): 2 s, 5 s, 15 s, 30 s
and then every minute, for up to 30 minutes. So treat a `captureId` you already
have as success.

The app gives up on a photo at once when the answer is `400`, `413`, `415` or
`422`, since sending it again cannot help. Every other failure, including
`401` and `404`, is retried, so a key or route set up a few minutes late loses
nothing. At 1920x1080 a photo is about 200 KB.
