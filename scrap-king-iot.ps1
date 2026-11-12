<#
.SYNOPSIS
  Scrap King Smart's yard agent: streams the truck scales and the scale camera
  on this PC to the web app's New Log page.

.DESCRIPTION
  One script and its config.json. Windows PowerShell 5.1, nothing to install.

    install    (the default) set it up and start it, now and at every boot,
               check it, and print this PC's public key for the web app
    check      is it working? GOOD TO GO or NOT READY
    uninstall  stop it and remove it (this PC's key is kept)
    run        the agent itself: what Task Scheduler starts

  The scales are read passively. ScrapRight's IoT Agent stays connected to the
  serial device server as it always has; this script listens to the readings
  arriving at this PC for that connection and never connects to the device
  server itself, so it cannot take a scale away from ScrapRight.

  The web app is told who this is once: the script signs a handshake with this
  PC's private key, kept in the Windows key store, and gets a token. Each request
  after that carries only the token, over HTTPS connections kept open.

  Nothing is uploaded while nobody has the yard's New Log open. The script
  only asks, with one request the web app holds open, and starts sending the
  moment someone opens the page.

  When a ticket is logged, the web app asks for the scale camera's full-size
  picture in an event's answer; the script takes it from the camera's
  captureUrl and sends it, and the web app stores it with the ticket.
#>
param(
  [ValidateSet('install', 'check', 'uninstall', 'run')]
  [string]$Command = 'install'
)

$ErrorActionPreference = 'Stop'
$Version = '2.2.0'
$TaskName = 'Scrap King IoT'
$KeyName = 'ScrapKingIoT'
# The 64-bit Program Files, even when a 32-bit PowerShell runs the install.
$ProgramFilesDir = $env:ProgramW6432
if (-not $ProgramFilesDir) { $ProgramFilesDir = $env:ProgramFiles }
$InstallDir = Join-Path $ProgramFilesDir 'ScrapKingIoT'
$ScriptFile = Join-Path $InstallDir 'scrap-king-iot.ps1'
$StatusFile = Join-Path $InstallDir 'status.json'
$LogFile = Join-Path $InstallDir 'agent.log'
$PowerShellExe = Join-Path $env:SystemRoot 'System32\WindowsPowerShell\v1.0\powershell.exe'

# A weight is sent when it changes, at most every 250 ms, and every 5 s
# regardless, so the page can tell a quiet scale from a silent one.
$SendAfterChangeMs = 250
$SendEveryMs = 5000
# A scale with no reading for this long is not sent at all: the page shows it
# offline. Both scales send several readings a second.
$QuietAfterMs = 3000

$Units = @{ L = 'lb'; K = 'kg'; T = 't'; G = 'g'; O = 'oz' }
$Modes = @{ G = 'gross'; N = 'net'; T = 'tare' }
$Statuses = @{ ' ' = 'ok'; M = 'motion'; Z = 'zero'; O = 'overload'; I = 'invalid' }

# --- Small helpers -----------------------------------------------------------

function Get-Reason($err) { $err.Exception.GetBaseException().Message }

function Write-Log([string]$message) {
  if ($Command -ne 'run') { Write-Host $message }
  try {
    if ((Test-Path $LogFile) -and (Get-Item $LogFile).Length -gt 1MB) {
      Move-Item -Force $LogFile "$LogFile.old"
    }
    Add-Content -Path $LogFile -Encoding UTF8 -Value ('{0:yyyy-MM-dd HH:mm:ss} {1}' -f (Get-Date), $message)
  } catch {}
}

function Read-Config([string]$path) {
  if (-not (Test-Path $path)) { throw "There is no config.json beside the script ($path)." }
  $c = Get-Content -Raw -Encoding UTF8 $path | ConvertFrom-Json
  $id = '^[a-z0-9][a-z0-9_-]{0,63}$'
  if ($c.agentId -cnotmatch $id) { throw 'config.json: agentId must be the yard as the web app writes it, like amr-north.' }
  $endpoint = "$($c.endpoint)".TrimEnd('/')
  if ($endpoint -notmatch '^https://[^/]+/api/iot$') { throw 'config.json: endpoint must look like https://scrapkingsmart.com/api/iot.' }

  $scales = @(foreach ($s in @($c.scales)) {
      if ($null -eq $s -or $s.enabled -eq $false) { continue }
      if ($s.id -cnotmatch $id -or -not $s.name) { throw "config.json: each scale needs an id like inbound-1 and a name." }
      $ip = $null
      if (-not ($s.address -match '^(\d{1,3}(\.\d{1,3}){3}):(\d{1,5})$') -or
        -not [Net.IPAddress]::TryParse($Matches[1], [ref]$ip) -or [int]$Matches[3] -lt 1 -or [int]$Matches[3] -gt 65535) { throw "config.json: scale $($s.id): address must look like 192.168.1.12:10001." }
      @{
        id = $s.id; name = "$($s.name)"; address = $s.address
        ip = $Matches[1]; port = [int]$Matches[3]
        next = $null; text = ''; heardAt = [int64]-1
        weight = $null; unit = 'lb'; mode = $null; status = 'ok'
        sentKey = ''; sentAt = [int64]-100000
      }
    })
  $cameras = @(foreach ($cam in @($c.cameras)) {
      if ($null -eq $cam -or $cam.enabled -eq $false) { continue }
      if ($cam.id -cnotmatch $id -or -not $cam.name) { throw "config.json: each camera needs an id like scale-camera and a name." }
      if ($cam.snapshotUrl -notmatch '^https?://') { throw "config.json: camera $($cam.id): snapshotUrl must be the camera's http:// picture address." }
      $capture = $cam.snapshotUrl
      if ($cam.captureUrl) {
        if ($cam.captureUrl -notmatch '^https?://') { throw "config.json: camera $($cam.id): captureUrl must be the camera's http:// picture address." }
        $capture = $cam.captureUrl
      }
      $every = 1
      if ($cam.liveIntervalSeconds -gt 0) { $every = [double]$cam.liveIntervalSeconds }
      @{
        id = $cam.id; name = "$($cam.name)"; url = $cam.snapshotUrl; captureUrl = $capture; everyMs = [int64]($every * 1000)
        askedAt = [int64]-100000; ok = $false; detail = 'not asked yet'
      }
    })
  if ($scales.Count + $cameras.Count -eq 0) { throw 'config.json: no scale or camera is enabled.' }
  return @{ agentId = $c.agentId; endpoint = $endpoint; scales = $scales; cameras = $cameras }
}

function Test-Admin {
  $me = [Security.Principal.WindowsPrincipal][Security.Principal.WindowsIdentity]::GetCurrent()
  return $me.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)
}

# --- This PC's key -------------------------------------------------------------

# An ECDSA P-256 key in the machine's key store. Its private half cannot be
# exported, so it never leaves this PC; Administrators and SYSTEM (the agent)
# may use it.
function Get-AgentKey([switch]$Create) {
  $provider = [Security.Cryptography.CngProvider]::MicrosoftSoftwareKeyStorageProvider
  $machine = [Security.Cryptography.CngKeyOpenOptions]::MachineKey
  if ([Security.Cryptography.CngKey]::Exists($KeyName, $provider, $machine)) {
    return [Security.Cryptography.CngKey]::Open($KeyName, $provider, $machine)
  }
  if (-not $Create) { throw 'This PC has no key yet: run the install.' }

  $p = [Security.Cryptography.CngKeyCreationParameters]::new()
  $p.Provider = $provider
  $p.KeyCreationOptions = [Security.Cryptography.CngKeyCreationOptions]::MachineKey
  $p.ExportPolicy = [Security.Cryptography.CngExportPolicies]::None
  $p.KeyUsage = [Security.Cryptography.CngKeyUsages]::Signing
  $access = [Security.AccessControl.RawSecurityDescriptor]::new('D:P(A;;0xd01f01ff;;;SY)(A;;0xd01f01ff;;;BA)')
  $bytes = New-Object byte[] $access.BinaryLength
  $access.GetBinaryForm($bytes, 0)
  # Persist, plus DACL_SECURITY_INFORMATION (4): who may use the key.
  $options = [Enum]::ToObject([Security.Cryptography.CngPropertyOptions], ([int][Security.Cryptography.CngPropertyOptions]::Persist) -bor 4)
  $p.Parameters.Add([Security.Cryptography.CngProperty]::new('Security Descr', $bytes, $options))
  try {
    return [Security.Cryptography.CngKey]::Create([Security.Cryptography.CngAlgorithm]::ECDsaP256, $KeyName, $p)
  } catch {
    # Windows' default for a machine key made by an Administrator already lets
    # Administrators and SYSTEM use it.
    $p.Parameters.Clear()
    return [Security.Cryptography.CngKey]::Create([Security.Cryptography.CngAlgorithm]::ECDsaP256, $KeyName, $p)
  }
}

# The public key as the web app reads it: base64 DER (SubjectPublicKeyInfo).
function Get-PublicKey($key) {
  $blob = $key.Export([Security.Cryptography.CngKeyBlobFormat]::EccPublicBlob)
  if ($blob.Length -ne 72 -or [BitConverter]::ToInt32($blob, 4) -ne 32) { throw 'The key store returned an unexpected public key.' }
  $prefix = [byte[]](0x30, 0x59, 0x30, 0x13, 0x06, 0x07, 0x2A, 0x86, 0x48, 0xCE, 0x3D, 0x02, 0x01,
    0x06, 0x08, 0x2A, 0x86, 0x48, 0xCE, 0x3D, 0x03, 0x01, 0x07, 0x03, 0x42, 0x00, 0x04)
  return [Convert]::ToBase64String([byte[]]($prefix + $blob[8..71]))
}

# --- The web app ---------------------------------------------------------------

function New-HttpClient([int]$timeoutSeconds) {
  $client = [System.Net.Http.HttpClient]::new()
  $client.Timeout = [TimeSpan]::FromSeconds($timeoutSeconds)
  $client.DefaultRequestHeaders.UserAgent.ParseAdd("scrap-king-iot/$Version")
  return $client
}

function Invoke-Http($request) {
  try {
    $response = $script:Http.SendAsync($request).GetAwaiter().GetResult()
    $text = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
  } catch {
    throw "Cannot reach $($Config.endpoint): $(Get-Reason $_)"
  } finally {
    $request.Dispose()
  }
  $reason = "HTTP $([int]$response.StatusCode)"
  try { $parsed = $text | ConvertFrom-Json; if ($parsed.error) { $reason = $parsed.error } } catch {}
  return @{ status = [int]$response.StatusCode; body = $text; reason = $reason }
}

# Proves who this PC is, once, and keeps the token the web app answers.
function Open-Session {
  $timestamp = [string][DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
  $message = [Text.Encoding]::UTF8.GetBytes("scrap-king-iot-session`n$($Config.agentId)`n$timestamp")
  $signature = $script:Signer.SignData($message, [Security.Cryptography.HashAlgorithmName]::SHA256)
  $body = [ordered]@{ agentId = $Config.agentId; timestamp = $timestamp; signature = [Convert]::ToBase64String($signature) } | ConvertTo-Json -Compress
  $request = [System.Net.Http.HttpRequestMessage]::new([System.Net.Http.HttpMethod]::Post, "$($Config.endpoint)/session")
  $request.Content = [System.Net.Http.StringContent]::new($body, [Text.Encoding]::UTF8, 'application/json')
  $response = Invoke-Http $request
  if ($response.status -ne 200) { throw "The web app refused this PC: $($response.reason)" }
  $script:Token = ($response.body | ConvertFrom-Json).token
  Write-Log "Connected to $($Config.endpoint)"
}

# Posts to the web app with the token. A 401 means the web app restarted or
# the token lapsed: shake hands again and post again, once.
function Send-Body([string]$path, [byte[]]$bytes, [string]$type) {
  for ($attempt = 1; $attempt -le 2; $attempt++) {
    if (-not $script:Token) { Open-Session }
    $request = [System.Net.Http.HttpRequestMessage]::new([System.Net.Http.HttpMethod]::Post, "$($Config.endpoint)$path")
    $request.Headers.Authorization = [System.Net.Http.Headers.AuthenticationHeaderValue]::new('Bearer', $script:Token)
    $request.Content = [System.Net.Http.ByteArrayContent]::new($bytes)
    $request.Content.Headers.ContentType = [System.Net.Http.Headers.MediaTypeHeaderValue]::new($type)
    $response = Invoke-Http $request
    if ($response.status -ne 401) { return $response }
    $script:Token = $null
  }
  return $response
}

# Sends one event. Its answer says whether anyone still watches, and which
# full-size pictures logged tickets are waiting for.
function Send-Event([string]$query, [byte[]]$bytes, [string]$type) {
  $response = Send-Body "/events$query" $bytes $type
  if ($response.status -ne 200) { throw "The web app refused an event: $($response.reason)" }
  $answer = $response.body | ConvertFrom-Json
  foreach ($id in @($answer.capture)) { if ($id) { [void]$script:Captures.Add([string]$id) } }
  if (-not $answer.watching) { Set-Watching $false }
}

# A ticket was just logged: its full-size picture, from the first camera, for
# the web app to store with it. One that fails is logged and given up: the
# ticket is saved without it, and the save is not kept waiting.
function Send-Captures {
  if ($script:Captures.Count -eq 0) { return }
  $ids = $script:Captures.ToArray()
  $script:Captures.Clear()
  $cam = $Config.cameras | Select-Object -First 1
  foreach ($id in $ids) {
    try {
      if (-not $cam) { throw 'no camera is enabled' }
      $jpeg = $script:CaptureHttp.GetByteArrayAsync($cam.captureUrl).GetAwaiter().GetResult()
      $response = Send-Body "/capture?id=$([Uri]::EscapeDataString($id))" $jpeg 'image/jpeg'
      if ($response.status -ne 204) { throw "the web app answered $($response.reason)" }
      Write-Log ('Sent the {0:N0} KB picture for ticket {1}' -f ($jpeg.Length / 1KB), $id)
    } catch {
      Write-Log "No picture for ticket ${id}: $(Get-Reason $_)"
    }
  }
}

# While nobody has the yard's New Log open, this question is all the agent
# sends: is anyone watching? The web app holds it open for up to 25 s and
# answers the moment a page opens. It is asked without blocking the loop, so
# the scales are still read meanwhile.
function Wait-ForWatcher {
  if ($null -eq $script:WatchTask) {
    if (-not $script:Token) { Open-Session }
    $request = [System.Net.Http.HttpRequestMessage]::new([System.Net.Http.HttpMethod]::Get, "$($Config.endpoint)/watch")
    $request.Headers.Authorization = [System.Net.Http.Headers.AuthenticationHeaderValue]::new('Bearer', $script:Token)
    $script:WatchTask = $script:WatchHttp.SendAsync($request)
    return
  }
  if (-not $script:WatchTask.IsCompleted) { return }
  $task = $script:WatchTask
  $script:WatchTask = $null
  if ($task.IsFaulted -or $task.IsCanceled) {
    $why = 'no answer in time'
    if ($task.Exception) { $why = $task.Exception.GetBaseException().Message }
    throw "Cannot reach $($Config.endpoint): $why"
  }
  $response = $task.Result
  $status = [int]$response.StatusCode
  $text = $response.Content.ReadAsStringAsync().GetAwaiter().GetResult()
  $response.Dispose()
  if ($status -eq 401) { $script:Token = $null; return } # the web app restarted: shake hands again
  if ($status -ne 200) { throw "The web app refused this PC's question: HTTP $status" }
  if (($text | ConvertFrom-Json).watching) { Set-Watching $true }
}

function Set-Watching([bool]$watching) {
  if ($watching -eq $script:Watching) { return }
  $script:Watching = $watching
  if ($watching) {
    Write-Log 'Someone opened New Log: sending'
    # Everything at once, so the page fills straight away.
    foreach ($s in $Config.scales) { $s.sentAt = [int64]-100000 }
    foreach ($cam in $Config.cameras) { $cam.askedAt = [int64]-100000 }
  } else {
    Write-Log 'Nobody is watching: sending nothing'
  }
}

# --- The scales ----------------------------------------------------------------

# A raw socket on the address ScrapRight's connection to the device server
# uses: Windows hands it a copy of every packet through that address, which is
# how the readings are heard without connecting to anything.
function Open-Tap {
  $ips = @($Config.scales | ForEach-Object { $_.ip } | Select-Object -Unique)
  if ($ips.Count -eq 0) { return $null }
  $connection = Get-NetTCPConnection -State Established -ErrorAction SilentlyContinue |
    Where-Object { $ips -contains $_.RemoteAddress } | Select-Object -First 1
  if (-not $connection) {
    $script:TapProblem = "No connection from this PC to $($ips -join ', '). Is ScrapRight's IoT Agent running?"
    return $null
  }
  $socket = $null
  try {
    $socket = [Net.Sockets.Socket]::new([Net.Sockets.AddressFamily]::InterNetwork, [Net.Sockets.SocketType]::Raw, [Net.Sockets.ProtocolType]::IP)
    $socket.ReceiveBufferSize = 4MB
    $socket.Bind([Net.IPEndPoint]::new([Net.IPAddress]::Parse($connection.LocalAddress), 0))
    try {
      # RCVALL_IPLEVEL: the packets this PC receives, with the network card
      # left out of promiscuous mode.
      [void]$socket.IOControl([Net.Sockets.IOControlCode]::ReceiveAll, [BitConverter]::GetBytes([int]3), $null)
    } catch {
      # RCVALL_ON, for a network card that does not offer the first.
      [void]$socket.IOControl([Net.Sockets.IOControlCode]::ReceiveAll, [BitConverter]::GetBytes([int]1), $null)
    }
  } catch {
    if ($null -ne $socket) { $socket.Close() }
    $script:TapProblem = "Cannot listen on $($connection.LocalAddress): $(Get-Reason $_)"
    return $null
  }
  $script:TapProblem = $null
  $script:TapIp = [BitConverter]::ToUInt32([Net.IPAddress]::Parse($connection.LocalAddress).GetAddressBytes(), 0)
  Write-Log "Listening for the scales on $($connection.LocalAddress)"
  return $socket
}

# Reads every packet waiting on the tap and passes the scales' bytes on.
function Read-Tap($tap, [int64]$now) {
  $p = $script:Packet
  $limit = 5000
  while ($tap.Available -gt 0 -and $limit-- -gt 0) {
    $n = $tap.Receive($p)
    # TCP, addressed to this PC
    if ($n -lt 40 -or $p[9] -ne 6 -or [BitConverter]::ToUInt32($p, 16) -ne $script:TapIp) { continue }
    $ports = $script:ScalesByIp[[BitConverter]::ToUInt32($p, 12)]
    if ($null -eq $ports) { continue }
    $ihl = ($p[0] -band 15) * 4
    $scale = $ports[[int]$p[$ihl] * 256 + $p[$ihl + 1]]
    if ($null -eq $scale) { continue }
    $start = $ihl + ($p[$ihl + 12] -shr 4) * 4
    $end = [Math]::Min($n, [int]$p[2] * 256 + $p[3])
    if ($end -le $start) { continue }
    $seq = [int64]$p[$ihl + 4] * 16777216 + [int64]$p[$ihl + 5] * 65536 + [int64]$p[$ihl + 6] * 256 + $p[$ihl + 7]
    Add-Bytes $scale $seq $p $start ($end - $start) $now
  }
}

# Puts a TCP segment's bytes back in order. Windows may hand over the same
# segment more than once, and the device server splits a reading across
# segments, so bytes are placed by sequence number and each is read once.
function Add-Bytes($scale, [int64]$seq, [byte[]]$p, [int]$start, [int]$length, [int64]$now) {
  if ($null -ne $scale.next) {
    $d = $seq - $scale.next
    if ($d -gt 2147483647) { $d -= 4294967296 } elseif ($d -lt -2147483648) { $d += 4294967296 }
    if ($d -lt -65536 -or $d -gt 65536) { $scale.text = '' }          # a new connection: start over
    elseif ($d + $length -le 0) { return }                             # a copy of bytes already read
    elseif ($d -lt 0) { $start -= $d; $length += $d; $seq = $scale.next } # keep only the new tail
    elseif ($d -gt 0) { $scale.text = '' }                             # bytes missed: drop the partial line
  }
  $scale.next = ($seq + $length) % 4294967296
  $scale.text += [Text.Encoding]::ASCII.GetString($p, $start, $length)

  # A reading is 14 characters: STX, sign, 7 for the weight, unit, mode,
  # status, CR, LF. For example STX + '    2840LG ' + CR LF.
  while ($true) {
    $at = $scale.text.IndexOf([char]2)
    if ($at -lt 0) { $scale.text = ''; return }
    if ($scale.text.Length - $at -lt 14) { $scale.text = $scale.text.Substring($at); return }
    $line = $scale.text.Substring($at, 14)
    if ($line.Substring(12) -ne "`r`n") { $scale.text = $scale.text.Substring($at + 1); continue }
    $scale.text = $scale.text.Substring($at + 14)

    $weight = 0.0
    $number = $line.Substring(2, 7).Trim()
    if ([double]::TryParse($number, [Globalization.NumberStyles]::Float, [Globalization.CultureInfo]::InvariantCulture, [ref]$weight)) {
      if ($line[1] -eq '-') { $weight = -$weight }
      $scale.weight = $weight
      $status = $Statuses[[string]$line[11]]
      if ($null -eq $status) { $status = 'ok' }
      $scale.status = $status
    } else {
      $scale.weight = $null
      $scale.status = 'overload'
    }
    $unit = $Units[[string]$line[9]]
    if ($null -eq $unit) { $unit = 'lb' }
    $scale.unit = $unit
    $scale.mode = $Modes[[string]$line[10]]
    $scale.heardAt = $now
    $script:LastReadingAt = $now
  }
}

function Send-Weights([int64]$now) {
  foreach ($s in $Config.scales) {
    if (-not $script:Watching) { return }
    if ($s.heardAt -lt 0 -or $now - $s.heardAt -gt $QuietAfterMs) { continue }
    $key = "$($s.weight)|$($s.unit)|$($s.mode)|$($s.status)"
    $since = $now - $s.sentAt
    if (($key -ne $s.sentKey -and $since -ge $SendAfterChangeMs) -or $since -ge $SendEveryMs) {
      $s.sentAt = $now
      $reading = [ordered]@{ scaleId = $s.id; name = $s.name; weight = $s.weight; unit = $s.unit; mode = $s.mode; status = $s.status }
      Send-Event '' ([Text.Encoding]::UTF8.GetBytes(($reading | ConvertTo-Json -Compress))) 'application/json'
      $s.sentKey = $key
    }
  }
}

# --- The camera ----------------------------------------------------------------

# While someone watches, each camera is asked for a picture every
# liveIntervalSeconds and the picture is sent. Otherwise it is asked once a
# minute, on this network only, so the check can say whether it works; nothing
# is uploaded.
function Update-Cameras([int64]$now) {
  foreach ($cam in $Config.cameras) {
    $every = 60000
    if ($script:Watching) { $every = $cam.everyMs }
    if ($now - $cam.askedAt -lt $every) { continue }
    $cam.askedAt = $now
    try {
      $jpeg = $script:CameraHttp.GetByteArrayAsync($cam.url).GetAwaiter().GetResult()
    } catch {
      $cam.ok = $false
      $cam.detail = "The camera did not answer: $(Get-Reason $_)"
      continue
    }
    $cam.ok = $true
    $cam.detail = '{0:N0} KB picture' -f ($jpeg.Length / 1KB)
    if ($script:Watching) {
      $query = "?camera=$($cam.id)&name=$([Uri]::EscapeDataString($cam.name))"
      Send-Event $query $jpeg 'image/jpeg'
    }
  }
}

# --- Status, for the check -------------------------------------------------------

function Write-Status([int64]$now) {
  $lines = New-Object System.Collections.ArrayList
  if ($script:WebAppProblem) { $web = @{ ok = $false; detail = $script:WebAppProblem } }
  elseif ($script:Token -and $script:Watching) { $web = @{ ok = $true; detail = "$($Config.endpoint): New Log is open, sending" } }
  elseif ($script:Token) { $web = @{ ok = $true; detail = "$($Config.endpoint): nobody watching, sending nothing" } }
  else { $web = @{ ok = $false; detail = 'not connected yet' } }
  [void]$lines.Add(@{ what = 'web app'; ok = $web.ok; detail = $web.detail })

  foreach ($s in $Config.scales) {
    $heard = $s.heardAt -ge 0 -and $now - $s.heardAt -le $QuietAfterMs
    if ($heard) { $detail = "$($s.weight) $($s.unit) $($s.mode), $($s.status)" }
    elseif ($script:TapProblem) { $detail = $script:TapProblem }
    elseif ($s.heardAt -lt 0) { $detail = "No readings yet from $($s.address)." }
    else { $detail = "No readings for $([Math]::Floor(($now - $s.heardAt) / 1000)) s from $($s.address)." }
    [void]$lines.Add(@{ what = "scale $($s.id)"; ok = $heard; detail = $detail })
  }
  foreach ($cam in $Config.cameras) {
    [void]$lines.Add(@{ what = "camera $($cam.id)"; ok = $cam.ok; detail = $cam.detail })
  }

  $status = @{
    at = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
    startedAt = $script:StartedAt
    version = $Version
    lines = $lines.ToArray()
  }
  $temp = "$StatusFile.tmp"
  Set-Content -Path $temp -Encoding UTF8 -Value ($status | ConvertTo-Json -Depth 4)
  Move-Item -Force $temp $StatusFile
}

# --- Commands --------------------------------------------------------------------

function Start-Agent {
  Add-Type -AssemblyName System.Net.Http
  [Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
  # Otherwise .NET holds every POST body back for up to 350 ms.
  [Net.ServicePointManager]::Expect100Continue = $false
  $script:Config = Read-Config (Join-Path $PSScriptRoot 'config.json')
  $script:Signer = [Security.Cryptography.ECDsaCng]::new((Get-AgentKey))
  $script:Http = New-HttpClient 10
  $script:CameraHttp = New-HttpClient 3
  # A full-size picture is larger and a ticket is waiting on it: a little longer.
  $script:CaptureHttp = New-HttpClient 6
  $script:Captures = New-Object System.Collections.Generic.List[string]
  # Longer than the 25 s the web app holds a question about watching.
  $script:WatchHttp = New-HttpClient 40
  $script:WatchTask = $null
  $script:Watching = $false
  $script:Packet = New-Object byte[] 65535
  $script:Token = $null
  $script:WebAppProblem = $null
  $script:TapProblem = $null
  $script:LastReadingAt = [int64]0
  $script:StartedAt = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()

  # Scales by the device server's address (as Windows writes it in a packet)
  # and port, so a packet that is not a reading is passed over at once.
  $script:ScalesByIp = @{}
  foreach ($s in $Config.scales) {
    $ipKey = [BitConverter]::ToUInt32([Net.IPAddress]::Parse($s.ip).GetAddressBytes(), 0)
    if (-not $ScalesByIp.ContainsKey($ipKey)) { $ScalesByIp[$ipKey] = @{} }
    $ScalesByIp[$ipKey][$s.port] = $s
  }

  Write-Log "Started $Version for $($Config.agentId), sending to $($Config.endpoint)"
  $clock = [Diagnostics.Stopwatch]::StartNew()
  $tap = $null
  $tapAt = [int64]-100000
  $statusAt = [int64]-100000
  $holdUntil = [int64]0

  while ($true) {
    $now = $clock.ElapsedMilliseconds

    if ($null -eq $tap -and $Config.scales.Count -gt 0 -and $now - $tapAt -ge 10000) {
      $tapAt = $now
      $tap = Open-Tap
    }
    if ($null -ne $tap) {
      try { Read-Tap $tap $now } catch { $script:TapProblem = "Listening failed: $(Get-Reason $_)"; $tap.Close(); $tap = $null }
      # Nothing from any scale for 30 s: ScrapRight may have reconnected on
      # another address. Look again.
      if ($null -ne $tap -and $now - $script:LastReadingAt -gt 30000 -and $now - $tapAt -gt 30000) {
        $tap.Close()
        $tap = $null
      }
    }

    if ($now -ge $holdUntil) {
      try {
        if ($script:Watching) { Send-Weights $now } else { Wait-ForWatcher }
        Update-Cameras $now
        Send-Captures
        if ($script:WebAppProblem) { Write-Log 'The web app is taking events again.' }
        $script:WebAppProblem = $null
      } catch {
        $reason = Get-Reason $_
        if ($reason -ne $script:WebAppProblem) { Write-Log $reason }
        $script:WebAppProblem = $reason
        # Wait before trying again; longer when this PC itself was refused.
        $wait = 5000
        if ($reason -like 'The web app refused this PC*') { $wait = 30000 }
        $holdUntil = $now + $wait
      }
    }

    if ($now - $statusAt -ge 2000) {
      $statusAt = $now
      try { Write-Status $now } catch {}
    }
    Start-Sleep -Milliseconds 50
  }
}

# Prints what the running agent last saw. True when everything works.
function Show-Check {
  if (-not (Test-Path $StatusFile)) {
    Write-Host '  FAIL  agent         has never run on this PC: run the install'
    return $false
  }
  try { $status = Get-Content -Raw -Encoding UTF8 $StatusFile | ConvertFrom-Json }
  catch { Write-Host "  FAIL  agent         its status file could not be read: $(Get-Reason $_)"; return $false }
  $age = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds() - $status.at
  $good = $true
  if ($age -gt 15) {
    Write-Host "  FAIL  agent         not running: last seen $age s ago (see $LogFile)"
    $good = $false
  }
  foreach ($line in $status.lines) {
    $mark = 'FAIL'
    if ($line.ok) { $mark = 'ok  ' }
    Write-Host ('  {0}  {1,-18}  {2}' -f $mark, $line.what, $line.detail)
    if (-not $line.ok) { $good = $false }
  }
  return $good
}

# Stops the agent and removes it from Task Scheduler, including a copy still
# running from before.
function Stop-Agent {
  if (Get-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue) {
    Stop-ScheduledTask -TaskName $TaskName -ErrorAction SilentlyContinue
    Unregister-ScheduledTask -TaskName $TaskName -Confirm:$false
  }
  $old = @(Get-CimInstance Win32_Process -Filter "Name = 'powershell.exe'" |
      Where-Object { $_.ProcessId -ne $PID -and $_.CommandLine -like '*scrap-king-iot.ps1" run*' })
  foreach ($proc in $old) { Stop-Process -Id $proc.ProcessId -Force -ErrorAction SilentlyContinue }
  foreach ($proc in $old) { Wait-Process -Id $proc.ProcessId -Timeout 10 -ErrorAction SilentlyContinue }
}

function Install-Agent {
  $config = Read-Config (Join-Path $PSScriptRoot 'config.json')
  $began = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()

  Write-Host "Copying the agent to $InstallDir"
  New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
  if ((Resolve-Path $PSScriptRoot).Path.TrimEnd('\') -ne $InstallDir.TrimEnd('\')) {
    Copy-Item -Force -Path $PSCommandPath -Destination $ScriptFile
    Copy-Item -Force -Path (Join-Path $PSScriptRoot 'config.json') -Destination $InstallDir
  }
  Get-ChildItem $InstallDir | Unblock-File

  Write-Host "Making this PC's key (once; later installs keep it)"
  $publicKey = Get-PublicKey (Get-AgentKey -Create)

  Write-Host 'Letting the agent hear the scales through Windows Firewall'
  Get-NetFirewallRule -DisplayName $TaskName -ErrorAction SilentlyContinue | Remove-NetFirewallRule
  $deviceIps = @($config.scales | ForEach-Object { $_.ip } | Select-Object -Unique)
  if ($deviceIps.Count -gt 0) {
    New-NetFirewallRule -DisplayName $TaskName -Direction Inbound -Action Allow -Profile Any `
      -Program $PowerShellExe -RemoteAddress $deviceIps | Out-Null
  }

  Write-Host 'Keeping the PC awake while plugged in'
  powercfg /change standby-timeout-ac 0 | Out-Null
  powercfg /change hibernate-timeout-ac 0 | Out-Null

  Write-Host 'Starting the agent now and at every boot'
  Stop-Agent
  Remove-Item -Force $StatusFile -ErrorAction SilentlyContinue
  $action = New-ScheduledTaskAction -Execute $PowerShellExe -WorkingDirectory $InstallDir `
    -Argument "-NoProfile -NonInteractive -WindowStyle Hidden -ExecutionPolicy Bypass -File `"$ScriptFile`" run"
  # At boot, and every minute after: a minute's check that starts the agent
  # again if it ever stops. A running agent makes it a no-op (IgnoreNew).
  $triggers = @(
    (New-ScheduledTaskTrigger -AtStartup),
    (New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(1) -RepetitionInterval (New-TimeSpan -Minutes 1))
  )
  $principal = New-ScheduledTaskPrincipal -UserId 'NT AUTHORITY\SYSTEM' -LogonType ServiceAccount -RunLevel Highest
  $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable
  Register-ScheduledTask -TaskName $TaskName -Action $action -Trigger $triggers -Principal $principal -Settings $settings -Force | Out-Null
  $task = Get-ScheduledTask -TaskName $TaskName
  if ($task.Triggers[1].Repetition.Duration) {
    # Some Windows builds store an end to the repetition; remove it.
    $task.Triggers[1].Repetition.Duration = ''
    try { $task | Set-ScheduledTask | Out-Null } catch {}
  }
  Start-ScheduledTask -TaskName $TaskName

  Write-Host 'Checking (10 seconds)'
  $deadline = (Get-Date).AddSeconds(25)
  do {
    Start-Sleep -Seconds 1
    $status = $null
    try { $status = Get-Content -Raw -Encoding UTF8 $StatusFile | ConvertFrom-Json } catch {}
  } until (((Get-Date) -gt $deadline) -or ($status -and $status.startedAt -ge $began -and $status.at - $status.startedAt -ge 8))
  Write-Host ''
  $good = Show-Check

  $entry = "$($config.agentId):$publicKey"
  Set-Content -Path (Join-Path $InstallDir 'public-key.txt') -Encoding ASCII -Value $entry
  Write-Host ''
  Write-Host '========================================================================'
  Write-Host " This PC's public key. Add this line to the web app's IOT_AGENT_KEYS"
  Write-Host ' (once; it stays the same on every later install):'
  Write-Host ''
  Write-Host "   $entry"
  Write-Host ''
  Write-Host " Also saved in $InstallDir\public-key.txt"
  Write-Host '========================================================================'
  if ($good) { Write-Host 'GOOD TO GO' -ForegroundColor Green }
  else { Write-Host 'NOT READY: see the FAIL lines above' -ForegroundColor Yellow }
}

function Uninstall-Agent {
  Set-Location $env:SystemRoot
  Stop-Agent
  Get-NetFirewallRule -DisplayName $TaskName -ErrorAction SilentlyContinue | Remove-NetFirewallRule
  if (Test-Path $InstallDir) { Remove-Item -Recurse -Force $InstallDir }
  Write-Host "Removed. This PC's key is kept, so installing again needs no change in the web app."
}

switch ($Command) {
  'run' {
    try { Start-Agent } catch { Write-Log "Stopped: $(Get-Reason $_)"; exit 1 }
  }
  'check' {
    if (Show-Check) { Write-Host 'GOOD TO GO' -ForegroundColor Green; exit 0 }
    Write-Host 'NOT READY' -ForegroundColor Yellow
    exit 1
  }
  default {
    if (-not (Test-Admin)) {
      # Ask for Administrator in a new window that stays open to show the key.
      Start-Process -FilePath $PowerShellExe -Verb RunAs `
        -ArgumentList "-NoProfile -NoExit -ExecutionPolicy Bypass -File `"$PSCommandPath`" $Command"
      exit
    }
    if ($Command -eq 'install') { Install-Agent } else { Uninstall-Agent }
  }
}
