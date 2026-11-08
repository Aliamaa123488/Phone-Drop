param(
    [Parameter(Mandatory = $true)][string]$Executable,
    [Parameter(Mandatory = $true)][string]$IconFile
)

$ErrorActionPreference = 'Stop'

if (-not ('PhoneDropIconResources' -as [type])) {
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;

public static class PhoneDropIconResources
{
    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, EntryPoint = "BeginUpdateResourceW", SetLastError = true)]
    public static extern IntPtr BeginUpdateResource(string fileName, [MarshalAs(UnmanagedType.Bool)] bool deleteExistingResources);

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, EntryPoint = "UpdateResourceW", SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool UpdateResource(IntPtr update, IntPtr type, IntPtr name, ushort language, byte[] data, uint dataSize);

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, EntryPoint = "EndUpdateResourceW", SetLastError = true)]
    [return: MarshalAs(UnmanagedType.Bool)]
    public static extern bool EndUpdateResource(IntPtr update, [MarshalAs(UnmanagedType.Bool)] bool discard);
}
'@
}

$exePath = [System.IO.Path]::GetFullPath($Executable)
$icoPath = [System.IO.Path]::GetFullPath($IconFile)
$ico = [System.IO.File]::ReadAllBytes($icoPath)
if ($ico.Length -lt 6 -or [BitConverter]::ToUInt16($ico, 0) -ne 0 -or [BitConverter]::ToUInt16($ico, 2) -ne 1) {
    throw 'The icon file is not a valid Windows ICO file.'
}

$count = [int][BitConverter]::ToUInt16($ico, 4)
if ($count -lt 1 -or $ico.Length -lt (6 + 16 * $count)) {
    throw 'The icon file has no complete image entries.'
}

$entries = @()
for ($i = 0; $i -lt $count; $i++) {
    $entryOffset = 6 + (16 * $i)
    $byteCount = [int][BitConverter]::ToUInt32($ico, $entryOffset + 8)
    $imageOffset = [int][BitConverter]::ToUInt32($ico, $entryOffset + 12)
    if ($byteCount -lt 1 -or $imageOffset -lt 0 -or ($imageOffset + $byteCount) -gt $ico.Length) {
        throw "Icon image entry $i points outside the ICO file."
    }
    $imageData = [byte[]]::new($byteCount)
    [Array]::Copy($ico, $imageOffset, $imageData, 0, $byteCount)
    $entries += [pscustomobject]@{
        Directory = [byte[]]$ico[$entryOffset..($entryOffset + 15)]
        ImageData = $imageData
        ResourceId = [uint16]($i + 1)
    }
}

$groupStream = [System.IO.MemoryStream]::new()
$groupWriter = [System.IO.BinaryWriter]::new($groupStream)
try {
    $groupWriter.Write([uint16]0)
    $groupWriter.Write([uint16]1)
    $groupWriter.Write([uint16]$count)
    foreach ($entry in $entries) {
        $directory = $entry.Directory
        $groupWriter.Write([byte[]]$directory[0..7])
        $groupWriter.Write([uint32]$entry.ImageData.Length)
        $groupWriter.Write([uint16]$entry.ResourceId)
    }
    $groupData = $groupStream.ToArray()
} finally {
    $groupWriter.Dispose()
    $groupStream.Dispose()
}

$update = [PhoneDropIconResources]::BeginUpdateResource($exePath, $false)
if ($update -eq [IntPtr]::Zero) {
    throw [System.ComponentModel.Win32Exception]::new([Runtime.InteropServices.Marshal]::GetLastWin32Error())
}

try {
    foreach ($entry in $entries) {
        if (-not [PhoneDropIconResources]::UpdateResource($update, [IntPtr]3, [IntPtr]$entry.ResourceId, 0, $entry.ImageData, [uint32]$entry.ImageData.Length)) {
            throw [System.ComponentModel.Win32Exception]::new([Runtime.InteropServices.Marshal]::GetLastWin32Error())
        }
    }
    if (-not [PhoneDropIconResources]::UpdateResource($update, [IntPtr]14, [IntPtr]1, 0, $groupData, [uint32]$groupData.Length)) {
        throw [System.ComponentModel.Win32Exception]::new([Runtime.InteropServices.Marshal]::GetLastWin32Error())
    }
    if (-not [PhoneDropIconResources]::EndUpdateResource($update, $false)) {
        throw [System.ComponentModel.Win32Exception]::new([Runtime.InteropServices.Marshal]::GetLastWin32Error())
    }
} catch {
    [PhoneDropIconResources]::EndUpdateResource($update, $true) | Out-Null
    throw
}

Write-Host "Embedded PhoneDrop.ico into $exePath"
