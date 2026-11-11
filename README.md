# scrap-king-smart-iot

Streams the yard's truck scales and scale camera to the New Log page of Scrap King
Smart. It is one PowerShell script, `scrap-king-iot.ps1`, and its settings,
`config.json`. It runs on the yard PC with the PowerShell built into Windows, so
there is nothing else to install.

| Web app | `endpoint` in `config.json` |
| --- | --- |
| Testing (the default) | `https://dev.scrapkingsmart.com/api/iot` |
| Production | `https://scrapkingsmart.com/api/iot` |

## Install

Do this on the yard PC that runs ScrapRight's IoT Agent. At AMR North that is the
Dell in the scale house. You need an Administrator account.

1. **Get the files.** Sign in to GitHub and open this repository. Click
   **Code**, then **Download ZIP**, and unzip it anywhere, for example in Downloads.

2. **Check `config.json`.** It is already set up for AMR North on the testing site.
   For production, change `endpoint` (see the table above). The fields are explained
   [below](#configjson).

3. **Run the script.** Right-click `scrap-king-iot.ps1`, choose **Run with
   PowerShell**, and answer **Yes** when Windows asks. Or, in PowerShell opened as
   Administrator, from the unzipped folder:

   ```powershell
   powershell -ExecutionPolicy Bypass -File .\scrap-king-iot.ps1
   ```

   The script then:
   - copies itself to `C:\Program Files\ScrapKingIoT`;
   - makes this PC's key;
   - starts the agent now and at every boot;
   - checks it.

   It ends by printing this PC's public key, which is also saved in
   `C:\Program Files\ScrapKingIoT\public-key.txt`:

   ```
   amr-north:MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE...
   ```

4. **Give the key to the web app (once).** Add that whole line to the
   `IOT_AGENT_KEYS` environment variable in the web app's ECS task definition (the
   testing one or the production one, to match `endpoint`), then deploy. If you
   have more than one yard, separate the lines with commas. The key stays the same
   every time you install again, so you only do this once per PC.

5. **Check it** with the line below. Then open New Log for the yard: the scales and
   the camera appear beside the form.

## Is it good to go?

```powershell
powershell -ExecutionPolicy Bypass -File "C:\Program Files\ScrapKingIoT\scrap-king-iot.ps1" check
```

```
  ok    web app             https://dev.scrapkingsmart.com/api/iot
  ok    scale inbound-1     2840 lb gross, ok
  ok    scale outbound-2    0 lb gross, ok
  ok    camera scale-camera  35 KB picture
GOOD TO GO
```

It ends with `GOOD TO GO` (exit code 0) or `NOT READY` (exit code 1). When it is
not ready, a `FAIL` line says why:

| `FAIL` line says | Do this |
| --- | --- |
| `No public key is set for this agent` | Do step 4, or wait for its deploy to finish |
| `No connection from this PC to 192.168.1.12. Is ScrapRight's IoT Agent running?` | Start ScrapRight's IoT Agent. This script hears the scales through ScrapRight's own connection |
| `The camera did not answer` | Open the camera's `snapshotUrl` in a browser on this PC |
| `not running` | Read `C:\Program Files\ScrapKingIoT\agent.log` |
| `This PC's clock is more than 5 minutes off` | Turn on *Set time automatically* in Windows' date and time settings |

## Changing settings

Edit `config.json` beside the script you first ran, then run the script again. The
key stays the same.

## Uninstall

```powershell
powershell -ExecutionPolicy Bypass -File "C:\Program Files\ScrapKingIoT\scrap-king-iot.ps1" uninstall
```

This stops the agent and deletes `C:\Program Files\ScrapKingIoT`. It keeps this
PC's key, so installing again needs no change in the web app.

## config.json

| Field | What it is |
| --- | --- |
| `agentId` | The yard, as it appears in the web app's addresses: `amr-north`, `amr-south` or `scrap-king` |
| `endpoint` | The web app to send to (see the table at the top) |
| `scales` | Each scale: an `id` (lowercase letters, digits and `-`), the `name` New Log shows, and the `address` (IP:port) of the serial device server port it is read from. `enabled: false` turns a scale off |
| `cameras` | Each camera: an `id`, a `name`, a `snapshotUrl` that returns one JPEG, and `liveIntervalSeconds` between pictures. `enabled: false` turns a camera off |

## How it works

- **Scales.** The script never connects to the serial device server.
  - ScrapRight's IoT Agent keeps its connections to the device server, as it
    always has. Windows gives this script a copy of the readings arriving at this
    PC for those connections (the same thing `pktmon` showed).
  - ScrapRight works exactly as before.
  - This script hears the scales only while ScrapRight's agent is connected.
- **The web app.** Once, the script signs a handshake with this PC's private key
  and gets a token back.
  - The private key lives in the Windows key store and cannot be copied off this
    PC.
  - Every event after that carries only the token, over one HTTPS connection that
    stays open.
- **What it sends.**
  - A scale's weight, when it changes (at most 4 times a second) and every
    5 seconds even if it doesn't.
  - A camera picture every `liveIntervalSeconds`. A picture is about 35 KB, so one
    a second uses about 3 GB of the yard's upload a day.
  - Nothing is stored on the way: New Log shows each event as it arrives.
- **Staying up.** Task Scheduler starts the script as SYSTEM at boot and checks
  every minute that it is still running.
- **The log** is in `C:\Program Files\ScrapKingIoT\agent.log`.
