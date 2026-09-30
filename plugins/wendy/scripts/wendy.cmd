@echo off
rem Wendy CLI launcher for Windows. wendy-install.ps1 picks (and, if needed,
rem installs) wendy.exe the same way the POSIX launcher `wendy` next to this
rem file does, and prints its path; this script then runs it with the original
rem arguments so the CLI inherits stdin/stdout, which carry the MCP protocol,
rem and starts with the caller's original environment (endlocal).
rem PowerShell's stdin is NUL so it can't swallow the client's first message.
setlocal
set "WENDY_EXE="
for /f "usebackq delims=" %%P in (`powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "%~dp0wendy-install.ps1" ^<NUL`) do set "WENDY_EXE=%%P"
if not defined WENDY_EXE exit /b 1
endlocal & "%WENDY_EXE%" %*
exit /b %ERRORLEVEL%
