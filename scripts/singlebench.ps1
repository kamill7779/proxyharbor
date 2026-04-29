#!/usr/bin/env pwsh
param(
  [string]$BaseUrl = "http://localhost:18080",
  [string]$AdminKey = $env:PROXYHARBOR_ADMIN_KEY,
  [int]$Requests = 500,
  [int]$Concurrency = 32,
  [int]$Proxies = 16,
  [ValidateSet("mixed", "lease_create", "renew", "validate", "catalog")]
  [string]$Operation = "mixed",
  [ValidateSet("json", "csv")]
  [string]$Output = "json",
  [string]$Out = "",
  [switch]$SkipDocker,
  [switch]$AllowInternal
)

$ErrorActionPreference = "Stop"
$env:PROXYHARBOR_BASE_URL = $BaseUrl
$env:PROXYHARBOR_HOST_PORT = ([Uri]$BaseUrl).Port.ToString()
if ($env:PROXYHARBOR_HOST_PORT -eq "-1") { $env:PROXYHARBOR_HOST_PORT = "18080" }
$env:PROXYHARBOR_AUTH_REFRESH_INTERVAL = "1s"

$args = @(
  "run", "./tools/singlebench",
  "-base-url", $BaseUrl,
  "-requests", $Requests,
  "-concurrency", $Concurrency,
  "-proxies", $Proxies,
  "-operation", $Operation,
  "-output", $Output
)
if ($AdminKey -ne "") { $args += @("-admin-key", $AdminKey) }
if (-not $SkipDocker) { $args += "-docker" }
if ($AllowInternal) { $args += "-allow-internal-proxy-endpoint" }
if ($Out -ne "") { $args += @("-out", $Out) }

go @args
