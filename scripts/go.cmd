@echo off
rem Wrapper so Go commands always use workspace-local caches.
rem The DSH sandbox only permits writes under the session workspace.
rem The dot prefix matters: the go tool skips .*/_* directories in ./... patterns.
setlocal
for %%I in ("%~dp0..") do set "ROOT=%%~fI"
set "CACHE=%ROOT%\build\.go-cache"
if not exist "%CACHE%" mkdir "%CACHE%"
set "GOPATH=%CACHE%\gopath"
set "GOMODCACHE=%GOPATH%\pkg\mod"
set "GOCACHE=%CACHE%\build"
go %*
exit /b %errorlevel%
