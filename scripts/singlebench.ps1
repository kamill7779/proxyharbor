#!/usr/bin/env pwsh
param(
  [string]$BaseUrl = "http://localhost:18080",
  [string]$AdminKey = "dev-admin-key-min-32-chars-long!!!!",
  [string]$KeyPepper = "dev-key-pepper-min-32-bytes-random!!!!",
  [int]$Requests = 500,
  [int]$Concurrency = 32,
  [int]$Proxies = 16,
  [ValidateSet("mixed", "lease_create", "renew", "validate", "catalog")]
  [string]$Operation = "mixed",
  [ValidateSet("json", "csv")]
  [string]$Output = "json",
  [string]$Out = "",
  [switch]$SkipDocker
)

$ErrorActionPreference = "Stop"
$env:PROXYHARBOR_BASE_URL = $BaseUrl
$env:PROXYHARBOR_ADMIN_KEY = $AdminKey
$env:PROXYHARBOR_KEY_PEPPER = $KeyPepper
$env:PROXYHARBOR_HOST_PORT = ([Uri]$BaseUrl).Port.ToString()
if ($env:PROXYHARBOR_HOST_PORT -eq "-1") { $env:PROXYHARBOR_HOST_PORT = "18080" }
$env:PROXYHARBOR_ALLOW_INTERNAL_PROXY_ENDPOINT = "true"
$env:PROXYHARBOR_AUTH_REFRESH_INTERVAL = "1s"

$args = @(
  "run", "./tools/singlebench",
  "-base-url", $BaseUrl,
  "-admin-key", $AdminKey,
  "-requests", $Requests,
  "-concurrency", $Concurrency,
  "-proxies", $Proxies,
  "-operation", $Operation,
  "-output", $Output
)
if (-not $SkipDocker) { $args += "-docker" }
if ($Out -ne "") { $args += @("-out", $Out) }

go @args
