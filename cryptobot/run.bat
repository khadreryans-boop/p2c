@echo off
REM
set JAVA_HOME=%~dp0jre
set PATH=%JAVA_HOME%\bin;%PATH%

REM
java -jar "%~dp0app.jar"

pause
