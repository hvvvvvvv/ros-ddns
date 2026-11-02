# Wrapper so Go commands always use workspace-local caches.
# The DSH sandbox only permits writes under the session workspace; the default
# GOPATH/GOMODCACHE live outside it, so redirect them into build/.go-cache.
# The dot prefix matters: the go tool skips .*/_* directories in ./... patterns,
# so the module cache sources are not mistaken for packages of this module.
$root = Split-Path -Parent $PSScriptRoot
$cache = Join-Path $root 'build\.go-cache'
New-Item -ItemType Directory -Force $cache | Out-Null
$env:GOPATH = Join-Path $cache 'gopath'
$env:GOMODCACHE = Join-Path $env:GOPATH 'pkg\mod'
$env:GOCACHE = Join-Path $cache 'build'
& go @args
exit $LASTEXITCODE
