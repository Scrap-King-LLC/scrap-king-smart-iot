# scrap-king-smart-iot

A Windows service for the yard PC. It reads the truck scales and the scale
camera and streams them to Scrap King Smart at:

| Web app | `endpoint` in `config.json` |
| --- | --- |
| Production | `https://scrapkingsmart.com/api/iot` |
| Testing | `https://dev.scrapkingsmart.com/api/iot` |

## Install

You need an Administrator account on the yard PC, and the addresses of the
scales and the camera.

1. **Copy the files.** Create `C:\Program Files\ScrapKingSmartIoT\` and copy
   `scrap-king-smart-iot.exe` into it. Copy `config.example.json` beside it and
   rename the copy to `config.json`.

2. **Open Command Prompt as Administrator** (Start, type `cmd`, right-click,
   *Run as administrator*) and go to the folder. Every command below runs here.

   ```bat
   cd "C:\Program Files\ScrapKingSmartIoT"
   ```

3. **Make this PC's key.** It prints the public key, a line starting `MCow`.
   The private key stays in `agent.key` on this PC: never copy or share it.

   ```bat
   scrap-king-smart-iot keygen
   ```

4. **Give the public key to the web app.** Add `<yard>:<public key>` to the web
   app's `IOT_AGENT_KEYS` setting (on its ECS task definition) and redeploy it.
   Separate yards with commas:

   ```
   IOT_AGENT_KEYS=amr-north:MCowBQYDK2VwAyEAop8rkkhzqnyt7N1kZIJp18Os+TFHy+L4jZy+FEyCgvI=
   ```

   `scrap-king-smart-iot pubkey` prints the public key again.

5. **Edit `config.json`** in Notepad (as Administrator):
   - `agentId`: the yard, exactly as in the web app's addresses: `amr-north`,
     `amr-south` or `scrap-king`. It must match the name used in step 4.
   - `endpoint`: from the table above.
   - `scales`: each scale's port on the serial device server (`address`), or,
     for a scale cabled to this PC, `"serial": { "port": "COM5" }` instead.
     Leave a scale at `"enabled": false` while another program (ScrapRight)
     still reads it: the device server lets only one program read each scale,
     and this one would take it over.
   - `cameras`: `snapshotUrl` returns one JPEG (Axis:
     `http://<camera ip>/axis-cgi/jpg/image.cgi?resolution=640x480`; Hikvision:
     `http://<camera ip>/ISAPI/Streaming/channels/101/picture`). Add `username`
     and `password` only if the camera asks for them.

6. **Keep the PC awake** while it is plugged in, and keep Windows' automatic
   clock setting on (the web app refuses a PC whose clock is 5 minutes off):

   ```bat
   powercfg /change standby-timeout-ac 0
   powercfg /change hibernate-timeout-ac 0
   ```

7. **Check everything** with the one-liner below, then **install and start the
   service**. It starts with Windows, before anyone logs in, and restarts after
   a failure.

   ```bat
   scrap-king-smart-iot install
   scrap-king-smart-iot start
   ```

## Is it good to go?

```bat
scrap-king-smart-iot check && echo GOOD TO GO || echo NOT READY
```

It reads each enabled scale and each camera once and sends one signed report
to the web app, so it checks the wiring, the key, the endpoint and the clock
together. Each line says `ok` or `FAIL` with the reason:

```
  ok    scale inbound-1    tcp://192.168.1.12:10001: 2840 lb gross, ok
  skip  scale outbound-2   tcp://192.168.1.12:10002 (disabled)
  ok    camera scale-camera live: 640x480, 30 KB
  ok    endpoint: report accepted
GOOD TO GO
```

Once the service is installed, run `scrap-king-smart-iot stop` first and
`scrap-king-smart-iot start` after: while the service runs it holds the scales,
and `check` is turned away from them.
