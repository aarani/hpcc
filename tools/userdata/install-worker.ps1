# AWS EC2 userdata — installs and supervises an hpcc worker on
# Windows Server 2022. Paste verbatim into the userdata field
# (wrapped in <powershell>...</powershell>); edit the parameter
# block first.
#
# Pulls every binary from a tagged hpcc release; `hpcc init worker
# --runtime runhcs-wcow-hypervisor` then writes worker.toml + a
# self-signed TLS leaf and runs Validate() before this script registers
# the scheduled task. Hyper-V isolation requires nested virt (bare-metal
# *.metal instance types).
$ErrorActionPreference = "Stop"

# ---- parameters -------------------------------------------------------
$HpccVersion    = "v0.1.0-alpha"
$HpccRepo       = "aarani/hpcc"
$ContainerdVer  = "1.7.20"   # containerd release version (windows-amd64)

# Scheduler pairing — paste the worker_token from `hpcc init scheduler`.
$SchedulerUrl   = "scheduler.internal:9091"
$WorkerToken    = "replace-with-a-long-random-string"

# Listening. PublicAddr is auto-discovered via IMDSv2 when empty.
$PublicAddr     = ""
$Listen         = ":9092"
$MetricsListen  = ":9192"

# Per-tenant Hyper-V container sizing. Tune per instance type;
# `hpcc init worker`'s defaults (2GB / 4 vCPUs / 32 pool) are
# conservative.
$VmMemory       = "2GB"
$VmVcpus        = 4
$PoolMaxActive  = 32

# Isolation mode. "hyperv" is the production value (kernel boundary
# per container); "process" loses the boundary and is only valid on
# hosts without nested virt (CI hosted runners, dev laptops).
$Isolation      = "hyperv"

# ---- paths ------------------------------------------------------------
$Root           = "C:\Program Files\hpcc"
$ContainerdDir  = "C:\Program Files\containerd"
$Conf           = "C:\ProgramData\hpcc"
$RunDir         = Join-Path $Conf "run"
$CtrdState      = "C:\ProgramData\containerd"
New-Item -ItemType Directory -Force `
  -Path $Root,$ContainerdDir,$Conf,$RunDir,$CtrdState | Out-Null

# ---- enable Windows features (Containers + Hyper-V) -------------------
# Both need a reboot before containerd can use them; the script
# triggers exactly one reboot at the very end via exit 3010.
Install-WindowsFeature -Name Containers | Out-Null
Install-WindowsFeature -Name Hyper-V -IncludeManagementTools | Out-Null

# ---- install containerd ----------------------------------------------
$ctrdUrl = "https://github.com/containerd/containerd/releases/download/v$ContainerdVer/containerd-$ContainerdVer-windows-amd64.tar.gz"
$ctrdTar = Join-Path $env:TEMP "containerd.tar.gz"
Invoke-WebRequest -UseBasicParsing -Uri $ctrdUrl -OutFile $ctrdTar
& tar.exe -xzf $ctrdTar -C $ContainerdDir
Remove-Item $ctrdTar

$ContainerdExe = (Get-ChildItem -Path $ContainerdDir -Recurse -Filter containerd.exe |
                  Select-Object -First 1).FullName
if (-not $ContainerdExe) { throw "containerd.exe not found in archive" }

# Put containerd's bin dir on the system PATH so ctr.exe and the runhcs
# shim resolve by name.
$ContainerdBinDir = Split-Path $ContainerdExe -Parent
$sysPath = [Environment]::GetEnvironmentVariable("Path","Machine")
if ($sysPath -notlike "*$ContainerdBinDir*") {
  [Environment]::SetEnvironmentVariable("Path","$sysPath;$ContainerdBinDir","Machine")
  $env:Path = "$env:Path;$ContainerdBinDir"
}

# Default containerd config, with root/state moved off C:\Program Files.
$CtrdConfig = Join-Path $CtrdState "config.toml"
& $ContainerdExe config default | Out-File -Encoding ascii -FilePath $CtrdConfig
(Get-Content $CtrdConfig) `
  -replace '^root\s*=.*',  ("root = `"" + ($CtrdState -replace '\\','\\\\') + "\\root`"") `
  -replace '^state\s*=.*', ("state = `"" + ($CtrdState -replace '\\','\\\\') + "\\state`"") |
  Set-Content $CtrdConfig -Encoding ascii

# Register as a Windows service; auto-start on next boot (post-reboot
# the Containers/Hyper-V features are live).
& $ContainerdExe --register-service --config $CtrdConfig
Set-Service -Name containerd -StartupType Automatic

# ---- install hpcc.exe + stage hpcc-agent.exe -------------------------
$HpccUrl = "https://github.com/$HpccRepo/releases/download/$HpccVersion/hpcc-$HpccVersion-windows-amd64.zip"
$tmpZip  = Join-Path $env:TEMP "hpcc.zip"
Invoke-WebRequest -UseBasicParsing -Uri $HpccUrl -OutFile $tmpZip
Expand-Archive -Force -Path $tmpZip -DestinationPath $Root
Remove-Item $tmpZip

$HpccExe = Join-Path $Root "hpcc.exe"
if (-not (Test-Path $HpccExe)) { throw "hpcc.exe not found in release zip" }

# `hpcc init worker` writes the agent path C:\ProgramData\hpcc\hpcc-agent.exe
# into worker.toml by default; stage the binary there so the path
# resolves on first start.
Copy-Item -Path (Join-Path $Root "hpcc-agent.exe") `
          -Destination (Join-Path $Conf "hpcc-agent.exe") -Force

# ---- public_addr via IMDSv2 if not set --------------------------------
if (-not $PublicAddr) {
  $tok = Invoke-RestMethod -Method Put -Uri "http://169.254.169.254/latest/api/token" `
    -Headers @{ "X-aws-ec2-metadata-token-ttl-seconds" = "60" }
  $ip  = Invoke-RestMethod -Uri "http://169.254.169.254/latest/meta-data/local-ipv4" `
    -Headers @{ "X-aws-ec2-metadata-token" = $tok }
  $PublicAddr = "${ip}${Listen}"
}

# ---- generate worker.toml + TLS via `hpcc init worker` ----------------
$ConfFile = Join-Path $Conf "worker.toml"
& $HpccExe init worker `
  --config $ConfFile `
  --force `
  --scheduler $SchedulerUrl `
  --token $WorkerToken `
  --public-addr $PublicAddr `
  --listen $Listen `
  --metrics-listen $MetricsListen `
  --runtime runhcs-wcow-hypervisor
if ($LASTEXITCODE -ne 0) { throw "hpcc init worker failed" }

# ---- patch ops-tunable fields init can't parameterize today ----------
$content = Get-Content $ConfFile -Raw
$content = $content `
  -replace '(?m)^memory\s+= ".*"',  "memory          = `"$VmMemory`"" `
  -replace '(?m)^vcpus\s+= \d+',    "vcpus           = $VmVcpus" `
  -replace '(?m)^max_active = \d+', "max_active = $PoolMaxActive"
if ($Isolation -ne "hyperv") {
  $content = $content -replace '(?m)^isolation\s+= ".*"', "isolation   = `"$Isolation`""
}
Set-Content -Path $ConfFile -Value $content -Encoding utf8

# ---- supervise hpcc via Scheduled Task --------------------------------
# hpcc.exe is a console program, not a Windows service. A scheduled
# task running as SYSTEM at boot with restart-on-failure is the
# zero-dependency way to keep it alive across crashes and reboots.
$TaskName = "hpcc-worker"
schtasks.exe /Delete /TN $TaskName /F 2>$null | Out-Null

$Action    = New-ScheduledTaskAction -Execute $HpccExe `
              -Argument "worker --config `"$ConfFile`"" `
              -WorkingDirectory $Conf
$Trigger   = New-ScheduledTaskTrigger -AtStartup
$Principal = New-ScheduledTaskPrincipal -UserId "SYSTEM" -RunLevel Highest
$Settings  = New-ScheduledTaskSettingsSet `
              -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries `
              -ExecutionTimeLimit ([TimeSpan]::Zero) `
              -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1) `
              -MultipleInstances IgnoreNew

Register-ScheduledTask -TaskName $TaskName `
  -Action $Action -Trigger $Trigger `
  -Principal $Principal -Settings $Settings | Out-Null

# Containers/Hyper-V need a reboot to activate. Both services
# (containerd + the hpcc-worker scheduled task) are set to auto-start
# on the next boot. Exit 3010 — the Windows installer "reboot
# required" code — which EC2Launch / EC2Launch v2 recognises and acts
# on. Never call Restart-Computer from inside userdata: it tears down
# the launcher mid-run and leaves the instance in an undefined state.
exit 3010
