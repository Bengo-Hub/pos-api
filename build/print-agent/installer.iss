; Inno Setup script for the Codevertex POS Print Agent.
; Built by .github/workflows/print-agent-release.yml on a windows-latest runner:
;   go build -o build\print-agent\print-agent.exe .\cmd\print-agent
;   ISCC.exe build\print-agent\installer.iss
; Produces Output\CodevertexPrintAgentSetup.exe which installs the exe and registers + starts a
; Windows service (CodevertexPrintAgent) that auto-starts on boot and serves 127.0.0.1:9330.

#define AppVersion GetEnv("AGENT_VERSION")
#if AppVersion == ""
  #define AppVersion "1.0.1"
#endif

[Setup]
AppId={{B8F1B7E2-9C3A-4E1D-9F2B-CODEVERTEXPA01}
AppName=Codevertex POS Print Agent
AppVersion={#AppVersion}
AppPublisher=Codevertex Africa Limited
DefaultDirName={autopf}\Codevertex\PrintAgent
DefaultGroupName=Codevertex Print Agent
DisableProgramGroupPage=yes
OutputDir=Output
OutputBaseFilename=CodevertexPrintAgentSetup
Compression=lzma2
SolidCompression=yes
PrivilegesRequired=admin
ArchitecturesInstallIn64BitMode=x64
UninstallDisplayName=Codevertex POS Print Agent
WizardStyle=modern

[Files]
Source: "print-agent.exe"; DestDir: "{app}"; Flags: ignoreversion

[Run]
; Register + start the background service (auto-start on boot).
Filename: "{app}\print-agent.exe"; Parameters: "install"; Flags: runhidden waituntilterminated; StatusMsg: "Registering the print service..."
Filename: "{app}\print-agent.exe"; Parameters: "start"; Flags: runhidden waituntilterminated; StatusMsg: "Starting the print service..."

[UninstallRun]
; Stop + remove the service before deleting files.
Filename: "{app}\print-agent.exe"; Parameters: "stop"; Flags: runhidden waituntilterminated; RunOnceId: "StopPrintAgent"
Filename: "{app}\print-agent.exe"; Parameters: "uninstall"; Flags: runhidden waituntilterminated; RunOnceId: "RemovePrintAgent"

[Messages]
SetupWindowTitle=Codevertex POS Print Agent Setup
