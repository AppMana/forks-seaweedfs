# Select a process's tracerpt XML events and cross-thread IRP completions.
# This is a diagnostic view, not a replacement for the original ETL/XML.
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$InputXml,
    [Parameter(Mandatory)][ValidateRange(1,2147483647)][int]$TraceProcessId,
    [Parameter(Mandatory)][string]$OutputJsonLines
)
$ErrorActionPreference = 'Stop'
$stream = [IO.File]::Open($OutputJsonLines, [IO.FileMode]::CreateNew, [IO.FileAccess]::Write)
$writer = New-Object IO.StreamWriter($stream, (New-Object Text.UTF8Encoding($false)))
$reader = $null
$pending = @{}
$count = 0
try {
    $reader = [Xml.XmlReader]::Create($InputXml)
    while (!$reader.EOF) {
        if ($reader.NodeType -ne [Xml.XmlNodeType]::Element -or $reader.LocalName -ne 'Event') {
            if (!$reader.Read()) { break }
            continue
        }
        $raw = $reader.ReadOuterXml()
        $own = $raw.Contains('ProcessID="' + $TraceProcessId + '"')
        $completion = $raw.Contains('<Opcode>OperationEnd</Opcode>') -or $raw.Contains('<Opcode>OpEnd</Opcode>')
        if (!$own -and !$completion) { continue }
        $event = ([xml]$raw).Event
        $data = [ordered]@{}
        foreach ($value in $event.EventData.Data) { $data[[string]$value.Name] = [string]$value.InnerText }
        $irp = [string]$data['IrpPtr']
        $correlated = $completion -and $irp -and $pending.ContainsKey($irp)
        if (!$own -and !$correlated) { continue }
        if ($own -and $irp -and !$completion) { $pending[$irp] = $true }
        if ($completion -and $irp) { $pending.Remove($irp) }
        [ordered]@{
            time = [string]$event.System.TimeCreated.SystemTime
            process = [string]$event.System.Execution.ProcessID
            thread = [string]$event.System.Execution.ThreadID
            event = [string]$event.SelectSingleNode('*[local-name()="RenderingInfo"]/*[local-name()="EventName"]').InnerText
            operation = [string]$event.RenderingInfo.Opcode
            correlated_completion = [bool]$correlated
            data = $data
        } | ConvertTo-Json -Depth 5 -Compress | ForEach-Object { $writer.WriteLine($_) }
        $count++
    }
} finally {
    if ($reader) { $reader.Dispose() }
    $writer.Dispose()
}
Write-Output "Selected $count events; $($pending.Count) IRPs without a captured completion. Preserve the original trace and its loss summary."
