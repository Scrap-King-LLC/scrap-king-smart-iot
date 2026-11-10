# scrap-king-smart-iot

A Windows service for the PC at a scrap yard. It reads the truck scales and
cameras on the yard's network and streams them to the Scrap King Smart web app:

- every weight change, within a quarter of a second;
- a live picture from each camera, as often as once a second;
- a full-size photo when a truck's weight settles, tagged with that weight.

It starts with Windows, runs with nobody logged in, restarts itself after a
failure, and reconnects by itself when a scale, camera or the internet drops.
It only makes outgoing connections, so the router needs no changes.

Each request is signed with a private key that never leaves the yard PC. The
web app holds the matching public key, which can check signatures but cannot
make them. The web app's side is described in [PROTOCOL.md](PROTOCOL.md).

## Contents

1. [Install](#install)
2. [Turning on the scales](#turning-on-the-scales)
3. [Restarting after a power cut](#restarting-after-a-power-cut)
4. [Configuration](#configuration)
5. [Commands](#commands)
6. [Logs and troubleshooting](#logs-and-troubleshooting)
7. [Updating, new keys and removal](#updating-new-keys-and-removal)
8. [Building from source](#building-from-source)

## Install

You need an Administrator account on the yard PC, the web app's address, and
the addresses of the scales and cameras.

1. **Copy the files.** Create `C:\Program Files\ScrapKingSmartIoT\` and copy
   `scrap-king-smart-iot.exe` into it. Copy `config.example.json` beside it and
   rename the copy to `config.json`.

2. **Open Command Prompt as Administrator** (Start, type `cmd`, right-click,
   *Run as administrator*) and go to the folder. All the commands below run here.
   In PowerShell, write `.\scrap-king-smart-iot` instead of `scrap-king-smart-iot`.

   ```bat
   cd "C:\Program Files\ScrapKingSmartIoT"
   ```

3. **Make this PC's key pair.**

   ```bat
   scrap-king-smart-iot keygen
   ```

   ```
   Private key written to C:\Program Files\ScrapKingSmartIoT\agent.key. It stays on this PC: never copy or share it.

   Public key, for the web app:

   MCowBQYDK2VwAyEAop8rkkhzqnyt7N1kZIJp18Os+TFHy+L4jZy+FEyCgvI=
   ```

   `agent.key` is created readable only by Administrators and the service.
   Never email it, copy it to another PC or commit it anywhere. If it is ever
   exposed, make a new one (see [New keys](#updating-new-keys-and-removal)).

4. **Give the public key to the web app.** Put the `MCow...` line in the web
   app's environment (for example `IOT_AGENT_PUBLIC_KEY=MCow...` in its `.env`)
   and redeploy it. The public key is not a secret; it can be pasted anywhere.
   To see it again later, run `scrap-king-smart-iot pubkey`.

5. **Edit `config.json`** (in Notepad, as Administrator). Set `agentId`, the
   web app's `endpoint`, and the scales and cameras; see
   [Configuration](#configuration). Leave the scales at `"enabled": false` until
   you have read [Turning on the scales](#turning-on-the-scales).

6. **Check everything.** This reads each enabled scale and each camera once,
   and sends one signed report to the web app:

   ```bat
   scrap-king-smart-iot check
   ```

   ```
   scrap-king-smart-iot 0.1.0, agent "amr-north"
   public key MCowBQYDK2VwAyEAop8rkkhzqnyt7N1kZIJp18Os+TFHy+L4jZy+FEyCgvI=

     skip  scale inbound-1    tcp://192.168.1.12:10001 (disabled)
     skip  scale outbound-2   tcp://192.168.1.12:10002 (disabled)
     ok    camera scale-camera live: 640x480, 30 KB
     ok    camera scale-camera capture: 1920x1080, 196 KB
     ok    endpoint: report accepted
   ```

   Once a scale is enabled, `check` reads it too:
   `ok    scale inbound-1    tcp://192.168.1.12:10001: 0 lb gross, zero`.
   Fix every `FAIL` before going on; [troubleshooting](#logs-and-troubleshooting)
   explains each one.

7. **Keep the PC awake.** A sleeping PC stops sending. Turn sleep and
   hibernation off while it is plugged in:

   ```bat
   powercfg /change standby-timeout-ac 0
   powercfg /change hibernate-timeout-ac 0
   ```

8. **Install and start the service.**

   ```bat
   scrap-king-smart-iot install
   scrap-king-smart-iot start
   scrap-king-smart-iot status
   ```

   It shows in the Services console as **Scrap King Smart IoT** and runs as
   `NT AUTHORITY\LocalService`, an account that can reach the network and
   little else.

## Turning on the scales

A serial device server, such as the Lantronix UDS2100 the scales are cabled to,
lets only **one program at a time** read each scale. While another program
(such as the yard's current scale software) is reading a scale, this app is
turned away and reports:

```
192.168.1.12:10001 refused the connection; another program may be holding this scale's line
```

It keeps retrying, which does the other program no harm while it holds the
line. But if that program ever lets go (it restarts, or its PC reboots), this
app takes the scale and the other program is the one turned away. So set a
scale to `"enabled": true` only when one of these is true:

- **The other program no longer reads the scale.** Turn its scale reading off,
  enable the scale here, and restart this service.
- **The scale's signal is split.** A Y-splitter on the scale indicator's serial
  cable feeds a second serial port, such as a USB serial adapter on this PC, and
  the scale is set up with `serial` instead of `address` (see
  [Configuration](#configuration)). Both programs then read every weight.

Cameras are not affected: any number of programs can view a camera.

The scale indicators must send a continuous weight in the standard Rice Lake
format (the Rice Lake 920i's default; the B-TEK DD700 and most indicators can
be set to it). The app never sends anything to an indicator.

## Restarting after a power cut

- **After Windows restarts** (updates included), the service starts on its
  own, before anyone logs in.
- **If the app fails**, Windows restarts it after 5 seconds, then 30 seconds,
  then every minute.
- **When a scale drops**, the app tries it again after 1 second, then 2, 5
  and 10 seconds, then every 30 seconds, for as long as it takes.
- **When a camera or the internet drops**, the app simply keeps going: it asks
  the camera again for the next picture, and sends the next report on time.
  Photos taken while the internet is down wait in memory and are retried for up
  to 30 minutes (the newest 100 are kept). They are lost if the PC restarts
  before the internet is back.
- **After a power cut**, the PC has to turn itself back on. Set this once in
  its BIOS. On a Dell OptiPlex, press **F2** while it starts, open **Power
  Management**, set **AC Recovery** (on some models **AC Behavior**) to
  **Power On**, and save. Other makes have the same setting under a similar
  name.

Keep Windows' automatic clock setting on (Settings, Time & language, *Set time
automatically*). The web app refuses requests from a PC whose clock is more
than 5 minutes off.

## Configuration

`config.json` sits next to `scrap-king-smart-iot.exe` unless you pass
`-config <path>`. Unknown settings are rejected, so a typo is reported rather
than ignored, and a missing setting (or a 0) takes its default. After changing
it, restart the service (`stop`, then `start`).

```json
{
  "agentId": "amr-north",
  "endpoint": "https://your-app.example.com/api/iot",
  "keyFile": "agent.key",
  "scales": [
    {
      "id": "inbound-1",
      "name": "Inbound Scale 1",
      "enabled": false,
      "address": "192.168.1.12:10001",
      "cameras": ["scale-camera"]
    },
    {
      "id": "outbound-2",
      "name": "Outbound Scale 2",
      "enabled": false,
      "address": "192.168.1.12:10002",
      "cameras": ["scale-camera"]
    }
  ],
  "cameras": [
    {
      "id": "scale-camera",
      "name": "Scale camera",
      "snapshotUrl": "http://192.168.1.222/axis-cgi/jpg/image.cgi?resolution=640x480",
      "captureUrl": "http://192.168.1.222/axis-cgi/jpg/image.cgi?resolution=1920x1080",
      "liveIntervalSeconds": 1
    }
  ]
}
```

| Setting | Default | Meaning |
| --- | --- | --- |
| `agentId` | (required) | This yard's name in the web app: lowercase letters, digits, `-` and `_`. |
| `endpoint` | (required) | The web app's address for this app, from the web app. It must start with `https://` (plain `http://` only works for this PC itself, for testing), and must be the final address: the app does not follow redirects. |
| `keyFile` | `agent.key` | The private key from `keygen`. A relative path starts from the folder holding `config.json`. |
| `heartbeatSeconds` | 5 | The web app hears from the app at least this often, even when nothing changes. |
| `sendEveryMs` | 250 | After a change, how long to gather further changes before sending them. |
| `steady.settleSeconds` | 1 | How long a weight must hold still before it counts as settled. |
| `steady.tolerance` | 20 | How much a settled weight may flicker and still count as settled, in the scale's unit. Set it to one graduation of the scale. |
| `steady.minCaptureWeight` | 500 | No photo is taken when the settled weight is below this (an empty scale). |
| `scales[].id`, `name` | | A fixed id for the web app, and a label for people. |
| `scales[].enabled` | `true` | `false` leaves the scale alone and reports it as disabled. |
| `scales[].address` | | The scale's port on the serial device server, as `ip:port`. A UDS2100 uses port 10001 for its serial port 1 and 10002 for port 2. |
| `scales[].serial` | | Instead of `address`, for a scale cabled to this PC: `{ "port": "COM5", "baudRate": 9600, "dataBits": 8, "parity": "none", "stopBits": 1 }`. Only `port` is required; the rest default to 9600 8N1. Device Manager, *Ports (COM & LPT)*, shows the COM number. |
| `scales[].cameras` | none | Ids of the cameras that photograph this scale when a weight settles. |
| `scales[].staleSeconds` | 3 | With no weight for this long, the scale is reported offline. |
| `scales[].reconnectSeconds` | 60 | With no weight for this long, the connection is dropped and opened again. |
| `cameras[].id`, `name` | | As for scales. |
| `cameras[].snapshotUrl` | (required) | An address that returns one JPEG picture; used for the live picture. |
| `cameras[].captureUrl` | `snapshotUrl` | An address for the settled-weight photos, usually at a higher resolution. |
| `cameras[].username`, `password` | none | Only if the camera asks for them. They are sent only when the camera asks, the way it asks (basic or digest). |
| `cameras[].liveIntervalSeconds` | 0 | Seconds between live pictures; 0 sends none. At 640x480, one picture a second uses about 0.25 Mbit/s of the yard's upload. |

Anyone who can log on to this PC can read `config.json`. If it holds a camera
password, limit it to Administrators and the service:

```bat
icacls config.json /inheritance:r /grant:r *S-1-5-32-544:F *S-1-5-18:F *S-1-5-19:R
```

Picture addresses for common cameras:

- Axis: `http://<camera ip>/axis-cgi/jpg/image.cgi?resolution=640x480`
- Hikvision: `http://<camera ip>/ISAPI/Streaming/channels/101/picture`

## Commands

Run them from the app's folder in an Administrator Command Prompt.

| Command | What it does |
| --- | --- |
| `scrap-king-smart-iot keygen` | Makes this PC's key pair and prints the public key. Refuses to replace an existing key unless given `-force`. |
| `scrap-king-smart-iot pubkey` | Prints the public key again. |
| `scrap-king-smart-iot check` | Reads each enabled scale and each camera once, sends one report, and prints the results. Stop the service first: while it runs it holds the scales, and `check` is turned away. |
| `scrap-king-smart-iot install` | Installs the service to start with Windows and restart after failures. It refuses if the config or key is not right. |
| `scrap-king-smart-iot start`, `stop`, `status` | Start, stop or show the service. |
| `scrap-king-smart-iot uninstall` | Stops and removes the service. |
| `scrap-king-smart-iot run` | Runs in the window instead of as a service, printing its log; Ctrl+C stops it. With `-dry-run` it prints what it would send instead of sending it, and needs no key. |
| `scrap-king-smart-iot version` | Prints the version. |

`-config <path>` (for `check`, `run` and `install`) and `-key <path>` (for
`keygen` and `pubkey`) point at files somewhere other than the app's folder.

## Logs and troubleshooting

The service writes to the Windows **Application** log as **ScrapKingSmartIoT**
(Event Viewer, *Windows Logs*, *Application*). To see the latest 50 entries,
run this in PowerShell:

```powershell
Get-WinEvent -FilterHashtable @{ LogName = 'Application'; ProviderName = 'ScrapKingSmartIoT' } -MaxEvents 50 |
  Format-Table TimeCreated, LevelDisplayName, Message -AutoSize -Wrap
```

Changes are logged, not repeats: a scale that stays off all night is logged
once when it goes off and once when it comes back. A camera or upload counts as
failing only after 3 failures in a row, so one slow answer is not logged.

| Entry | Meaning |
| --- | --- |
| `agent.start`, `agent.stop` | The app started or stopped. |
| `agent.stop_timeout` | The app took more than 15 seconds to stop; Windows stopped it anyway. |
| `config.invalid`, `config.loaded` | The config or key could not be loaded (the entry says why), or loads now. The service tries again every minute, so fixing the file is enough. |
| `scale.online`, `scale.unavailable` | A scale started sending weights, or cannot be read (with the reason). |
| `scale.disabled` | A scale is set to `"enabled": false`. |
| `camera.online`, `camera.unavailable` | A camera answered, or stopped answering. |
| `uplink.connected`, `uplink.failed`, `uplink.restored` | The web app accepted the first report, stopped accepting them (with the reason), or accepts them again. |
| `live.upload_failed`, `live.upload_restored` | The same, for live pictures. |
| `capture.sent` | A settled-weight photo reached the web app. |
| `capture.failed` | The camera did not return a photo after 3 tries, a second apart. |
| `capture.retrying`, `capture.dropped` | A photo is waiting for the web app, or was given up on: after 30 minutes, when 100 are already waiting, or when the web app refused it outright. |

Common problems:

| You see | What to do |
| --- | --- |
| `refused the connection; another program may be holding this scale's line` | Another program is reading that scale. See [Turning on the scales](#turning-on-the-scales). During `check`, it may be this app's own service: stop it first. |
| `connected, but the scale is sending no readings` | The indicator is off, its cable is out, or it is not set to send a continuous weight. |
| `the device server closed the connection`, `the connection to the scale dropped` | The device server or the network between them dropped the line. If it keeps happening, check the cable and the device server. |
| `COM5: Serial port busy` or `COM5: Serial port not found` | Another program has the COM port open, or the adapter is unplugged or has a different number (see Device Manager). |
| `the web app answered 401 Unauthorized ... is this PC's public key ... set in the web app?` | The web app does not have this PC's public key. Run `pubkey`, compare it with the web app's setting, and redeploy the web app if you change it. |
| `the web app answered 401 Unauthorized ... this PC's clock is ... ahead of the web app's` | Set the PC's clock to update automatically (see [Restarting after a power cut](#restarting-after-a-power-cut)). |
| `the web app answered 404 Not Found` | `endpoint` is wrong, or the web app has not been deployed with this feature yet. |
| `the web app answered 301 ... set endpoint to the address it redirects to` | Use the address shown, exactly (often `https://` instead of `http://`, or a different ending slash). |
| `no key at ...: run scrap-king-smart-iot keygen first` | Run `keygen` (step 3 of [Install](#install)), or fix `keyFile`. |
| `Access is denied` | Use a Command Prompt opened with *Run as administrator*. |
| `the service stopped as it started` | Look for the reason in the log above. |

## Updating, new keys and removal

**Update the app.** Stop the service, replace the `.exe` and start it again.
The key and config stay as they are.

```bat
scrap-king-smart-iot stop
copy /y "%USERPROFILE%\Downloads\scrap-king-smart-iot.exe" .
scrap-king-smart-iot start
```

**New keys.** Do this if the key may have been exposed, or when moving the app
to another PC (make a new key there instead of copying the old one):

```bat
scrap-king-smart-iot stop
scrap-king-smart-iot keygen -force
scrap-king-smart-iot start
```

Then put the new public key in the web app and redeploy it. Until then, the
web app turns this PC away (`uplink.failed` in the log), and the old key stops
working as soon as the web app has the new public key.

**Remove the app.** Run `scrap-king-smart-iot uninstall`, then delete the folder
(the key goes with it).

## Building from source

With [Go](https://go.dev/dl/) 1.26 or later installed, from this folder on any
operating system:

```sh
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
  go build -trimpath -ldflags "-s -w -X main.version=0.1.0" -o dist/scrap-king-smart-iot.exe .
```

On a Windows PC, Go builds for Windows by default:

```bat
go build -trimpath -ldflags "-s -w -X main.version=0.1.0" -o dist\scrap-king-smart-iot.exe .
```

`go test ./...` runs the tests. `run` and `check` also work on Linux and macOS
(`go build -o scrap-king-smart-iot .`), which is a quick way to try a config
against a yard's network from a laptop; the service commands are Windows-only.
