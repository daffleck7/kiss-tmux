<#
.SYNOPSIS
    Restart KISS-TMUX (optionally swapping in a new build) and resume every Claude
    conversation that was open in it.

.DESCRIPTION
    1. Snapshot: finds the claude.exe processes started by the running kiss-tmux.exe and
       reads Claude Code's session registry (~/.claude/sessions/<pid>.json) to get each
       conversation's ID and folder. The snapshot is saved so it survives the restart.
    2. Swap (runs in its own window, so it outlives KISS-TMUX): stops KISS-TMUX, backs up
       kiss-tmux.exe to kiss-tmux.exe~, moves kiss-tmux-new.exe into place if present, and
       starts KISS-TMUX again.
    3. Restore: opens one session per conversation over the WebSocket, running the original
       command with "--resume <id>".

    Remote (Mac) sessions keep running inside screen and are rediscovered automatically.

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File scripts\restart-and-resume.ps1
.EXAMPLE
    powershell -ExecutionPolicy Bypass -File scripts\restart-and-resume.ps1 -DryRun
#>
param(
    [string]$ExePath = (Join-Path (Split-Path -Parent $PSScriptRoot) 'kiss-tmux.exe'),
    [string]$NewExe = (Join-Path (Split-Path -Parent $PSScriptRoot) 'kiss-tmux-new.exe'),
    [int]$Port = 7777,
    [string]$StateFile = (Join-Path $env:LOCALAPPDATA 'kiss-tmux\restore.json'),
    [ValidateSet('snapshot', 'swap', 'restore')]
    [string]$Phase = 'snapshot',
    [switch]$Force,
    [switch]$DryRun
)

$ErrorActionPreference = 'Stop'
$SessionRegistry = Join-Path $HOME '.claude\sessions'

function Get-KissProcesses {
    <# Returns kiss-tmux.exe processes started from $ExePath. #>
    Get-CimInstance Win32_Process -Filter "Name='kiss-tmux.exe'" |
        Where-Object { $_.ExecutablePath -eq $ExePath }
}

function Get-ResumeCommand([string]$CommandLine, [string]$SessionId) {
    <# Strips any existing resume/continue flags and appends --resume <id>. #>
    $tokens = @($CommandLine -split '\s+' | Where-Object { $_ -ne '' })
    $kept = New-Object System.Collections.Generic.List[string]
    for ($i = 0; $i -lt $tokens.Count; $i++) {
        $token = $tokens[$i]
        if ($token -in @('--continue', '-c')) { continue }
        if ($token -in @('--resume', '-r')) {
            if ($i + 1 -lt $tokens.Count -and -not $tokens[$i + 1].StartsWith('-')) { $i++ }
            continue
        }
        $kept.Add($token)
    }
    if ($kept.Count -eq 0) { $kept.Add('claude') }
    return (($kept -join ' ') + " --resume $SessionId")
}

function Get-Snapshot {
    <# Builds the list of conversations to restore from the running KISS-TMUX. #>
    $entries = New-Object System.Collections.Generic.List[object]
    foreach ($kiss in @(Get-KissProcesses)) {
        $children = Get-CimInstance Win32_Process -Filter "ParentProcessId=$($kiss.ProcessId)" |
            Where-Object { $_.Name -ne 'conhost.exe' }
        foreach ($child in $children) {
            $registryFile = Join-Path $SessionRegistry "$($child.ProcessId).json"
            if ($child.Name -ne 'claude.exe' -or -not (Test-Path $registryFile)) {
                Write-Warning "Can't restore '$($child.CommandLine)' (pid $($child.ProcessId)): not a Claude session."
                continue
            }
            $info = Get-Content $registryFile -Raw | ConvertFrom-Json
            $entries.Add([pscustomobject]@{
                    pid       = $child.ProcessId
                    sessionId = $info.sessionId
                    cwd       = $info.cwd
                    name      = $info.name
                    status    = $info.status
                    startedAt = $info.startedAt
                    cmd       = Get-ResumeCommand $child.CommandLine $info.sessionId
                })
        }
    }
    return @($entries | Sort-Object startedAt)
}

function Wait-Port([int]$TcpPort, [int]$TimeoutSeconds) {
    <# Waits until something accepts connections on 127.0.0.1:$TcpPort. #>
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        $client = New-Object System.Net.Sockets.TcpClient
        try {
            $client.Connect('127.0.0.1', $TcpPort)
            return $true
        } catch [System.Net.Sockets.SocketException] {
            Start-Sleep -Milliseconds 500
        } finally {
            $client.Dispose()
        }
    }
    return $false
}

function Send-WsJson($Socket, $Object) {
    <# Sends one JSON text message. #>
    $json = $Object | ConvertTo-Json -Compress
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($json)
    $segment = New-Object System.ArraySegment[byte] -ArgumentList (, $bytes)
    $Socket.SendAsync($segment, [System.Net.WebSockets.WebSocketMessageType]::Text, $true,
        [System.Threading.CancellationToken]::None).GetAwaiter().GetResult() | Out-Null
}

function Receive-SpawnReplies($Socket, [int]$Expected, [int]$TimeoutSeconds) {
    <# Collects 'spawned' and 'error' replies until $Expected arrive or time runs out. #>
    $replies = New-Object System.Collections.Generic.List[object]
    $buffer = New-Object byte[] 65536
    $message = New-Object System.IO.MemoryStream
    $cts = New-Object System.Threading.CancellationTokenSource
    $cts.CancelAfter($TimeoutSeconds * 1000)
    try {
        while ($replies.Count -lt $Expected -and $Socket.State -eq 'Open') {
            $segment = New-Object System.ArraySegment[byte] -ArgumentList (, $buffer)
            $result = $Socket.ReceiveAsync($segment, $cts.Token).GetAwaiter().GetResult()
            $message.Write($buffer, 0, $result.Count)
            if (-not $result.EndOfMessage) { continue }
            $text = [System.Text.Encoding]::UTF8.GetString($message.ToArray())
            $message.SetLength(0)
            if ($text.StartsWith('{"type":"spawned"') -or $text.StartsWith('{"type":"error"')) {
                $replies.Add(($text | ConvertFrom-Json))
            }
        }
    } catch [System.OperationCanceledException] {
        Write-Warning "Timed out waiting for replies ($($replies.Count) of $Expected)."
    }
    return $replies
}

function Restore-Sessions($Entries) {
    <# Opens one KISS-TMUX session per saved conversation. #>
    if ($Entries.Count -eq 0) {
        Write-Host 'No Claude conversations to restore.'
        return
    }
    if (-not (Wait-Port $Port 30)) { throw "KISS-TMUX didn't start listening on port $Port." }
    $socket = New-Object System.Net.WebSockets.ClientWebSocket
    $socket.ConnectAsync([Uri]"ws://127.0.0.1:$Port/ws",
        [System.Threading.CancellationToken]::None).GetAwaiter().GetResult() | Out-Null
    foreach ($entry in $Entries) {
        Write-Host "Resuming '$($entry.name)' in $($entry.cwd)"
        Send-WsJson $socket ([ordered]@{
                type = 'spawn'; cmd = $entry.cmd; cwd = $entry.cwd; name = $entry.name
                cols = 120; rows = 30
            })
    }
    $replies = Receive-SpawnReplies $socket $Entries.Count 20
    $spawned = @($replies | Where-Object { $_.type -eq 'spawned' }).Count
    foreach ($failure in @($replies | Where-Object { $_.type -eq 'error' })) {
        Write-Warning "A session failed to open: $($failure.error)"
    }
    Write-Host "Restored $spawned of $($Entries.Count) conversations."
    try {
        $socket.CloseOutputAsync([System.Net.WebSockets.WebSocketCloseStatus]::NormalClosure, 'done',
            [System.Threading.CancellationToken]::None).GetAwaiter().GetResult() | Out-Null
    } catch [System.Net.WebSockets.WebSocketException] {
        Write-Verbose "WebSocket close: $_"
    } finally {
        $socket.Dispose()
    }
}

function Read-State {
    <# Loads the saved snapshot. Assigning first avoids PowerShell 5.1 nesting the array. #>
    $parsed = Get-Content $StateFile -Raw | ConvertFrom-Json
    return , @($parsed)
}

function Invoke-Swap {
    <# Stops KISS-TMUX, swaps the exe if a new build exists, starts it, and restores. #>
    $entries = @()
    if (Test-Path $StateFile) { $entries = Read-State }

    Write-Host 'Stopping KISS-TMUX...'
    foreach ($kiss in @(Get-KissProcesses)) {
        Stop-Process -Id $kiss.ProcessId -Force -ErrorAction SilentlyContinue
        Wait-Process -Id $kiss.ProcessId -Timeout 10 -ErrorAction SilentlyContinue
    }
    foreach ($entry in $entries) {
        $leftover = Get-Process -Id $entry.pid -ErrorAction SilentlyContinue
        if ($leftover -and $leftover.ProcessName -eq 'claude') {
            Stop-Process -Id $entry.pid -Force -ErrorAction SilentlyContinue
        }
    }
    Get-Process -Name 'kiss-tmux-dev' -ErrorAction SilentlyContinue | Stop-Process -Force

    if (Test-Path $NewExe) {
        Write-Host 'Installing new build (old one kept as kiss-tmux.exe~)...'
        $installed = $false
        for ($attempt = 1; $attempt -le 10 -and -not $installed; $attempt++) {
            try {
                Copy-Item $ExePath "$ExePath~" -Force
                Move-Item $NewExe $ExePath -Force
                $installed = $true
            } catch [System.IO.IOException] {
                Start-Sleep -Seconds 1
            }
        }
        if (-not $installed) { throw "Couldn't replace $ExePath (still locked?)." }
    }

    Write-Host 'Starting KISS-TMUX...'
    Start-Process -FilePath $ExePath -ArgumentList "--port=$Port" `
        -WorkingDirectory (Split-Path -Parent $ExePath)
    Restore-Sessions $entries
}

switch ($Phase) {
    'snapshot' {
        $entries = @(Get-Snapshot)
        Write-Host "Found $($entries.Count) Claude conversation(s) in KISS-TMUX:"
        $entries | Format-Table name, status, cwd, cmd -AutoSize -Wrap | Out-String -Width 200 | Write-Host
        if (Test-Path $NewExe) { Write-Host "New build to install: $NewExe" }
        if ($DryRun) { Write-Host 'Dry run: nothing changed.'; return }

        $busy = @($entries | Where-Object { $_.status -eq 'busy' })
        if ($busy.Count -gt 0 -and -not $Force) {
            Write-Warning "Still working: $(($busy | ForEach-Object { $_.name }) -join ', ')."
            Write-Warning 'Restarting now interrupts their current step (the conversation itself is saved).'
            $answer = Read-Host 'Restart anyway? (y/N)'
            if ($answer -notmatch '^[yY]') { Write-Host 'Cancelled.'; return }
        }

        New-Item -ItemType Directory -Force -Path (Split-Path -Parent $StateFile) | Out-Null
        ConvertTo-Json -InputObject @($entries) -Depth 3 | Out-File -FilePath $StateFile -Encoding utf8
        Write-Host "Snapshot saved to $StateFile. Continuing in a new window..."

        $arguments = @(
            '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', "`"$PSCommandPath`"",
            '-Phase', 'swap', '-ExePath', "`"$ExePath`"", '-NewExe', "`"$NewExe`"",
            '-Port', $Port, '-StateFile', "`"$StateFile`""
        )
        Start-Process -FilePath 'powershell.exe' -ArgumentList $arguments
    }
    'swap' {
        try {
            Invoke-Swap
        } catch {
            Write-Host "Restart failed: $_" -ForegroundColor Red
            Write-Host "Snapshot is still in $StateFile; rerun with -Phase restore once KISS-TMUX is up."
        }
        Read-Host 'Press Enter to close'
    }
    'restore' {
        $entries = Read-State
        Restore-Sessions $entries
    }
}
