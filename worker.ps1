# Ctrlit vSphere VM Stats - background helper (started and stopped by ctrlit-vSphere-VM-Stats.exe).
# Reads one JSON request per line on stdin and writes one reply per line, prefixed with "@@VH ".
#   {"op":"uia","x":..,"y":..}      -> names of the screen element under that point and its parents
#   {"op":"guest","key":..,"targets":[..],"user":..,"pass":..}
#                                   -> Windows guest CPU/memory/network and top 10 processes over WinRM
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'

$utf8 = New-Object System.Text.UTF8Encoding($false)
$reader = New-Object System.IO.StreamReader([Console]::OpenStandardInput(), $utf8)
$writer = New-Object System.IO.StreamWriter([Console]::OpenStandardOutput(), $utf8)
$writer.AutoFlush = $true

function Reply($obj) {
    $writer.WriteLine('@@VH ' + (ConvertTo-Json -InputObject $obj -Depth 6 -Compress))
}

$script:uiaReady = $false
function Initialize-Uia {
    if ($script:uiaReady) { return }
    # Screen coordinates from the app are physical pixels, so this process must be DPI aware too.
    try {
        Add-Type -Namespace VHover -Name Dpi -MemberDefinition @'
[System.Runtime.InteropServices.DllImport("user32.dll")] public static extern bool SetProcessDpiAwarenessContext(System.IntPtr value);
[System.Runtime.InteropServices.DllImport("user32.dll")] public static extern bool SetProcessDPIAware();
'@
        if (-not [VHover.Dpi]::SetProcessDpiAwarenessContext([IntPtr](-4))) { [void][VHover.Dpi]::SetProcessDPIAware() }
    } catch { }
    Add-Type -AssemblyName UIAutomationClient, UIAutomationTypes, WindowsBase
    $script:uiaReady = $true
}

function Get-UnderPoint($x, $y) {
    Initialize-Uia
    $pt = New-Object System.Windows.Point([double]$x, [double]$y)
    $el = [System.Windows.Automation.AutomationElement]::FromPoint($pt)
    $names = New-Object System.Collections.Generic.List[string]
    $procName = ''
    try { $procName = (Get-Process -Id $el.Current.ProcessId).ProcessName } catch { }
    $walker = [System.Windows.Automation.TreeWalker]::RawViewWalker
    for ($i = 0; $el -and $i -lt 8; $i++) {
        try {
            $ct = $el.Current.ControlType
            if ($ct -eq [System.Windows.Automation.ControlType]::Window -or $ct -eq [System.Windows.Automation.ControlType]::Document) { break }
            $n = $el.Current.Name
            if ($n) { $names.Add($n) }
        } catch { }
        try { $el = $walker.GetParent($el) } catch { $el = $null }
    }
    @{ ok = $true; names = [object[]]$names.ToArray(); process = $procName }
}

$script:sessions = @{}

function New-GuestSession($req) {
    $errs = @()
    foreach ($target in @($req.targets)) {
        $session = $null
        try {
            $p = @{ ComputerName = $target; OperationTimeoutSec = 8; ErrorAction = 'Stop' }
            if ($req.user) {
                $sec = ConvertTo-SecureString ([string]$req.pass) -AsPlainText -Force
                $p.Credential = New-Object System.Management.Automation.PSCredential([string]$req.user, $sec)
            }
            $session = New-CimSession @p
            $lp = (Get-CimInstance -CimSession $session -ClassName Win32_ComputerSystem -Property NumberOfLogicalProcessors -ErrorAction Stop).NumberOfLogicalProcessors
            # The first read of the "formatted" perf classes on a new connection often returns zeros - prime them.
            $null = Get-CimInstance -CimSession $session -ClassName Win32_PerfFormattedData_PerfOS_Processor -Property Name -ErrorAction SilentlyContinue
            $null = Get-CimInstance -CimSession $session -ClassName Win32_PerfFormattedData_PerfProc_Process -Property Name -ErrorAction SilentlyContinue
            Start-Sleep -Milliseconds 1000
            $entry = @{ Session = $session; Target = $target; LogicalProcs = [math]::Max(1, [int]$lp) }
            $script:sessions[$req.key] = $entry
            return $entry
        } catch {
            if ($session) { Remove-CimSession $session -ErrorAction SilentlyContinue }
            $errs += "$target - $($_.Exception.Message.Trim())"
        }
    }
    if ($errs.Count -eq 0) { throw 'VMware Tools reports no host name or IPv4 address for this VM.' }
    throw ($errs -join ' | ')
}

function Get-GuestSample($entry) {
    $q = @{ CimSession = $entry.Session; OperationTimeoutSec = 10; ErrorAction = 'Stop' }
    $cpu = Get-CimInstance @q -ClassName Win32_PerfFormattedData_PerfOS_Processor -Filter "Name='_Total'" -Property PercentProcessorTime
    $os = Get-CimInstance @q -ClassName Win32_OperatingSystem -Property TotalVisibleMemorySize, FreePhysicalMemory
    $nics = @(Get-CimInstance @q -ClassName Win32_PerfFormattedData_Tcpip_NetworkInterface -Property BytesTotalPersec)
    $procs = @(Get-CimInstance @q -ClassName Win32_PerfFormattedData_PerfProc_Process -Property Name, IDProcess, PercentProcessorTime, WorkingSetPrivate)

    $totalKB = [double]$os.TotalVisibleMemorySize
    $freeKB = [double]$os.FreePhysicalMemory          # "available" memory, as Task Manager counts it
    $netBytes = 0.0
    foreach ($n in $nics) { $netBytes += [double]$n.BytesTotalPersec }

    # Per-process CPU is per core (can exceed 100); divide by logical CPUs to match Task Manager.
    $lp = $entry.LogicalProcs
    $list = New-Object System.Collections.Generic.List[object]
    foreach ($p in $procs) {
        if ($p.Name -eq '_Total' -or $p.Name -eq 'Idle') { continue }
        $list.Add([pscustomobject]@{
            name  = ($p.Name -replace '#\d+$', '')
            pid   = [int]$p.IDProcess
            cpu   = [math]::Round([double]$p.PercentProcessorTime / $lp, 1)
            memMB = [math]::Round([double]$p.WorkingSetPrivate / 1MB, 1)
        })
    }
    $top = @($list | Sort-Object -Property @{ Expression = 'cpu'; Descending = $true }, @{ Expression = 'memMB'; Descending = $true } | Select-Object -First 10)

    @{
        ok           = $true
        target       = $entry.Target
        logicalProcs = $lp
        cpu          = [math]::Round([double]$cpu.PercentProcessorTime, 1)
        mem          = $(if ($totalKB -gt 0) { [math]::Round(($totalKB - $freeKB) / $totalKB * 100, 1) } else { 0 })
        net          = [math]::Round($netBytes / 1024, 1)
        totalMB      = [math]::Round($totalKB / 1024)
        usedMB       = [math]::Round(($totalKB - $freeKB) / 1024)
        processCount = $list.Count
        top          = [object[]]$top
    }
}

function Get-Guest($req) {
    $entry = $script:sessions[$req.key]
    if (-not $entry) { $entry = New-GuestSession $req }
    try {
        return Get-GuestSample $entry
    } catch {
        # Stale connection (VM rebooted, IP changed...): reconnect once.
        Remove-CimSession $entry.Session -ErrorAction SilentlyContinue
        $script:sessions.Remove($req.key)
        $entry = New-GuestSession $req
        return Get-GuestSample $entry
    }
}

Reply @{ ok = $true; ready = $true }
while ($true) {
    $line = $reader.ReadLine()
    if ($null -eq $line) { break }          # app closed our stdin
    if (-not $line.Trim()) { continue }
    try {
        $req = ConvertFrom-Json $line
        switch ($req.op) {
            'uia'   { $r = Get-UnderPoint $req.x $req.y }
            'guest' { $r = Get-Guest $req }
            'ping'  { $r = @{ ok = $true } }
            default { $r = @{ ok = $false; message = "unknown op $($req.op)" } }
        }
    } catch {
        $r = @{ ok = $false; message = $_.Exception.Message }
    }
    Reply $r
}
foreach ($e in $script:sessions.Values) { Remove-CimSession $e.Session -ErrorAction SilentlyContinue }
